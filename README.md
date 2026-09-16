# dbos-version-controller

A small Kubernetes controller. It runs one worker Deployment per DBOS
application version, and it removes an old version only after that version has
no work left.

Status: built for the London service at Trustle and in rollout there. Private for now.

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

This controller holds the list instead. Git holds one desired
`(version, image)` pair. The controller owns everything else.

## How it works

Every `DVC_INTERVAL_SECONDS` the controller runs one reconcile tick:

1. Read the desired ConfigMap. Take the `version` and `image` keys.
2. Validate both. An invalid value skips the whole tick, and the controller
   deletes nothing.
3. Read the template ConfigMap. Substitute `__VERSION__` and `__IMAGE__` in the
   three manifest templates.
4. Create the desired version's Deployment, ScaledObject and PDB, or JSON
   merge-patch them if they exist. Clear the drained marker on the Deployment.
5. List every Deployment with the label
   `app.kubernetes.io/managed-by=<managed_by>`. Skip the desired version.
6. For each other version, count its `ENQUEUED` and `PENDING` rows in
   `dbos.workflow_status`.
7. Park that version: patch its ScaledObject to `minReplicaCount: 0` with one
   postgresql trigger that counts the same rows. KEDA then scales the version up
   when its own work appears, and back to zero when that work finishes.
8. If the version has zero rows and zero pods, write the drained marker
   annotation with the current time. If work returns, clear the marker.
9. Delete the version's three objects once the marker is older than
   `DVC_GRACE_SECONDS`.

```mermaid
flowchart TD
    A[Read desired ConfigMap] --> B{version and image valid?}
    B -- no --> Z[Skip the tick. Delete nothing.]
    B -- yes --> C[Render templates for the desired version]
    C --> D[Create or patch Deployment, ScaledObject, PDB]
    D --> E[List managed Deployments]
    E --> F{Is this the desired version?}
    F -- yes --> E
    F -- no --> G[Count ENQUEUED + PENDING rows for that version]
    G --> H[Patch ScaledObject: minReplicaCount 0, one outstanding trigger]
    H --> I{rows = 0 and pods = 0?}
    I -- no --> J[Clear the drained marker]
    I -- yes --> K{Marker older than grace?}
    K -- no --> L[Write or keep the marker]
    K -- yes --> M[Delete ScaledObject, PDB, Deployment]
```

## The ConfigMap contract

The controller reads two ConfigMaps in its own namespace. It never writes to
them.

### Desired ConfigMap

Default name `dbos-worker-desired`. Your deploy pipeline writes it.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dbos-worker-desired
  namespace: dbos-workers
data:
  version: "v20260916135248"
  image: "registry.example.com/app:8332cd5"
```

Rules:

- `version` must match `^[a-z0-9][a-z0-9.-]{0,48}$`. The pattern admits no quote
  character, because the version goes into a KEDA query string.
- `image` must match `^[A-Za-z0-9./:_@-]+$`.
- A value outside these patterns skips the tick.

### Template ConfigMap

Default name `dbos-worker-template`. It holds three manifests, one per key.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dbos-worker-template
  namespace: dbos-workers
data:
  deployment.yaml: |
    ...
  scaledobject.yaml: |
    ...
  pdb.yaml: |
    ...
```

Rules for each template:

- Write `__VERSION__` where the application version belongs. Write `__IMAGE__`
  where the image reference belongs. The controller replaces both as plain text.
- Give all three objects the same `metadata.name`. The controller addresses a
  version's Deployment, ScaledObject and PDB by one shared name. A name pattern
  such as `<prefix>-__VERSION__` is the usual choice, but the controller only
  reads the name the template produces.
- Put `dbos-app-version: __VERSION__` and
  `app.kubernetes.io/managed-by: dbos-version-controller` in the labels of all
  three objects. The controller finds the versions it owns through these two
  labels. Change the label names or the managed-by value with
  `DVC_VERSION_LABEL` and `DVC_MANAGED_BY`.
- Set `DBOS_APPLICATION_VERSION` to `__VERSION__` in the worker container, so
  the executor reports the version its objects are labelled with.
- The ScaledObject needs at least one `postgresql` trigger. The controller reads
  the first trigger and reuses its `type`, its `authenticationRef` and its other
  metadata keys when it builds the drain trigger.

See `examples/` for a complete pair.

## Configuration

Every setting reads one environment variable with the `DVC_` prefix.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DVC_NAMESPACE` | required | Namespace holding the worker objects and both ConfigMaps. |
| `DVC_DATABASE_URL` | required | URL of the DBOS system database. |
| `DVC_DESIRED_CONFIGMAP` | `dbos-worker-desired` | ConfigMap with the `version` and `image` keys. |
| `DVC_TEMPLATE_CONFIGMAP` | `dbos-worker-template` | ConfigMap with the three manifest templates. |
| `DVC_INTERVAL_SECONDS` | `30` | Seconds between reconcile ticks. |
| `DVC_GRACE_SECONDS` | `600` | Seconds a drained version must stay idle before deletion. |
| `DVC_DRY_RUN` | `false` | Log every write as `Would ...` and change nothing. |
| `DVC_MANAGED_BY` | `dbos-version-controller` | Value of the `app.kubernetes.io/managed-by` label. |
| `DVC_VERSION_LABEL` | `dbos-app-version` | Label carrying the application version. |
| `DVC_LOG_LEVEL` | `INFO` | Level of the root logger. |

The controller records the idle time in the annotation
`dbos-version-controller/drained-since`.

## RBAC

The controller needs one namespaced Role:

| API group | Resource | Verbs |
| --- | --- | --- |
| `""` | `configmaps` | get, list, watch |
| `""` | `pods` | get, list |
| `apps` | `deployments` | get, list, watch, create, patch, delete |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, patch, delete |
| `keda.sh` | `scaledobjects` | get, list, watch, create, patch, delete |

`examples/controller.yaml` holds the ServiceAccount, Role, RoleBinding and
Deployment.

## Safety rules

- The controller never deletes the desired version.
- The controller deletes nothing while the desired ConfigMap is invalid.
- The controller deletes nothing while the database is unreachable. An unknown
  row count keeps every object.
- The controller deletes a version only after zero rows and zero pods for the
  full grace period. Work that returns clears the marker and restarts the clock.
- A rollback to a drained version clears its marker, so the version gets a fresh
  grace period rather than an immediate delete.
- A failed drain logs and continues with the remaining versions.
- `DVC_DRY_RUN=true` performs no write at all.
- Run exactly one replica. Two controllers reconciling the same desired state
  fight each other. Use `strategy: Recreate`.
- The controller never starts DBOS. It reads `dbos.workflow_status` with
  psycopg, so a crashing worker version cannot take the controller down.

## Run locally against a kubeconfig

The controller loads in-cluster credentials first. It falls back to
`load_kube_config`, so a local run uses your current kubeconfig context.

```bash
uv sync
export DVC_NAMESPACE=dbos-workers
export DVC_DATABASE_URL="postgresql://user:pass@localhost:5432/dbos"
export DVC_DRY_RUN=true
uv run python -m dbos_version_controller
```

Start with `DVC_DRY_RUN=true`. Read the `Would create`, `Would patch` and
`Would delete` lines before you let the controller write.

## Development

```bash
uv sync
uv run ruff check .
uv run ruff format --check .
uv run pyright
uv run pytest -q
```

## Roadmap

- Read the outstanding-workflow signal from a DBOS Conductor autoscale API,
  instead of a direct query against the system database. The controller keeps
  the same loop; only the counter changes.
- Emit Kubernetes Events for each drain and delete.
- Publish Prometheus metrics for live versions and drain age.

## License

Apache-2.0. See `LICENSE`.
