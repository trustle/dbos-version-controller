from datetime import UTC, datetime, timedelta
from unittest.mock import MagicMock

import pytest
from kubernetes import client as k8s_client

from dbos_version_controller.reconcile import (
    CRD_GROUP,
    CRD_KIND,
    CRD_VERSION,
    DRAINED_SINCE_ANNOTATION,
    SET_LABEL,
    KubeClient,
    ManagedDeployment,
    reconcile,
)
from dbos_version_controller.reconcile import (
    logger as reconcile_logger,
)
from dbos_version_controller.settings import ControllerSettings

DESIRED_VERSION = "v20260916135248"
DESIRED_IMAGE = "registry.example.com/app:8332cd5"
OLD_VERSION = "v20260901120000"

NAMESPACE = "dbos-workers"
DATABASE_URL = "postgresql://user:pass@localhost:5432/dbos"
SET_NAME = "dbos-worker"

NOW = datetime(2026, 9, 16, 14, 0, 0, tzinfo=UTC)
NOW_STAMP = "2026-09-16T14:00:00Z"

DEPLOYMENT_TEMPLATE = """
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  template:
    spec:
      containers:
        - name: app
          image: __IMAGE__
          env:
            - name: DBOS_APPLICATION_VERSION
              value: __VERSION__
"""

SCALED_OBJECT_TEMPLATE = """
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  scaleTargetRef:
    name: dbos-worker-__VERSION__
  minReplicaCount: 1
  triggers:
    - type: postgresql
      metricType: AverageValue
      authenticationRef:
        name: dbos-postgres-auth
      metadata:
        query: "SELECT 1 FROM dbos.workflow_status WHERE application_version = '__VERSION__'"
        targetQueryValue: "4"
    - type: postgresql
      metricType: AverageValue
      authenticationRef:
        name: dbos-postgres-auth
      metadata:
        query: "SELECT 2 FROM dbos.workflow_status WHERE application_version = '__VERSION__'"
        targetQueryValue: "2"
"""

PDB_TEMPLATE = """
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: dbos-worker-__VERSION__
  labels:
    dbos-app-version: __VERSION__
    app.kubernetes.io/managed-by: dbos-version-controller
spec:
  maxUnavailable: 1
"""

TEMPLATE = {
    "deployment": DEPLOYMENT_TEMPLATE,
    "scaledObject": SCALED_OBJECT_TEMPLATE,
    "podDisruptionBudget": PDB_TEMPLATE,
}


def worker_version_set(
    name=SET_NAME,
    version=DESIRED_VERSION,
    image=DESIRED_IMAGE,
    grace_seconds=None,
    template=None,
    generation=1,
    status=None,
):
    spec = {"version": version, "image": image, "template": TEMPLATE if template is None else template}
    if grace_seconds is not None:
        spec["graceSeconds"] = grace_seconds
    item = {
        "apiVersion": f"{CRD_GROUP}/{CRD_VERSION}",
        "kind": CRD_KIND,
        "metadata": {"name": name, "namespace": NAMESPACE, "uid": f"uid-{name}", "generation": generation},
        "spec": spec,
    }
    if status is not None:
        item["status"] = status
    return item


class FakeKubeClient:
    """The collaborator the controller writes through, recording every call.

    ``managed`` is either a list for the single default set, or a mapping from
    set name to its Deployments, which is what the real label selector returns.
    """

    def __init__(self, existing=(), managed=(), sets=None, dry_run=False):
        self.sets = list(sets) if sets is not None else [worker_version_set()]
        self.existing = set(existing)
        self.managed = managed if isinstance(managed, dict) else {SET_NAME: list(managed)}
        self._dry_run = dry_run
        self.created = []
        self.patched = []
        self.deleted = []
        self.status_patches = []

    def list_worker_version_sets(self):
        return self.sets

    def patch_status(self, name, status):
        self.status_patches.append((name, status))

    def list_managed_deployments(self, managed_by, version_label, set_name):
        return self.managed.get(set_name, [])

    def exists(self, kind, name):
        return (kind, name) in self.existing

    def create(self, kind, manifest):
        name = manifest["metadata"]["name"]
        if self._dry_run:
            reconcile_logger.info("Would create worker object", kind=kind, name=name)
            return
        self.created.append((kind, manifest))

    def patch(self, kind, name, body):
        if self._dry_run:
            reconcile_logger.info("Would patch worker object", kind=kind, name=name)
            return
        self.patched.append((kind, name, body))

    def delete(self, kind, name):
        if self._dry_run:
            reconcile_logger.info("Would delete worker object", kind=kind, name=name)
            return
        self.deleted.append((kind, name))

    def mark(self, name, body):
        """The drained-since marker bypasses the dry-run guard, like the real KubeClient.mark."""
        self.patched.append(("deployment", name, body))

    def patches_for(self, kind, name):
        return [body for k, n, body in self.patched if k == kind and n == name]

    def status_for(self, name=SET_NAME):
        return [status for patched_name, status in self.status_patches if patched_name == name][-1]

    def condition_for(self, name=SET_NAME):
        return self.status_for(name)["conditions"][0]

    def version_status(self, version, name=SET_NAME):
        return next(entry for entry in self.status_for(name)["versions"] if entry["version"] == version)


def run(kube, outstanding=None, now=NOW, **overrides):
    settings = ControllerSettings(
        namespace=NAMESPACE,
        database_url=DATABASE_URL,
        **overrides,
    )
    reconcile(
        kube=kube,
        settings=settings,
        outstanding=outstanding or (lambda version: 0),
        now=lambda: now,
    )
    return kube


def managed_deployment(version=OLD_VERSION, replicas=0, annotations=None, image=None):
    return ManagedDeployment(
        name=f"dbos-worker-{version}",
        version=version,
        replicas=replicas,
        annotations=annotations or {},
        image=image,
    )


def test_creates_all_three_objects_for_a_fresh_desired_version():
    kube = run(FakeKubeClient())

    assert [kind for kind, _ in kube.created] == ["deployment", "scaledobject", "pdb"]
    assert all(manifest["metadata"]["name"] == f"dbos-worker-{DESIRED_VERSION}" for _, manifest in kube.created)
    deployment = kube.created[0][1]
    assert deployment["spec"]["template"]["spec"]["containers"][0]["image"] == DESIRED_IMAGE
    assert not kube.patched
    assert not kube.deleted


def test_created_objects_carry_the_set_label_and_no_owner_reference():
    kube = run(FakeKubeClient())

    for _, manifest in kube.created:
        assert manifest["metadata"]["labels"][SET_LABEL] == SET_NAME
        assert "ownerReferences" not in manifest["metadata"]


def test_the_set_label_is_repeated_on_every_patch():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = run(FakeKubeClient(existing=existing))

    for _, _, body in kube.patched:
        assert "ownerReferences" not in body["metadata"]
        if "labels" in body["metadata"]:
            assert body["metadata"]["labels"][SET_LABEL] == SET_NAME


def test_patches_the_desired_version_when_the_image_changes():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = run(
        FakeKubeClient(
            existing=existing,
            sets=[worker_version_set(image="registry.example.com/app:deadbee")],
        )
    )

    assert not kube.created
    deployment_patch = kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}")[0]
    container = deployment_patch["spec"]["template"]["spec"]["containers"][0]
    assert container["image"] == "registry.example.com/app:deadbee"


def test_non_desired_version_is_parked_with_a_single_outstanding_trigger():
    existing = [("scaledobject", f"dbos-worker-{OLD_VERSION}")]
    kube = run(
        FakeKubeClient(existing=existing, managed=[managed_deployment(replicas=2)]),
        outstanding=lambda version: 3,
    )

    spec = kube.patches_for("scaledobject", f"dbos-worker-{OLD_VERSION}")[0]["spec"]
    assert spec["minReplicaCount"] == 0
    assert len(spec["triggers"]) == 1
    trigger = spec["triggers"][0]
    assert trigger["type"] == "postgresql"
    assert trigger["metricType"] == "AverageValue"
    assert trigger["authenticationRef"] == {"name": "dbos-postgres-auth"}
    assert trigger["metadata"]["targetQueryValue"] == "1"
    assert trigger["metadata"]["query"] == (
        "SELECT count(*) FROM dbos.workflow_status "
        f"WHERE application_version = '{OLD_VERSION}' AND status IN ('ENQUEUED','PENDING')"
    )
    assert not kube.deleted


def test_creates_the_scaled_object_when_draining_a_version_whose_scaled_object_is_missing():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = run(
        FakeKubeClient(existing=existing, managed=[managed_deployment(replicas=2)]),
        outstanding=lambda version: 3,
    )

    created = [
        manifest
        for kind, manifest in kube.created
        if kind == "scaledobject" and manifest["metadata"]["name"] == f"dbos-worker-{OLD_VERSION}"
    ]
    assert len(created) == 1
    manifest = created[0]
    assert manifest["metadata"]["name"] == f"dbos-worker-{OLD_VERSION}"
    assert manifest["metadata"]["labels"][SET_LABEL] == SET_NAME
    assert "ownerReferences" not in manifest["metadata"]
    assert manifest["spec"]["minReplicaCount"] == 0
    assert len(manifest["spec"]["triggers"]) == 1
    trigger = manifest["spec"]["triggers"][0]
    assert trigger["metadata"]["query"] == (
        "SELECT count(*) FROM dbos.workflow_status "
        f"WHERE application_version = '{OLD_VERSION}' AND status IN ('ENQUEUED','PENDING')"
    )
    assert manifest["spec"]["scaleTargetRef"]["name"] == f"dbos-worker-{OLD_VERSION}"
    assert not kube.patches_for("scaledobject", f"dbos-worker-{OLD_VERSION}")


class RaisingFirstPatchKubeClient(FakeKubeClient):
    """A FakeKubeClient whose scaledobject patch raises once for a chosen name."""

    def __init__(self, *args, boom_name, **kwargs):
        super().__init__(*args, **kwargs)
        self._boom_name = boom_name
        self._boomed = False

    def patch(self, kind, name, body):
        if kind == "scaledobject" and name == self._boom_name and not self._boomed:
            self._boomed = True
            raise RuntimeError("kube api unavailable")
        super().patch(kind, name, body)


def test_a_failed_drain_does_not_block_the_remaining_versions():
    other_version = "v20260902120000"
    existing = [
        ("scaledobject", f"dbos-worker-{OLD_VERSION}"),
        ("scaledobject", f"dbos-worker-{other_version}"),
    ]
    managed = [managed_deployment(version=OLD_VERSION), managed_deployment(version=other_version)]
    kube = RaisingFirstPatchKubeClient(existing=existing, managed=managed, boom_name=f"dbos-worker-{OLD_VERSION}")

    run(kube, outstanding=lambda version: 0)

    assert kube.patches_for("scaledobject", f"dbos-worker-{other_version}")
    assert kube.patches_for("deployment", f"dbos-worker-{other_version}")

    condition = kube.condition_for()
    assert condition["status"] == "False"
    assert condition["reason"] == "DrainFailed"
    assert OLD_VERSION in condition["message"]
    assert other_version not in condition["message"]


def test_desired_version_is_never_drained_or_deleted_even_at_zero():
    managed = [managed_deployment(version=DESIRED_VERSION, replicas=0)]
    kube = run(FakeKubeClient(managed=managed), outstanding=lambda version: 0)

    assert not kube.deleted
    assert not kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}")


def test_first_idle_tick_stamps_the_drained_marker_and_deletes_nothing():
    kube = run(FakeKubeClient(managed=[managed_deployment()]))

    patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW_STAMP
    assert not kube.deleted


def test_deletes_only_after_the_grace_period_elapses():
    stamped = managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: (NOW - timedelta(seconds=599)).isoformat()})
    kube = run(FakeKubeClient(managed=[stamped]))
    assert not kube.deleted

    expired = managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: (NOW - timedelta(seconds=601)).isoformat()})
    kube = run(FakeKubeClient(managed=[expired]))
    assert kube.deleted == [
        ("scaledobject", f"dbos-worker-{OLD_VERSION}"),
        ("pdb", f"dbos-worker-{OLD_VERSION}"),
        ("deployment", f"dbos-worker-{OLD_VERSION}"),
    ]


def test_spec_grace_seconds_overrides_the_controller_default():
    stamped = managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: (NOW - timedelta(seconds=90)).isoformat()})

    kube = run(FakeKubeClient(managed=[stamped], sets=[worker_version_set(grace_seconds=120)]))
    assert not kube.deleted

    kube = run(FakeKubeClient(managed=[stamped], sets=[worker_version_set(grace_seconds=60)]))
    assert [kind for kind, _ in kube.deleted] == ["scaledobject", "pdb", "deployment"]


def test_clears_the_marker_when_workflows_reappear():
    stamped = managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: (NOW - timedelta(hours=2)).isoformat()})
    kube = run(FakeKubeClient(managed=[stamped]), outstanding=lambda version: 7)

    patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert patch["metadata"]["annotations"] == {DRAINED_SINCE_ANNOTATION: None}
    assert not kube.deleted


def test_skips_all_deletes_when_the_outstanding_query_raises():
    expired = {DRAINED_SINCE_ANNOTATION: (NOW - timedelta(hours=2)).isoformat()}
    managed = [
        managed_deployment(version=OLD_VERSION, annotations=expired),
        managed_deployment(version="v20260902120000", annotations=expired),
    ]
    existing = [
        ("scaledobject", f"dbos-worker-{OLD_VERSION}"),
        ("scaledobject", "dbos-worker-v20260902120000"),
    ]

    def boom(version):
        raise RuntimeError("database unreachable")

    kube = run(FakeKubeClient(existing=existing, managed=managed), outstanding=boom)

    assert not kube.deleted
    assert not kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")
    assert len([name for kind, name, _ in kube.patched if kind == "scaledobject"]) == 2


def test_an_unreachable_database_reports_the_database_unavailable_condition():
    def boom(version):
        raise RuntimeError("database unreachable")

    kube = run(FakeKubeClient(managed=[managed_deployment()]), outstanding=boom)

    condition = kube.condition_for()
    assert condition["status"] == "False"
    assert condition["reason"] == "DatabaseUnavailable"
    assert "outstanding" not in kube.version_status(OLD_VERSION)


def test_database_unavailable_takes_precedence_over_a_failed_drain():
    existing = [("scaledobject", f"dbos-worker-{OLD_VERSION}")]
    kube = RaisingFirstPatchKubeClient(
        existing=existing, managed=[managed_deployment()], boom_name=f"dbos-worker-{OLD_VERSION}"
    )

    def boom(version):
        raise RuntimeError("database unreachable")

    run(kube, outstanding=boom)

    condition = kube.condition_for()
    assert condition["status"] == "False"
    assert condition["reason"] == "DatabaseUnavailable"


@pytest.mark.parametrize(
    ("version", "image", "template"),
    [
        ("V20260916135248", DESIRED_IMAGE, None),
        ("", DESIRED_IMAGE, None),
        ("v1'; drop table x--", DESIRED_IMAGE, None),
        (DESIRED_VERSION, "app:latest; rm -rf /", None),
        (DESIRED_VERSION, DESIRED_IMAGE, {"deployment": DEPLOYMENT_TEMPLATE}),
    ],
)
def test_an_invalid_spec_writes_nothing_and_reports_invalid_spec(version, image, template):
    expired = {DRAINED_SINCE_ANNOTATION: (NOW - timedelta(hours=2)).isoformat()}
    kube = run(
        FakeKubeClient(
            managed=[managed_deployment(annotations=expired)],
            sets=[worker_version_set(version=version, image=image, template=template)],
        ),
    )

    assert not kube.created
    assert not kube.patched
    assert not kube.deleted
    condition = kube.condition_for()
    assert condition["status"] == "False"
    assert condition["reason"] == "InvalidSpec"
    assert kube.status_for()["versions"] == []


def test_two_sets_in_one_namespace_reconcile_without_draining_each_other():
    other_set = "other-worker"
    other_version = "v20260902120000"
    sets = [worker_version_set(), worker_version_set(name=other_set, version=other_version)]
    managed = {
        SET_NAME: [managed_deployment(version=OLD_VERSION)],
        other_set: [managed_deployment(version=other_version)],
    }
    kube = run(FakeKubeClient(managed=managed, sets=sets))

    assert not kube.deleted
    drained = [
        name for kind, name, body in kube.patched if kind == "deployment" and DRAINED_SINCE_ANNOTATION in str(body)
    ]
    assert drained == [f"dbos-worker-{OLD_VERSION}"]
    assert [entry["version"] for entry in kube.status_for(SET_NAME)["versions"]] == [DESIRED_VERSION, OLD_VERSION]
    assert [entry["version"] for entry in kube.status_for(other_set)["versions"]] == [other_version]


class RaisingApplyKubeClient(FakeKubeClient):
    """A FakeKubeClient whose `exists` raises for one set's desired version."""

    def __init__(self, *args, boom_version, **kwargs):
        super().__init__(*args, **kwargs)
        self._boom_version = boom_version

    def exists(self, kind, name):
        if name == f"dbos-worker-{self._boom_version}":
            raise RuntimeError("kube api unavailable")
        return super().exists(kind, name)


def test_a_failing_set_reports_reconcile_error_and_the_next_set_still_reconciles():
    other_set = "other-worker"
    other_version = "v20260902120000"
    sets = [worker_version_set(), worker_version_set(name=other_set, version=other_version)]
    kube = RaisingApplyKubeClient(sets=sets, boom_version=DESIRED_VERSION)

    run(kube)

    assert kube.condition_for(SET_NAME)["reason"] == "ReconcileError"
    assert kube.condition_for(other_set)["reason"] == "Reconciled"
    assert [kind for kind, _ in kube.created] == ["deployment", "scaledobject", "pdb"]


def test_status_reports_each_phase_and_keeps_drained_since_to_drained_versions():
    busy = managed_deployment(version="v20260902120000", replicas=2, image="registry.example.com/app:old")
    idle = managed_deployment(version=OLD_VERSION, replicas=0)
    current = managed_deployment(version=DESIRED_VERSION, replicas=3, image=DESIRED_IMAGE)
    kube = run(
        FakeKubeClient(managed=[current, busy, idle]),
        outstanding=lambda version: 4 if version == busy.version else 0,
    )

    status = kube.status_for()
    assert status["observedGeneration"] == 1
    assert status["lastReconcileTime"] == NOW_STAMP
    assert kube.version_status(DESIRED_VERSION) == {
        "version": DESIRED_VERSION,
        "phase": "Current",
        "image": DESIRED_IMAGE,
        "replicas": 3,
        "outstanding": 0,
    }
    assert kube.version_status(busy.version) == {
        "version": busy.version,
        "phase": "Draining",
        "image": "registry.example.com/app:old",
        "replicas": 2,
        "outstanding": 4,
    }
    assert kube.version_status(OLD_VERSION) == {
        "version": OLD_VERSION,
        "phase": "Drained",
        "replicas": 0,
        "outstanding": 0,
        "drainedSince": NOW_STAMP,
    }
    assert kube.condition_for()["reason"] == "Reconciled"


def test_the_ready_transition_time_survives_a_tick_that_changes_nothing():
    earlier = "2026-09-01T00:00:00Z"
    ready = {"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": earlier}
    kube = run(FakeKubeClient(sets=[worker_version_set(status={"conditions": [ready]})]))

    assert kube.condition_for()["lastTransitionTime"] == earlier

    flipped = {**ready, "status": "False", "reason": "InvalidSpec"}
    kube = run(FakeKubeClient(sets=[worker_version_set(status={"conditions": [flipped]})]))

    assert kube.condition_for()["lastTransitionTime"] == NOW_STAMP


def _recording_api_client() -> tuple[k8s_client.ApiClient, MagicMock]:
    """A real ApiClient whose transport is mocked, so request headers can be asserted."""
    api_client = k8s_client.ApiClient(k8s_client.Configuration(host="http://kube.test"))
    response = MagicMock()
    response.data = b"{}"
    response.getheaders.return_value = {}
    response.getheader.return_value = "application/json"
    rest_client = MagicMock()
    rest_client.PATCH.return_value = response
    rest_client.POST.return_value = response
    rest_client.DELETE.return_value = response
    api_client.rest_client = rest_client
    return api_client, rest_client


def test_dry_run_performs_no_write():
    api_client, rest_client = _recording_api_client()
    patch_api_client, patch_rest_client = _recording_api_client()
    kube = KubeClient(namespace=NAMESPACE, dry_run=True, api_client=api_client, patch_api_client=patch_api_client)

    kube.create("deployment", {"metadata": {"name": "dbos-worker-x"}})
    kube.patch("scaledobject", "dbos-worker-x", {"spec": {"minReplicaCount": 0}})
    kube.delete("pdb", "dbos-worker-x")

    assert not rest_client.mock_calls
    assert not patch_rest_client.mock_calls


def test_status_is_written_in_dry_run_because_it_is_an_observation():
    api_client, _ = _recording_api_client()
    patch_api_client, patch_rest_client = _recording_api_client()
    kube = KubeClient(namespace=NAMESPACE, dry_run=True, api_client=api_client, patch_api_client=patch_api_client)

    kube.patch_status(SET_NAME, {"observedGeneration": 1})

    _, kwargs = patch_rest_client.PATCH.call_args
    assert kwargs["headers"]["Content-Type"] == "application/merge-patch+json"
    assert kwargs["body"] == {"status": {"observedGeneration": 1}}
    assert patch_rest_client.PATCH.call_args[0][0].endswith(
        f"/apis/{CRD_GROUP}/{CRD_VERSION}/namespaces/{NAMESPACE}/workerversionsets/{SET_NAME}/status"
    )


def test_mark_is_written_in_dry_run_because_it_is_an_observation():
    api_client, _ = _recording_api_client()
    patch_api_client, patch_rest_client = _recording_api_client()
    kube = KubeClient(namespace=NAMESPACE, dry_run=True, api_client=api_client, patch_api_client=patch_api_client)

    kube.mark(f"dbos-worker-{OLD_VERSION}", {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: NOW_STAMP}}})

    _, kwargs = patch_rest_client.PATCH.call_args
    assert kwargs["body"] == {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: NOW_STAMP}}}


def test_dry_run_still_writes_the_drained_marker_so_the_grace_clock_advances():
    kube = run(FakeKubeClient(managed=[managed_deployment()], dry_run=True))

    marker_patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert marker_patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW_STAMP


def test_dry_run_clears_a_stale_marker_on_the_desired_deployment_via_mark():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = FakeKubeClient(existing=existing, dry_run=True)

    run(kube)

    assert kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}") == [
        {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}}
    ]
    assert not kube.patches_for("scaledobject", f"dbos-worker-{DESIRED_VERSION}")
    assert not kube.patches_for("pdb", f"dbos-worker-{DESIRED_VERSION}")


def test_dry_run_past_grace_previews_all_three_deletes(caplog):
    expired = {DRAINED_SINCE_ANNOTATION: (NOW - timedelta(seconds=601)).isoformat()}
    kube = FakeKubeClient(managed=[managed_deployment(annotations=expired)], dry_run=True)

    with caplog.at_level("INFO", logger="dbos_version_controller.reconcile"):
        run(kube)

    would_delete = [record for record in caplog.records if record.getMessage().startswith("Would delete")]
    assert len(would_delete) == 3
    assert not kube.deleted


def test_patches_are_sent_as_json_merge_patches():
    patch_api_client, patch_rest_client = _recording_api_client()
    api_client, _ = _recording_api_client()
    kube = KubeClient(
        namespace=NAMESPACE,
        api_client=api_client,
        patch_api_client=patch_api_client,
    )

    kube.patch("scaledobject", "dbos-worker-x", {"spec": {"minReplicaCount": 0}})

    _, kwargs = patch_rest_client.PATCH.call_args
    assert kwargs["headers"]["Content-Type"] == "application/merge-patch+json"
    assert kwargs["body"] == {"spec": {"minReplicaCount": 0}}


def test_patching_the_desired_deployment_clears_any_stale_drained_marker():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = run(FakeKubeClient(existing=existing))

    deployment_patches = kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}")
    assert deployment_patches[-1] == {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}}

    scaledobject_patch = kube.patches_for("scaledobject", f"dbos-worker-{DESIRED_VERSION}")[0]
    assert "annotations" not in scaledobject_patch.get("metadata", {})


class StatefulFakeKubeClient(FakeKubeClient):
    """A FakeKubeClient that folds deployment annotation patches back into `managed`.

    Lets a test drive two reconcile ticks in a row and see the second tick observe
    the annotation state the first tick's patch actually produced.
    """

    def patch(self, kind, name, body):
        super().patch(kind, name, body)
        if kind == "deployment":
            self._fold_annotations(name, body)

    def mark(self, name, body):
        super().mark(name, body)
        self._fold_annotations(name, body)

    def _fold_annotations(self, name, body):
        patched_annotations = (body.get("metadata") or {}).get("annotations")
        if patched_annotations is None:
            return
        for deployments in self.managed.values():
            for index, deployment in enumerate(deployments):
                if deployment.name != name:
                    continue
                merged = dict(deployment.annotations)
                for key, value in patched_annotations.items():
                    if value is None:
                        merged.pop(key, None)
                    else:
                        merged[key] = value
                deployments[index] = ManagedDeployment(
                    name=deployment.name,
                    version=deployment.version,
                    replicas=deployment.replicas,
                    annotations=merged,
                    image=deployment.image,
                )


def test_rollback_gets_a_fresh_grace_period_instead_of_an_immediate_delete():
    stale_stamp = (NOW - timedelta(hours=2)).isoformat()
    managed = [managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: stale_stamp})]
    existing = [(kind, f"dbos-worker-{OLD_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = StatefulFakeKubeClient(existing=existing, managed=managed, sets=[worker_version_set(version=OLD_VERSION)])

    run(kube)  # OLD_VERSION is desired again (rollback): its Deployment gets patched.

    rollback_patches = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")
    assert rollback_patches[-1] == {"metadata": {"annotations": {DRAINED_SINCE_ANNOTATION: None}}}
    assert kube.managed[SET_NAME][0].annotations == {}

    kube.sets[0]["spec"]["version"] = DESIRED_VERSION
    kube.patched.clear()
    run(kube, outstanding=lambda version: 0)  # OLD_VERSION drains again and hits 0 rows / 0 pods.

    drain_patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert drain_patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW_STAMP
    assert not kube.deleted


def test_rollback_gets_a_fresh_grace_period_under_dry_run_too():
    """The marker clear goes through `mark`, which bypasses dry run like `_drain`'s own marker

    writes. Without that, a rollback previewed under dry run leaves the stale marker in place,
    and the next drain tick sees a marker already past grace and previews an immediate delete.
    """
    stale_stamp = (NOW - timedelta(hours=2)).isoformat()
    managed = [managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: stale_stamp})]
    existing = [(kind, f"dbos-worker-{OLD_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = StatefulFakeKubeClient(
        existing=existing, managed=managed, sets=[worker_version_set(version=OLD_VERSION)], dry_run=True
    )

    run(kube)  # OLD_VERSION is desired again (rollback): its stale marker is cleared via mark.

    assert kube.managed[SET_NAME][0].annotations == {}

    kube.sets[0]["spec"]["version"] = DESIRED_VERSION
    kube.patched.clear()
    run(kube, outstanding=lambda version: 0)  # OLD_VERSION drains again and hits 0 rows / 0 pods.

    drain_patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert drain_patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW_STAMP
    assert not kube.deleted


def test_settings_parse_from_env(monkeypatch):
    monkeypatch.setenv("DVC_NAMESPACE", "dbos-workers-stg")
    monkeypatch.setenv("DVC_DATABASE_URL", DATABASE_URL)
    monkeypatch.setenv("DVC_INTERVAL_SECONDS", "5")
    monkeypatch.setenv("DVC_GRACE_SECONDS", "120")
    monkeypatch.setenv("DVC_DRY_RUN", "true")

    settings = ControllerSettings()  # pyright: ignore[reportCallIssue]

    assert settings.namespace == "dbos-workers-stg"
    assert settings.database_url == DATABASE_URL
    assert settings.interval_seconds == 5
    assert settings.grace_seconds == 120
    assert settings.dry_run is True
    assert settings.managed_by == "dbos-version-controller"
    assert settings.version_label == "dbos-app-version"
