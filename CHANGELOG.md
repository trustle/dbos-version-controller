# Changelog

All notable changes to this project are recorded here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

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
