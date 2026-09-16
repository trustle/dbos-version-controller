from datetime import UTC, datetime, timedelta
from unittest.mock import MagicMock

import pytest
from kubernetes import client as k8s_client

from dbos_version_controller.reconcile import (
    DRAINED_SINCE_ANNOTATION,
    KubeClient,
    ManagedDeployment,
    reconcile,
)
from dbos_version_controller.settings import ControllerSettings

DESIRED_VERSION = "v20260916135248"
DESIRED_IMAGE = "registry.example.com/app:8332cd5"
OLD_VERSION = "v20260901120000"

NAMESPACE = "dbos-workers"
DATABASE_URL = "postgresql://user:pass@localhost:5432/dbos"

NOW = datetime(2026, 9, 16, 14, 0, 0, tzinfo=UTC)

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


class FakeKubeClient:
    def __init__(self, existing=(), managed=(), version=DESIRED_VERSION, image=DESIRED_IMAGE):
        self.configmaps = {
            "dbos-worker-desired": {"version": version, "image": image},
            "dbos-worker-template": {
                "deployment.yaml": DEPLOYMENT_TEMPLATE,
                "scaledobject.yaml": SCALED_OBJECT_TEMPLATE,
                "pdb.yaml": PDB_TEMPLATE,
            },
        }
        self.existing = set(existing)
        self.managed = list(managed)
        self.created = []
        self.patched = []
        self.deleted = []

    def read_configmap(self, name):
        return self.configmaps[name]

    def list_managed_deployments(self, managed_by, version_label):
        return self.managed

    def exists(self, kind, name):
        return (kind, name) in self.existing

    def create(self, kind, manifest):
        self.created.append((kind, manifest))

    def patch(self, kind, name, body):
        self.patched.append((kind, name, body))

    def delete(self, kind, name):
        self.deleted.append((kind, name))

    def patches_for(self, kind, name):
        return [body for k, n, body in self.patched if k == kind and n == name]


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


def managed_deployment(version=OLD_VERSION, replicas=0, annotations=None):
    return ManagedDeployment(
        name=f"dbos-worker-{version}",
        version=version,
        replicas=replicas,
        annotations=annotations or {},
    )


def test_creates_all_three_objects_for_a_fresh_desired_version():
    kube = run(FakeKubeClient())

    assert [kind for kind, _ in kube.created] == ["deployment", "scaledobject", "pdb"]
    assert all(manifest["metadata"]["name"] == f"dbos-worker-{DESIRED_VERSION}" for _, manifest in kube.created)
    deployment = kube.created[0][1]
    assert deployment["spec"]["template"]["spec"]["containers"][0]["image"] == DESIRED_IMAGE
    assert not kube.patched
    assert not kube.deleted


def test_patches_the_desired_version_when_the_image_changes():
    existing = [(kind, f"dbos-worker-{DESIRED_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = run(FakeKubeClient(existing=existing, image="registry.example.com/app:deadbee"))

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


def test_desired_version_is_never_drained_or_deleted_even_at_zero():
    managed = [managed_deployment(version=DESIRED_VERSION, replicas=0)]
    kube = run(FakeKubeClient(managed=managed), outstanding=lambda version: 0)

    assert not kube.deleted
    assert not kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}")


def test_first_idle_tick_stamps_the_drained_marker_and_deletes_nothing():
    kube = run(FakeKubeClient(managed=[managed_deployment()]))

    patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW.isoformat()
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


@pytest.mark.parametrize(
    ("version", "image"),
    [
        ("V20260916135248", DESIRED_IMAGE),
        ("", DESIRED_IMAGE),
        ("v1'; drop table x--", DESIRED_IMAGE),
        (DESIRED_VERSION, "app:latest; rm -rf /"),
    ],
)
def test_an_invalid_desired_version_skips_the_whole_tick(version, image):
    expired = {DRAINED_SINCE_ANNOTATION: (NOW - timedelta(hours=2)).isoformat()}
    kube = run(
        FakeKubeClient(managed=[managed_deployment(annotations=expired)], version=version, image=image),
    )

    assert not kube.created
    assert not kube.patched
    assert not kube.deleted


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

    deployment_patch = kube.patches_for("deployment", f"dbos-worker-{DESIRED_VERSION}")[0]
    assert deployment_patch["metadata"]["annotations"] == {DRAINED_SINCE_ANNOTATION: None}

    scaledobject_patch = kube.patches_for("scaledobject", f"dbos-worker-{DESIRED_VERSION}")[0]
    assert "annotations" not in scaledobject_patch.get("metadata", {})


class StatefulFakeKubeClient(FakeKubeClient):
    """A FakeKubeClient that folds deployment annotation patches back into `managed`.

    Lets a test drive two reconcile ticks in a row and see the second tick observe
    the annotation state the first tick's patch actually produced.
    """

    def patch(self, kind, name, body):
        super().patch(kind, name, body)
        if kind != "deployment":
            return
        patched_annotations = (body.get("metadata") or {}).get("annotations")
        if patched_annotations is None:
            return
        for index, deployment in enumerate(self.managed):
            if deployment.name != name:
                continue
            merged = dict(deployment.annotations)
            for key, value in patched_annotations.items():
                if value is None:
                    merged.pop(key, None)
                else:
                    merged[key] = value
            self.managed[index] = ManagedDeployment(
                name=deployment.name, version=deployment.version, replicas=deployment.replicas, annotations=merged
            )


def test_rollback_gets_a_fresh_grace_period_instead_of_an_immediate_delete():
    stale_stamp = (NOW - timedelta(hours=2)).isoformat()
    managed = [managed_deployment(annotations={DRAINED_SINCE_ANNOTATION: stale_stamp})]
    existing = [(kind, f"dbos-worker-{OLD_VERSION}") for kind in ("deployment", "scaledobject", "pdb")]
    kube = StatefulFakeKubeClient(existing=existing, managed=managed, version=OLD_VERSION)

    run(kube)  # OLD_VERSION is desired again (rollback): its Deployment gets patched.

    rollback_patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert rollback_patch["metadata"]["annotations"] == {DRAINED_SINCE_ANNOTATION: None}
    assert kube.managed[0].annotations == {}

    kube.configmaps["dbos-worker-desired"]["version"] = DESIRED_VERSION
    kube.patched.clear()
    run(kube, outstanding=lambda version: 0)  # OLD_VERSION drains again and hits 0 rows / 0 pods.

    drain_patch = kube.patches_for("deployment", f"dbos-worker-{OLD_VERSION}")[0]
    assert drain_patch["metadata"]["annotations"][DRAINED_SINCE_ANNOTATION] == NOW.isoformat()
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
    assert settings.desired_configmap == "dbos-worker-desired"
    assert settings.template_configmap == "dbos-worker-template"
    assert settings.managed_by == "dbos-version-controller"
    assert settings.version_label == "dbos-app-version"
