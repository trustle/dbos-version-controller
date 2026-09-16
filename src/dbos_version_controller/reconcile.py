"""Reconcile the per-version worker objects against one desired version.

A DBOS application runs one worker Deployment per application version, so a
version that is being replaced can still finish its in-flight workflows: DBOS
recovers a workflow only onto an executor reporting the same application
version. A Git-rendered chart cannot hold that list of versions, because the
list changes with the workload, not with the commit. Here Git holds one desired
``(version, image)`` pair and this controller owns the per-version objects: it
applies the desired version's Deployment, ScaledObject and PDB, puts every other
managed version into draining, and deletes a drained version once it has held
zero outstanding workflows and zero pods for the grace period.

The controller never launches DBOS. It reads ``dbos.workflow_status`` with
psycopg, which keeps a crashing worker version from taking the controller with
it.
"""

import copy
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
    "deployment": "deployment.yaml",
    "scaledobject": "scaledobject.yaml",
    "pdb": "pdb.yaml",
}

KEDA_GROUP = "keda.sh"
KEDA_VERSION = "v1alpha1"
KEDA_PLURAL = "scaledobjects"

DELETE_ORDER: tuple[Kind, ...] = ("scaledobject", "pdb", "deployment")

MANAGED_BY_LABEL = "app.kubernetes.io/managed-by"
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

    def read_configmap(self, name: str) -> dict[str, str]:
        config_map: Any = self._core.read_namespaced_config_map(name=name, namespace=self._namespace)
        return dict(config_map.data or {})

    def list_managed_deployments(self, managed_by: str, version_label: str) -> list[ManagedDeployment]:
        response: Any = self._apps.list_namespaced_deployment(
            namespace=self._namespace,
            label_selector=f"{MANAGED_BY_LABEL}={managed_by}",
        )
        managed: list[ManagedDeployment] = []
        for item in response.items:
            metadata = item.metadata
            labels = metadata.labels or {}
            status = item.status
            managed.append(
                ManagedDeployment(
                    name=metadata.name,
                    version=labels.get(version_label, ""),
                    replicas=(status.replicas if status and status.replicas else 0),
                    annotations=dict(metadata.annotations or {}),
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


def _apply_desired(kube: KubeClient, template: dict[str, str], version: str, image: str) -> dict[str, Any]:
    """Create or patch the three objects of the desired version; return the rendered ScaledObject."""
    rendered: dict[Kind, dict[str, Any]] = {}
    for kind in KINDS:
        manifest = render(template[TEMPLATE_KEYS[kind]], version, image)
        rendered[kind] = manifest
        name = manifest["metadata"]["name"]
        if kube.exists(kind, name):
            patch_body = manifest
            if kind == "deployment":
                patch_body = copy.deepcopy(manifest)
                annotations = patch_body["metadata"].get("annotations") or {}
                patch_body["metadata"]["annotations"] = {**annotations, DRAINED_SINCE_ANNOTATION: None}
            kube.patch(kind, name, patch_body)
            created = False
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
) -> None:
    """Park one non-desired version, and delete it once it has been idle for the grace period.

    ``rows is None`` means the outstanding count is unknown. An unknown count
    never deletes: a version with work we cannot see must keep its objects.

    The ScaledObject can be missing — deleted out of band, or never created
    because a prior tick failed before it patched. Re-render it from the raw
    template for this version and merge the drain patch over it, rather than
    patching an object that does not exist and leaving the Deployment with no
    KEDA trigger.
    """
    patch = drain_patch(template_scaled_object, deployment.version)
    if kube.exists("scaledobject", deployment.name):
        kube.patch("scaledobject", deployment.name, patch)
    else:
        manifest = render(scaledobject_template, deployment.version, image="unused")
        kube.create("scaledobject", _merge_patch(manifest, patch))
    logger.info("drain", name=deployment.name, version=deployment.version, rows=rows, pods=deployment.replicas)

    stamp = deployment.annotations.get(DRAINED_SINCE_ANNOTATION)
    if rows is None:
        return

    if rows > 0 or deployment.replicas > 0:
        if stamp:
            kube.patch("deployment", deployment.name, {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}})
            logger.info("Worker version is busy again; cleared drained marker", name=deployment.name)
        return

    drained_since = _parse_drained_since(stamp) if stamp else None
    if drained_since is None:
        kube.patch(
            "deployment",
            deployment.name,
            {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: now.isoformat()}}},
        )
        logger.info("Worker version reached zero; marked drained", name=deployment.name, version=deployment.version)
        return

    if (now - drained_since).total_seconds() < grace_seconds:
        return

    for kind in DELETE_ORDER:
        kube.delete(kind, deployment.name)
    logger.info("Deleted drained worker version", name=deployment.name, version=deployment.version)


def reconcile(
    kube: KubeClient,
    settings: ControllerSettings,
    outstanding: Callable[[str], int],
    now: Callable[[], datetime],
) -> None:
    """Run one reconcile tick.

    A bad desired version skips the whole tick: the controller deletes nothing
    while it cannot tell which version is desired.
    """
    desired = kube.read_configmap(settings.desired_configmap)
    version = (desired.get("version") or "").strip()
    image = (desired.get("image") or "").strip()
    if not VERSION_PATTERN.match(version) or not IMAGE_PATTERN.match(image):
        logger.error("Desired worker version is invalid; skipping tick", version=version, image=image)
        return

    template = kube.read_configmap(settings.template_configmap)
    template_scaled_object = _apply_desired(kube, template, version, image)

    tick_now = now()
    db_available = True
    for deployment in kube.list_managed_deployments(settings.managed_by, settings.version_label):
        if deployment.version == version:
            continue
        if not VERSION_PATTERN.match(deployment.version):
            logger.warning("Managed worker carries an unusable version label; leaving it alone", name=deployment.name)
            continue

        rows: int | None = None
        if db_available:
            try:
                rows = outstanding(deployment.version)
            except Exception:
                db_available = False
                logger.warning("Cannot count outstanding workflows; deleting nothing this tick", exc_info=True)

        try:
            _drain(
                kube,
                deployment,
                template_scaled_object,
                template[TEMPLATE_KEYS["scaledobject"]],
                rows,
                tick_now,
                settings.grace_seconds,
            )
        except Exception:
            logger.warning(
                "Failed to drain worker version; continuing with the remaining versions",
                name=deployment.name,
                version=deployment.version,
                exc_info=True,
            )
