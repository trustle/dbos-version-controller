# dbos-version-controller

A small Kubernetes controller. It runs one worker Deployment per DBOS
application version, and it removes an old version only after that version has
no work left.

Status: built for the London service at Trustle and in rollout there. Private for now.

Written in Go with kubebuilder v4 and controller-runtime. Version 0.3.0 replaced
the original Python implementation. The `WorkerVersionSet` contract did not
change, so an existing chart that renders the custom resource keeps working.

## The problem

DBOS stamps every workflow row with the application version that started it. A
DBOS executor dequeues and recovers only the workflows of its own version. That
rule is what makes a rolling deploy safe, and it is also the problem:

- You deploy version B. The new pods take new work.
- Version A still owns in-flight and enqueued workflows.
- If the deploy deletes the version A pods, that work has no executor. It stops
  until somebody brings version A back.

So the cluster must keep the old version alive until it is empty. The list of
live versions changes with the workload, not with the commit. Git cannot hold
that list, and a GitOps tool cannot manage it: a version disappears from the
list hours after the commit that replaced it.

This controller holds the list instead. Git holds one `WorkerVersionSet`
custom resource per application, with the desired `(version, image)` pair. The
controller owns everything else, and reports what it found on the resource's
status.

## How it works

The controller watches every `WorkerVersionSet` in `DVC_NAMESPACE`. It
reconciles one set per event, and requeues that set every
`DVC_INTERVAL_SECONDS`. A set that fails does not stop the other sets.

For one set:

1. Read `spec.version` and `spec.image`.
2. Validate both, and check that `spec.template` holds the three manifests. An
   invalid spec skips the set, and the controller deletes nothing for it.
3. Substitute `__VERSION__` and `__IMAGE__` in the three manifest templates.
4. Create the desired version's Deployment, ScaledObject and PDB, or JSON
   merge-patch them if they exist. Clear the drained marker on the Deployment.
5. List every Deployment carrying both
   `app.kubernetes.io/managed-by=<managed_by>` and
   `dbos.trustle.com/worker-version-set=<set name>`. Skip the desired version.
6. For each other version, count its `ENQUEUED` and `PENDING` rows in
   `dbos.workflow_status`.
7. Park that version: patch its ScaledObject to `minReplicaCount: 0` with one
   postgresql trigger that counts the same rows. KEDA then scales the version up
   when its own work appears, and back to zero when that work finishes.
8. Count the pods in the namespace that carry `<version_label>=<version>` but
   not `app.kubernetes.io/managed-by=<managed_by>`, and are not `Succeeded` or
   `Failed`: pods outside the set that still run that version, such as an API
   that enqueues work at its own version. A terminating pod still counts.
9. If the version has zero rows, zero worker pods and zero such pods, write the
   drained marker annotation with the current time. If work or a pod returns,
   clear the marker.
10. Delete the version's three objects once the marker is older than the grace
   period.
11. Patch the set's status subresource with the versions it saw and one `Ready`
    condition.

```mermaid
flowchart TD
    A[Reconcile one WorkerVersionSet] --> B{spec valid?}
    B -- no --> Z[Skip the set. Delete nothing. Ready=False/InvalidSpec.]
    B -- yes --> C[Render templates for the desired version]
    C --> D[Create or patch Deployment, ScaledObject, PDB]
    D --> E[List this set's managed Deployments]
    E --> F{Is this the desired version?}
    F -- yes --> E
    F -- no --> G[Count ENQUEUED + PENDING rows for that version]
    G --> H[Patch ScaledObject: minReplicaCount 0, one outstanding trigger]
    H --> I{rows = 0, worker pods = 0 and no other pod carries the version?}
    I -- no --> J[Clear the drained marker]
    I -- yes --> K{Marker older than grace?}
    K -- no --> L[Write or keep the marker]
    K -- yes --> M[Delete ScaledObject, PDB, Deployment]
    J --> N[Patch the set status]
    L --> N
    M --> N
```

## The WorkerVersionSet contract

The controller reads every `WorkerVersionSet` in `DVC_NAMESPACE`. It writes only
their status subresource, never their spec. Install the definition from
`crd/dbos.trustle.com_workerversionsets.yaml`. That file is the output of
`make manifests`, copied to a stable path the Helm chart reads. Do not edit it
by hand; edit `api/v1alpha1/workerversionset_types.go` and run `make manifests`.

```yaml
apiVersion: dbos.trustle.com/v1alpha1
kind: WorkerVersionSet
metadata:
  name: dbos-worker
  namespace: dbos-workers
spec:
  version: "v20260916135248"
  image: "registry.example.com/app:8332cd5"
  graceSeconds: 600
  template:
    deployment: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: dbos-worker-__VERSION__
        labels:
          app.kubernetes.io/managed-by: dbos-version-controller
          dbos-app-version: __VERSION__
      ...
    scaledObject: |
      ...
    podDisruptionBudget: |
      ...
```

Rules for the spec:

- `version` must match `^[a-z0-9][a-z0-9.-]{0,48}$`. The pattern admits no quote
  character, because the version goes into a KEDA query string.
- `image` must match `^[A-Za-z0-9./:_@-]+$`.
- `graceSeconds` is optional. It overrides `DVC_GRACE_SECONDS` for this set.
- `template` must hold all three keys: `deployment`, `scaledObject` and
  `podDisruptionBudget`.
- A spec outside these rules skips the set. The controller writes nothing for it
  except a `Ready=False` status with reason `InvalidSpec`.

Rules for each template:

- Write `__VERSION__` where the application version belongs. Write `__IMAGE__`
  where the image reference belongs. The controller replaces both as plain text.
- Give all three objects the same `metadata.name`. The controller addresses a
  version's Deployment, ScaledObject and PDB by one shared name. A name pattern
  such as `<prefix>-__VERSION__` is the usual choice, but the controller only
  reads the name the template produces.
- The controller forces three labels onto every rendered object:
  `app.kubernetes.io/managed-by: <DVC_MANAGED_BY>`,
  `<DVC_VERSION_LABEL>: <version>` and
  `dbos.trustle.com/worker-version-set: <set name>`. It finds the versions it
  owns through them, so a template cannot override them. Write them in the
  template too, for a reader.
- Deleting a WorkerVersionSet does not delete its worker objects. The rendered
  objects carry no ownerReference. Deletion is always the controller's decision,
  gated on zero rows, zero pods and the grace period. To retire an application,
  point `spec.version` at the last version, let the others drain, then delete the
  remaining objects by label. A re-created set with the same name adopts its
  objects.
- Set `DBOS_APPLICATION_VERSION` to `__VERSION__` in the worker container, so
  the executor reports the version its objects are labelled with.
- The ScaledObject needs at least one `postgresql` trigger. The controller reads
  the first trigger and reuses its `type`, its `authenticationRef` and its other
  metadata keys when it builds the drain trigger.

See `examples/workerversionset.yaml` for a complete resource.

### Status

The controller patches the status subresource after every reconcile of a set.
Status is an observation, not an action, so it is written in `DVC_DRY_RUN` too.

```yaml
status:
  observedGeneration: 3
  lastReconcileTime: "2026-09-17T00:00:00Z"
  versions:
    - version: v20260916135248
      phase: Current
      image: registry.example.com/app:8332cd5
      replicas: 1
      outstanding: 0
    - version: v20260901120000
      phase: Drained
      outstanding: 0
      replicas: 0
      drainedSince: "2026-09-16T23:40:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: Reconciled
      message: Reconciled the worker versions
      lastTransitionTime: "2026-09-16T23:40:00Z"
```

- `phase` is `Current` for the desired version, `Draining` for an old version
  KEDA holds at zero, and `Drained` for an old version at zero rows and zero
  pods serving its grace period. `drainedSince` appears only while `Drained`.
- `outstanding` is reported for every version, including `Current`, and is
  absent only when the database was unreachable.
- The `Ready` reason is `Reconciled`, `InvalidSpec`, `DatabaseUnavailable`,
  `PodsUnavailable`, `DrainFailed` or `ReconcileError`. `PodsUnavailable` names
  the versions whose holder pods could not be counted, so nothing was deleted for
  them; `DatabaseUnavailable` takes precedence over it. `DatabaseUnavailable` takes precedence
  over `DrainFailed` when both apply, since it explains any drain failure that
  follows from it.

```console
$ kubectl get wvs -n dbos-workers
NAME          VERSION           IMAGE                              READY   AGE
dbos-worker   v20260916135248   registry.example.com/app:8332cd5   True    6d
```

## Configuration

Every setting reads one environment variable with the `DVC_` prefix. Each setting
also has a flag, and the environment variable wins over the flag. The flag name
is not always the variable name.

| Variable | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `DVC_NAMESPACE` | `--namespace` | required | Namespace holding the WorkerVersionSets and the worker objects. The manager watches only this namespace. |
| `DVC_DATABASE_URL` | `--database-url` | required | URL of the DBOS system database. |
| `DVC_INTERVAL_SECONDS` | `--interval` | `30` | Time between reconciles of one set. |
| `DVC_GRACE_SECONDS` | `--grace` | `600` | Default time a drained version must stay idle before deletion. `spec.graceSeconds` overrides it. |
| `DVC_DRY_RUN` | `--dry-run` | `false` | Log every write as `Would ...` and change nothing. |
| `DVC_MANAGED_BY` | `--managed-by` | `dbos-version-controller` | Value of the `app.kubernetes.io/managed-by` label. |
| `DVC_VERSION_LABEL` | `--version-label` | `dbos-app-version` | Label carrying the application version. |
| `DVC_LOG_LEVEL` | `--log-level` | `info` | Level of the root logger. |

The two duration settings take different units. `DVC_INTERVAL_SECONDS` and
`DVC_GRACE_SECONDS` take a plain number of seconds. `--interval` and `--grace`
take a Go duration, such as `30s` or `10m`.

The controller records the idle time in the annotation
`dbos-version-controller/drained-since`.

The image is distroless. It carries no shell, so every setting must arrive as an
environment variable or a flag. A `secretKeyRef` is the way to pass the database
URL.

## RBAC

`config/rbac/role.yaml` is generated from the markers on the reconciler, and is
the source of truth:

| API group | Resource | Verbs |
| --- | --- | --- |
| `dbos.trustle.com` | `workerversionsets` | get, list, watch |
| `dbos.trustle.com` | `workerversionsets/status` | get, patch, update |
| `apps` | `deployments` | get, list, watch, create, patch, delete |
| `policy` | `poddisruptionbudgets` | get, create, patch, delete |
| `keda.sh` | `scaledobjects` | get, create, patch, delete |
| `""` | `pods` | list |

The pod list is read uncached, through the manager's API reader, so the
controller never watches every pod in the namespace.

`examples/controller.yaml` holds the same rules as one namespaced Role, next to
a ServiceAccount, a RoleBinding and a Deployment.

## Install

The kubebuilder layout under `config/` is the full install:

```bash
make install                                  # the CRD only
make deploy IMG=registry.example.com/dbos-version-controller:0.3.0
```

The smaller namespaced alternative:

```bash
kubectl apply -f crd/
kubectl apply -f examples/controller.yaml
kubectl apply -f examples/workerversionset.yaml
```

The CRD is cluster-scoped, so install it once per cluster. The controller and
its sets are namespaced.

## Safety rules

- The controller never deletes the desired version.
- The controller deletes nothing for a set whose spec is invalid.
- The controller deletes nothing while the database is unreachable. An unknown
  row count keeps every object.
- The controller deletes a version only after zero rows and zero pods for the
  full grace period. Work that returns clears the marker and restarts the clock.
- A pod outside the set that carries the version's label holds the version until
  it reaches a terminal phase, terminating pods included: while it does, the
  version is never marked drained or deleted. A pod list the controller cannot
  read, or a reconciler built with no pod reader, counts as unknown and deletes
  nothing.
- A rollback to a drained version clears its marker, so the version gets a fresh
  grace period rather than an immediate delete.
- A failed drain logs, continues with the remaining versions, and reports
  `Ready=False`/`DrainFailed` naming the versions that failed. A failed set
  logs and reports `ReconcileError` on its own status.
- `DVC_DRY_RUN=true` performs no write to a worker object. Dry run writes two
  things: the WorkerVersionSet status and the drained-since marker annotation.
  Both record what the controller observed. It creates, patches and deletes
  nothing else.
- Run exactly one replica, or enable leader election. Two controllers
  reconciling the same set fight each other. Use `strategy: Recreate`.
- The controller never starts DBOS. It reads `dbos.workflow_status` over one
  short pgx connection per reconcile, so a crashing worker version cannot take
  the controller down.

## Run locally against a kubeconfig

`make run` builds the manager and runs it against your current kubeconfig
context:

```bash
export DVC_NAMESPACE=dbos-workers
export DVC_DATABASE_URL="postgresql://user:pass@localhost:5432/dbos"
export DVC_DRY_RUN=true
make run
```

Start with `DVC_DRY_RUN=true`. Read the `Would create worker object`,
`Would patch worker object` and `Would delete worker object` lines before you
let the controller write.

## Development

```bash
make manifests generate fmt vet   # regenerate the CRD, the RBAC and the deepcopy code
make test                         # envtest suite, and coverage in cover.out
make test-e2e                     # end-to-end suite, against a kind cluster
make lint                         # golangci-lint
make docker-build IMG=...         # the controller image
```

`make test` runs against a real API server that `setup-envtest` downloads. The
suite installs the generated CRD and the KEDA ScaledObject CRD from
`test/crds/`, which is a copy of the ScaledObject document of KEDA 2.17.2. That
copy is a test fixture, and the `.dockerignore` keeps it out of the image.

### The end-to-end suite

`make test-e2e` runs `test/e2e` against a real cluster. It needs Docker and
`kubectl`: the target downloads `kind` itself, creates a cluster, and deletes
it again when the suite ends, whether the suite passed or failed, but the suite
drives the cluster through `kubectl` and does not install it. The cluster keeps
its own kubeconfig under `bin/`, so a run leaves your current context alone.
`make test` excludes this package, because it needs that cluster.

The suite covers what envtest cannot. It builds the image from the Dockerfile
and loads it into the node, deploys `config/default`, and points the controller
at a Postgres carrying `dbos.workflow_status`. It then drives one
WorkerVersionSet through a version change, and takes the three deletion gates
one at a time: the parked version survives on its running pod alone with no
outstanding rows, then on its one outstanding row alone with its pods at zero,
then through the grace period once both are clear — and only then are its
Deployment, ScaledObject and PDB deleted. Isolating them is the point: a suite
that clears two gates at once cannot say which one held.

It installs the KEDA ScaledObject definition, not KEDA. The controller writes
ScaledObjects and never reads what KEDA does with them, so where the suite
needs the effect of KEDA scaling a parked version to zero, it scales the
Deployment itself. The GitHub Actions job in `.github/workflows/e2e.yml` runs
the same `make test-e2e` on every pull request.

`make manifests` also copies the generated CRD to
`crd/dbos.trustle.com_workerversionsets.yaml`, because the Helm chart reads that
path verbatim. Commit both copies.

## Roadmap

- Read the outstanding-workflow signal from a DBOS Conductor autoscale API,
  instead of a direct query against the system database. The controller keeps
  the same loop; only the counter changes.
- Emit Kubernetes Events for each drain and delete.
- Publish Prometheus metrics for live versions and drain age.

## License

Apache-2.0. See `LICENSE`.
