# Changelog

All notable changes to this project are recorded here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## 0.1.0 — initial

- Reconcile loop: apply the desired version's Deployment, ScaledObject and PDB from a template ConfigMap.
- Drain every other managed version to `minReplicaCount: 0` with one outstanding-workflow trigger.
- Delete a drained version after zero rows and zero pods for the grace period.
- Safety rules: never delete the desired version, never delete while the database is unreachable, and `DVC_DRY_RUN`.
- Settings from `DVC_` environment variables.
