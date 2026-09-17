"""Reconcile the per-version worker objects of every WorkerVersionSet.

A DBOS application runs one worker Deployment per application version, so a
version that is being replaced can still finish its in-flight workflows: DBOS
recovers a workflow only onto an executor reporting the same application
version. A Git-rendered chart cannot hold that list of versions, because the
list changes with the workload, not with the commit. Here Git holds one
``WorkerVersionSet`` custom resource per application and this controller owns
the per-version objects: it applies the desired version's Deployment,
ScaledObject and PDB, puts every other version of that set into draining, and
deletes a drained version once it has held zero outstanding workflows and zero
pods for the grace period. It reports what it saw on the set's status.

The controller never launches DBOS. It reads ``dbos.workflow_status`` with
psycopg, which keeps a crashing worker version from taking the controller with
it.
"""

import re
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Literal

import yaml
from kubernetes import client as k8s_client
from kubernetes import config as k8s_config
from kubernetes.client.rest import ApiException

from dbos_version_controller._log import get_logger
from dbos_version_controller.settings import ControllerSettings

logger = get_logger(__name__)

Kind = Literal["deployment", "scaledobject", "pdb"]

KINDS: tuple[Kind, ...] = ("deployment", "scaledobject", "pdb")
TEMPLATE_KEYS: dict[Kind, str] = {
    "deployment": "deployment",
    "scaledobject": "scaledObject",
    "pdb": "podDisruptionBudget",
}

KEDA_GROUP = "keda.sh"
KEDA_VERSION = "v1alpha1"
KEDA_PLURAL = "scaledobjects"

CRD_GROUP = "dbos.trustle.com"
CRD_VERSION = "v1alpha1"
CRD_PLURAL = "workerversionsets"
CRD_KIND = "WorkerVersionSet"

DELETE_ORDER: tuple[Kind, ...] = ("scaledobject", "pdb", "deployment")

MANAGED_BY_LABEL = "app.kubernetes.io/managed-by"
SET_LABEL = "dbos.trustle.com/worker-version-set"
DRAINED_SINCE_ANNOTATION = "dbos-version-controller/drained-since"

VERSION_PATTERN = re.compile(r"^[a-z0-9][a-z0-9.-]{0,48}$")
IMAGE_PATTERN = re.compile(r"^[A-Za-z0-9./:_@-]+$")

OUTSTANDING_SQL = (
    "SELECT count(*) FROM dbos.workflow_status WHERE application_version = %s AND status IN ('ENQUEUED','PENDING')"
)


def outstanding_sql_for(version: str) -> str:
    """Return the outstanding-workflow query with the version inlined, for a KEDA trigger.

    KEDA carries no parameter binding, so the version is substituted literally.
    Every caller validates it against ``VERSION_PATTERN`` first, which admits no
    quote character.
    """
    return (
        "SELECT count(*) FROM dbos.workflow_status "
        f"WHERE application_version = '{version}' AND status IN ('ENQUEUED','PENDING')"
    )


@dataclass(frozen=True)
class ManagedDeployment:
    """One worker Deployment this controller owns."""

    name: str
    version: str
    replicas: int
    annotations: dict[str, str]
    image: str | None = None


class KubeClient:
    """Namespaced access to the object kinds the controller owns.

    Patches are sent as JSON merge patches, so that a patched list — the KEDA
    ``spec.triggers`` — is replaced wholesale rather than merged element by
    element. The generated client picks ``application/json-patch+json`` for every
    patch method, so the merge content type is forced through a second
    ``ApiClient`` whose default header wins over the generated one.
    """

    def __init__(
        self,
        namespace: str,
        dry_run: bool = False,
        api_client: Any = None,
        patch_api_client: Any = None,
    ) -> None:
        if api_client is None:
            _load_kube_config()
            api_client = k8s_client.ApiClient()
        if patch_api_client is None:
            patch_api_client = k8s_client.ApiClient(api_client.configuration)
            patch_api_client.set_default_header("Content-Type", "application/merge-patch+json")
        self._namespace = namespace
        self._dry_run = dry_run
        self._core = k8s_client.CoreV1Api(api_client)
        self._apps = k8s_client.AppsV1Api(api_client)
        self._policy = k8s_client.PolicyV1Api(api_client)
        self._custom = k8s_client.CustomObjectsApi(api_client)
        self._apps_patch = k8s_client.AppsV1Api(patch_api_client)
        self._policy_patch = k8s_client.PolicyV1Api(patch_api_client)
        self._custom_patch = k8s_client.CustomObjectsApi(patch_api_client)

    def list_worker_version_sets(self) -> list[dict[str, Any]]:
        response: Any = self._custom.list_namespaced_custom_object(
            group=CRD_GROUP,
            version=CRD_VERSION,
            namespace=self._namespace,
            plural=CRD_PLURAL,
        )
        return list(response.get("items") or [])

    def patch_status(self, name: str, status: dict[str, Any]) -> None:
        """Patch one set's status subresource.

        The status is an observation, not an action, so it is written in dry-run
        mode too: an operator trying the controller out still sees what it found.
        """
        self._custom_patch.patch_namespaced_custom_object_status(
            group=CRD_GROUP,
            version=CRD_VERSION,
            namespace=self._namespace,
            plural=CRD_PLURAL,
            name=name,
            body={"status": status},
        )

    def mark(self, name: str, body: dict[str, Any]) -> None:
        """Patch the drained-since marker annotation on one Deployment.

        Like the status subresource, this records what the controller observed
        rather than acting on the cluster, so it is written in dry-run mode too.
        Without it, the grace clock never advances under ``DVC_DRY_RUN`` and a
        dry-run tick past grace never logs ``Would delete``.
        """
        self._apps_patch.patch_namespaced_deployment(name=name, namespace=self._namespace, body=body)

    def list_managed_deployments(self, managed_by: str, version_label: str, set_name: str) -> list[ManagedDeployment]:
        response: Any = self._apps.list_namespaced_deployment(
            namespace=self._namespace,
            label_selector=f"{MANAGED_BY_LABEL}={managed_by},{SET_LABEL}={set_name}",
        )
        managed: list[ManagedDeployment] = []
        for item in response.items:
            metadata = item.metadata
            labels = metadata.labels or {}
            status = item.status
            containers = getattr(item.spec.template.spec, "containers", None) or []
            managed.append(
                ManagedDeployment(
                    name=metadata.name,
                    version=labels.get(version_label, ""),
                    replicas=(status.replicas if status and status.replicas else 0),
                    annotations=dict(metadata.annotations or {}),
                    image=(containers[0].image if containers else None),
                )
            )
        return managed

    def exists(self, kind: Kind, name: str) -> bool:
        try:
            if kind == "deployment":
                self._apps.read_namespaced_deployment(name=name, namespace=self._namespace)
            elif kind == "pdb":
                self._policy.read_namespaced_pod_disruption_budget(name=name, namespace=self._namespace)
            else:
                self._custom.get_namespaced_custom_object(
                    group=KEDA_GROUP,
                    version=KEDA_VERSION,
                    namespace=self._namespace,
                    plural=KEDA_PLURAL,
                    name=name,
                )
        except ApiException as exc:
            if exc.status == 404:
                return False
            raise
        return True

    def create(self, kind: Kind, manifest: dict[str, Any]) -> None:
        name = manifest["metadata"]["name"]
        if self._dry_run:
            logger.info("Would create worker object", kind=kind, name=name, namespace=self._namespace)
            return
        if kind == "deployment":
            self._apps.create_namespaced_deployment(namespace=self._namespace, body=manifest)
        elif kind == "pdb":
            self._policy.create_namespaced_pod_disruption_budget(namespace=self._namespace, body=manifest)
        else:
            self._custom.create_namespaced_custom_object(
                group=KEDA_GROUP,
                version=KEDA_VERSION,
                namespace=self._namespace,
                plural=KEDA_PLURAL,
                body=manifest,
            )

    def patch(self, kind: Kind, name: str, body: dict[str, Any]) -> None:
        if self._dry_run:
            logger.info("Would patch worker object", kind=kind, name=name, namespace=self._namespace)
            return
        if kind == "deployment":
            self._apps_patch.patch_namespaced_deployment(name=name, namespace=self._namespace, body=body)
        elif kind == "pdb":
            self._policy_patch.patch_namespaced_pod_disruption_budget(name=name, namespace=self._namespace, body=body)
        else:
            self._custom_patch.patch_namespaced_custom_object(
                group=KEDA_GROUP,
                version=KEDA_VERSION,
                namespace=self._namespace,
                plural=KEDA_PLURAL,
                name=name,
                body=body,
            )

    def delete(self, kind: Kind, name: str) -> None:
        if self._dry_run:
            logger.info("Would delete worker object", kind=kind, name=name, namespace=self._namespace)
            return
        try:
            if kind == "deployment":
                self._apps.delete_namespaced_deployment(name=name, namespace=self._namespace)
            elif kind == "pdb":
                self._policy.delete_namespaced_pod_disruption_budget(name=name, namespace=self._namespace)
            else:
                self._custom.delete_namespaced_custom_object(
                    group=KEDA_GROUP,
                    version=KEDA_VERSION,
                    namespace=self._namespace,
                    plural=KEDA_PLURAL,
                    name=name,
                )
        except ApiException as exc:
            if exc.status != 404:
                raise


def _load_kube_config() -> None:
    """Load in-cluster credentials, falling back to a kubeconfig for local runs."""
    try:
        k8s_config.load_incluster_config()
    except k8s_config.ConfigException:
        k8s_config.load_kube_config()


def render(template: str, version: str, image: str) -> dict[str, Any]:
    """Substitute the two placeholders and parse the result as a manifest."""
    rendered = template.replace("__VERSION__", version).replace("__IMAGE__", image)
    return yaml.safe_load(rendered)


def drain_patch(template_scaled_object: dict[str, Any], version: str) -> dict[str, Any]:
    """Build the ScaledObject patch that parks a draining version.

    The version keeps exactly one trigger: the outstanding-workflow count for its
    own version, targeting one workflow per pod. Together with
    ``minReplicaCount: 0`` that holds the version at zero pods until work of its
    own version appears, and returns it to zero when that work finishes.
    """
    trigger = template_scaled_object["spec"]["triggers"][0]
    metadata = {
        key: value for key, value in (trigger.get("metadata") or {}).items() if key not in ("query", "targetQueryValue")
    }
    metadata["query"] = outstanding_sql_for(version)
    metadata["targetQueryValue"] = "1"
    drained: dict[str, Any] = {
        "type": trigger["type"],
        "metricType": "AverageValue",
        "metadata": metadata,
    }
    if trigger.get("authenticationRef") is not None:
        drained["authenticationRef"] = trigger["authenticationRef"]
    return {"spec": {"minReplicaCount": 0, "triggers": [drained]}}


def _merge_patch(manifest: dict[str, Any], patch: dict[str, Any]) -> dict[str, Any]:
    """Apply a JSON merge patch (RFC 7396) onto a manifest, returning a new dict."""
    result = dict(manifest)
    for key, value in patch.items():
        if value is None:
            result.pop(key, None)
        elif isinstance(value, dict) and isinstance(result.get(key), dict):
            result[key] = _merge_patch(result[key], value)
        else:
            result[key] = value
    return result


def _parse_drained_since(value: str) -> datetime | None:
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def _timestamp(moment: datetime) -> str:
    """Render one RFC 3339 instant the way Kubernetes writes its own timestamps."""
    return moment.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _own(manifest: dict[str, Any], set_name: str) -> dict[str, Any]:
    """Stamp the set label onto one rendered manifest.

    Ownership is label-based, not an ownerReference: deleting and re-applying
    the WorkerVersionSet (Argo prune/recreate, ``kubectl replace --force``)
    must not garbage-collect a draining Deployment with work in flight.
    Deletion stays the controller's own decision. A set re-created with the
    same name adopts its existing objects by label.
    """
    metadata = manifest.setdefault("metadata", {})
    labels = metadata.get("labels") or {}
    labels[SET_LABEL] = set_name
    metadata["labels"] = labels
    return manifest


def spec_problem(version: str, image: str, template: dict[str, str]) -> str | None:
    """Return why a set's spec is unusable, or ``None`` when it is usable."""
    if not VERSION_PATTERN.match(version):
        return f"spec.version {version!r} does not match {VERSION_PATTERN.pattern}"
    if not IMAGE_PATTERN.match(image):
        return f"spec.image {image!r} does not match {IMAGE_PATTERN.pattern}"
    missing = [TEMPLATE_KEYS[kind] for kind in KINDS if not (template.get(TEMPLATE_KEYS[kind]) or "").strip()]
    if missing:
        return f"spec.template is missing {', '.join(missing)}"
    return None


def _apply_desired(
    kube: KubeClient,
    template: dict[str, str],
    version: str,
    image: str,
    set_name: str,
    existing_deployment: ManagedDeployment | None,
) -> dict[str, Any]:
    """Create or patch the three objects of the desired version; return the rendered ScaledObject.

    Clearing the drained-since marker on an existing Deployment goes through
    ``kube.mark``, not ``kube.patch``: ``mark`` bypasses dry run the same way
    ``_drain`` writes the marker, so a rollback in dry run does not leave a
    stale marker that the next drain would read as already past grace.

    ``mark`` is called only when ``existing_deployment`` still carries the
    marker: at steady state the desired version never drained, so there is
    nothing to clear and calling ``mark`` every tick would be a no-op write
    (and a dry-run write when nothing was observed).
    """
    rendered: dict[Kind, dict[str, Any]] = {}
    for kind in KINDS:
        manifest = _own(render(template[TEMPLATE_KEYS[kind]], version, image), set_name)
        rendered[kind] = manifest
        name = manifest["metadata"]["name"]
        if kube.exists(kind, name):
            kube.patch(kind, name, manifest)
            created = False
            if (
                kind == "deployment"
                and existing_deployment
                and existing_deployment.annotations.get(DRAINED_SINCE_ANNOTATION)
            ):
                kube.mark(name, {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}})
        else:
            kube.create(kind, manifest)
            created = True
        logger.info("apply", kind=kind, name=name, version=version, image=image, created=created)
    return rendered["scaledobject"]


def _drain(
    kube: KubeClient,
    deployment: ManagedDeployment,
    template_scaled_object: dict[str, Any],
    scaledobject_template: str,
    rows: int | None,
    now: datetime,
    grace_seconds: float,
    set_name: str,
) -> dict[str, Any]:
    """Park one non-desired version, and delete it once it has been idle for the grace period.

    ``rows is None`` means the outstanding count is unknown. An unknown count
    never deletes: a version with work we cannot see must keep its objects.

    The ScaledObject can be missing — deleted out of band, or never created
    because a prior tick failed before it patched. Re-render it from the raw
    template for this version and merge the drain patch over it, rather than
    patching an object that does not exist and leaving the Deployment with no
    KEDA trigger.

    Returns the status entry describing what this version looks like now.
    """
    entry = version_status(deployment.version, "Draining", deployment.image, deployment.replicas, rows)

    patch = drain_patch(template_scaled_object, deployment.version)
    if kube.exists("scaledobject", deployment.name):
        kube.patch("scaledobject", deployment.name, patch)
    else:
        manifest = render(scaledobject_template, deployment.version, image="unused")
        kube.create("scaledobject", _own(_merge_patch(manifest, patch), set_name))
    logger.info("drain", name=deployment.name, version=deployment.version, rows=rows, pods=deployment.replicas)

    stamp = deployment.annotations.get(DRAINED_SINCE_ANNOTATION)
    if rows is None:
        return entry

    if rows > 0 or deployment.replicas > 0:
        if stamp:
            kube.mark(deployment.name, {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}})
            logger.info("Worker version is busy again; cleared drained marker", name=deployment.name)
        return entry

    drained_since = _parse_drained_since(stamp) if stamp else None
    if drained_since is None:
        kube.mark(
            deployment.name,
            {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: _timestamp(now)}}},
        )
        logger.info("Worker version reached zero; marked drained", name=deployment.name, version=deployment.version)
        entry["phase"] = "Drained"
        entry["drainedSince"] = _timestamp(now)
        return entry

    entry["phase"] = "Drained"
    entry["drainedSince"] = _timestamp(drained_since)
    if (now - drained_since).total_seconds() < grace_seconds:
        return entry

    for kind in DELETE_ORDER:
        kube.delete(kind, deployment.name)
    logger.info("Deleted drained worker version", name=deployment.name, version=deployment.version)
    return entry


def version_status(
    version: str,
    phase: str,
    image: str | None,
    replicas: int,
    rows: int | None,
) -> dict[str, Any]:
    """Build one entry of ``status.versions``, leaving unknown fields out."""
    entry: dict[str, Any] = {"version": version, "phase": phase, "replicas": replicas}
    if image:
        entry["image"] = image
    if rows is not None:
        entry["outstanding"] = rows
    return entry


def _status(
    item: dict[str, Any],
    now: datetime,
    versions: list[dict[str, Any]],
    ready: str,
    reason: str,
    message: str,
) -> dict[str, Any]:
    """Build the status block, keeping the Ready transition time when nothing changed."""
    previous = (item.get("status") or {}).get("conditions") or []
    prior = next((condition for condition in previous if condition.get("type") == "Ready"), None)
    unchanged = prior is not None and prior.get("status") == ready and prior.get("reason") == reason
    transition = prior.get("lastTransitionTime") if unchanged and prior else None
    return {
        "observedGeneration": (item.get("metadata") or {}).get("generation", 0),
        "lastReconcileTime": _timestamp(now),
        "versions": versions,
        "conditions": [
            {
                "type": "Ready",
                "status": ready,
                "reason": reason,
                "message": message,
                "lastTransitionTime": transition or _timestamp(now),
            }
        ],
    }


def _report(
    kube: KubeClient,
    item: dict[str, Any],
    now: datetime,
    versions: list[dict[str, Any]],
    ready: str,
    reason: str,
    message: str,
) -> None:
    name = (item.get("metadata") or {}).get("name", "")
    try:
        kube.patch_status(name, _status(item, now, versions, ready, reason, message))
    except Exception:
        logger.warning("Failed to patch WorkerVersionSet status", name=name, exc_info=True)


def reconcile_set(
    kube: KubeClient,
    settings: ControllerSettings,
    outstanding: Callable[[str], int],
    item: dict[str, Any],
    now: datetime,
) -> None:
    """Reconcile one WorkerVersionSet and patch its status.

    An unusable spec skips every write for this set: the controller deletes
    nothing while it cannot tell which version is desired.
    """
    metadata = item.get("metadata") or {}
    spec = item.get("spec") or {}
    set_name = metadata["name"]
    version = str(spec.get("version") or "").strip()
    image = str(spec.get("image") or "").strip()
    template: dict[str, str] = spec.get("template") or {}

    problem = spec_problem(version, image, template)
    if problem:
        logger.error("WorkerVersionSet spec is unusable; skipping it", name=set_name, problem=problem)
        _report(kube, item, now, [], "False", "InvalidSpec", problem)
        return

    grace_seconds = float(spec["graceSeconds"]) if spec.get("graceSeconds") is not None else settings.grace_seconds
    managed = kube.list_managed_deployments(settings.managed_by, settings.version_label, set_name)
    existing_deployment = next((deployment for deployment in managed if deployment.version == version), None)
    template_scaled_object = _apply_desired(kube, template, version, image, set_name, existing_deployment)

    db_available = True
    failed_drains: list[str] = []

    def count(target_version: str) -> int | None:
        nonlocal db_available
        if not db_available:
            return None
        try:
            return outstanding(target_version)
        except Exception:
            db_available = False
            logger.warning("Cannot count outstanding workflows; deleting nothing this tick", exc_info=True)
            return None

    current_rows = count(version)
    desired = version_status(version, "Current", image, 0, current_rows)
    draining: list[dict[str, Any]] = []
    for deployment in managed:
        if deployment.version == version:
            desired = version_status(version, "Current", deployment.image or image, deployment.replicas, current_rows)
            continue
        if not VERSION_PATTERN.match(deployment.version):
            logger.warning("Managed worker carries an unusable version label; leaving it alone", name=deployment.name)
            continue

        rows = count(deployment.version)

        try:
            draining.append(
                _drain(
                    kube,
                    deployment,
                    template_scaled_object,
                    template[TEMPLATE_KEYS["scaledobject"]],
                    rows,
                    now,
                    grace_seconds,
                    set_name,
                )
            )
        except Exception:
            logger.warning(
                "Failed to drain worker version; continuing with the remaining versions",
                name=deployment.name,
                version=deployment.version,
                exc_info=True,
            )
            failed_drains.append(deployment.version)
            draining.append(version_status(deployment.version, "Draining", deployment.image, deployment.replicas, rows))

    # DatabaseUnavailable takes precedence over DrainFailed: an unreachable database
    # explains every drain failure that follows from it, and is the more actionable cause.
    if not db_available:
        _report(
            kube,
            item,
            now,
            [desired, *draining],
            "False",
            "DatabaseUnavailable",
            "Cannot count outstanding workflows; deleted nothing this tick",
        )
    elif failed_drains:
        _report(
            kube,
            item,
            now,
            [desired, *draining],
            "False",
            "DrainFailed",
            f"Failed to drain version(s): {', '.join(failed_drains)}",
        )
    else:
        _report(kube, item, now, [desired, *draining], "True", "Reconciled", "Reconciled the worker versions")


def reconcile(
    kube: KubeClient,
    settings: ControllerSettings,
    outstanding: Callable[[str], int],
    now: Callable[[], datetime],
) -> None:
    """Run one reconcile tick over every WorkerVersionSet in the namespace.

    Each set is reconciled on its own. A set that fails is reported on its own
    status and does not stop the sets after it.
    """
    tick_now = now()
    for item in kube.list_worker_version_sets():
        name = (item.get("metadata") or {}).get("name")
        if not name:
            continue
        try:
            reconcile_set(kube, settings, outstanding, item, tick_now)
        except Exception as exc:
            logger.exception("Failed to reconcile WorkerVersionSet", name=name)
            _report(kube, item, tick_now, [], "False", "ReconcileError", str(exc) or exc.__class__.__name__)
