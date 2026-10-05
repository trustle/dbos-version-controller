# Changelog

All notable changes to this project are recorded here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## Unreleased

- New Helm chart in `charts/dbos-version-controller`. It holds the CRD (on by default, behind `crds.install`; it carries Helm's `resource-policy: keep` and Argo CD's `Prune=false,Delete=false`, so neither tool deletes it), the namespaced ServiceAccount, Role and RoleBinding, the Deployment and an optional ExternalSecret for the database URL. Each `DVC_` setting is one value. `make manifests` copies the CRD and `config/rbac/role.yaml` into the chart, so the chart's Role always matches the reconciler's markers. The Deployment selector holds only `app.kubernetes.io/name=<full name>`, so a release with `fullnameOverride` can adopt an existing Deployment of that name.
- The e2e suite installs the controller through the chart instead of `make deploy`.
- A version stays while a live pod outside the set carries its version label. An application whose API enqueues work at its own build's version keeps writing work for that version until its API pods are replaced, so a version whose workers drained can still receive work. The controller counts the namespace's live pods that carry `<DVC_VERSION_LABEL>=<version>` but not `app.kubernetes.io/managed-by=<DVC_MANAGED_BY>`, and treats any such pod that is not `Succeeded` or `Failed` like an outstanding workflow: the version is not marked drained, and is never deleted. A terminating pod still counts. An unreadable pod list, or a reconciler built with no pod reader, deletes nothing.
- A pod count the controller cannot read reports `Ready=False` with the new reason `PodsUnavailable`, naming the versions it could not count. `DatabaseUnavailable` still takes precedence.
- `DVC_VERSION_LABEL` must be a valid label key and `DVC_MANAGED_BY` a non-empty valid label value; the controller refuses to start otherwise, because both go into the holder selector.
- The Role gains `list` on `pods`. Grant it before upgrading: without it every pod count is unknown, and the controller deletes no drained version.

## 0.3.0 — Rewritten in Go with kubebuilder; CRD contract unchanged

- The controller is a Go kubebuilder v4 / controller-runtime manager. The Python package, its tests and its uv packaging are gone.
- The `dbos.trustle.com/v1alpha1` `WorkerVersionSet` contract is unchanged: same group, kind, short name, fields, validation patterns, printer columns and status shape. A chart that renders the custom resource keeps working. The CRD is now generated from `api/v1alpha1/workerversionset_types.go`, and `make manifests` copies it to the stable path `crd/dbos.trustle.com_workerversionsets.yaml`.
- The timer loop became a controller-runtime reconcile: one set per event, requeued every `DVC_INTERVAL_SECONDS`. The manager watches only `DVC_NAMESPACE`.
- The settings keep their `DVC_` names. Each one also has a flag; the environment variable wins.
- The controller now assigns the managed-by and version labels to every rendered object, as it already did for the set label. A template can no longer render an object the controller cannot find again.
- The image is distroless and carries no shell. A deployment that configured the container through a shell command must move to a `secretKeyRef` or an init container.
- The tests run against a real API server through envtest, with the KEDA ScaledObject CRD installed from `test/crds/`.

## 0.2.0 — WorkerVersionSet CRD replaces the desired/template ConfigMaps

- New `dbos.trustle.com/v1alpha1` `WorkerVersionSet` (short name `wvs`): one custom resource holds the desired version, the image, the optional grace period and the three manifest templates.
- The controller reconciles every WorkerVersionSet in its namespace independently; a failing set does not stop the others.
- Each set's objects carry `dbos.trustle.com/worker-version-set=<set name>`, so the managed-set query is per set. Ownership is label-based, not an ownerReference: deleting the set does not delete its objects, and a re-created set with the same name adopts them.
- `spec.graceSeconds` overrides `DVC_GRACE_SECONDS` for one set.
- The controller writes `.status`: `observedGeneration`, `lastReconcileTime`, one entry per live version (`Current`, `Draining` or `Drained`) and a `Ready` condition with reason `Reconciled`, `InvalidSpec`, `DatabaseUnavailable`, `DrainFailed` or `ReconcileError`. Status is written in `DVC_DRY_RUN` too.
- Removed `DVC_DESIRED_CONFIGMAP` and `DVC_TEMPLATE_CONFIGMAP`. The Role drops `configmaps` and gains `workerversionsets` and `workerversionsets/status`.

## 0.1.0 — initial

- Reconcile loop: apply the desired version's Deployment, ScaledObject and PDB from a template ConfigMap.
- Drain every other managed version to `minReplicaCount: 0` with one outstanding-workflow trigger.
- Delete a drained version after zero rows and zero pods for the grace period.
- Safety rules: never delete the desired version, never delete while the database is unreachable, and `DVC_DRY_RUN`.
- Settings from `DVC_` environment variables.
