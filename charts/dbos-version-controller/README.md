# dbos-version-controller Helm chart

This chart installs the controller into one namespace. It renders:

- the `WorkerVersionSet` CRD, when `crds.install` is `true`;
- a ServiceAccount;
- a Role and a RoleBinding in the namespace the controller watches;
- the Deployment, with one replica and the `Recreate` strategy;
- an ExternalSecret for the database URL, when `databaseUrl.externalSecret.enabled` is `true`.

## Install

```bash
helm install dbos-version-controller charts/dbos-version-controller \
  --namespace dbos-workers --set databaseUrl.secretName=dbos-database
```

The Secret `dbos-database` must hold the DBOS system database URL under the key
`url`. The image has no shell, so the URL must arrive as an environment
variable.

## Controller settings

Each `DVC_` setting is one value. The defaults are the controller's own defaults.

| Value | Variable | Default |
| --- | --- | --- |
| `controller.namespace` | `DVC_NAMESPACE` | the release namespace |
| `controller.dryRun` | `DVC_DRY_RUN` | `false` |
| `controller.graceSeconds` | `DVC_GRACE_SECONDS` | `600` |
| `controller.intervalSeconds` | `DVC_INTERVAL_SECONDS` | `30` |
| `controller.managedBy` | `DVC_MANAGED_BY` | `dbos-version-controller` |
| `controller.versionLabel` | `DVC_VERSION_LABEL` | `dbos-app-version` |
| `controller.logLevel` | `DVC_LOG_LEVEL` | `info` |
| `databaseUrl.secretName`, `databaseUrl.secretKey` | `DVC_DATABASE_URL` | required, `url` |

`values.yaml` describes every other value.

## Notes

- **CRD.** The chart ships the CRD by default, so the schema and the
  controller that reads it change together. The CRD is cluster-scoped, so
  install the chart once per cluster with `crds.install=true`, and set it to
  `false` only when the CRD comes from somewhere else. With `crds.keep=true`,
  the default, the CRD carries `helm.sh/resource-policy: keep` and Argo CD's
  `Prune=false,Delete=false`. Neither Helm nor Argo CD then deletes it, because
  deleting the CRD deletes every WorkerVersionSet. To remove it, delete it by
  hand. With Argo CD, the Application's AppProject must allow
  `apiextensions.k8s.io/CustomResourceDefinition`.
- **RBAC.** The Role rules come from `config/rbac/role.yaml`, which
  controller-gen writes from the reconciler's markers. `make manifests` copies
  that file into `files/`. Do not edit the copy.
- **Object names.** Every object uses the full name. The Deployment selector is
  `app.kubernetes.io/name: <full name>`, and a Deployment selector cannot
  change. Use `fullnameOverride` to adopt objects that another tool created with
  the same names.
- **Pod labels.** `podLabels` must not include `controller.versionLabel`. A pod
  that carries the version label holds that version, so the controller would
  never delete it. The chart refuses to render such a value.
- **Database proxy.** Put a Cloud SQL proxy in `initContainers` as a native
  sidecar (`restartPolicy: Always`). The controller then reaches the database on
  `localhost`.
- **One replica.** The chart runs one replica with no leader election. Two
  controllers that reconcile the same set fight each other.
