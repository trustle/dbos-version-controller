# Security policy

## Report a vulnerability

Report a vulnerability through GitHub's private vulnerability reporting. Open
the repository's **Security** tab, select **Report a vulnerability**, and
describe the problem there. The report stays private to the maintainers until a
fix is published.

Do not open a public issue or a pull request for a vulnerability. A public
report tells every operator of the controller about the problem before a fix
exists.

Include what the report needs to be reproducible:

- The controller version, and the Kubernetes version it runs on.
- The settings that matter, with the database URL removed.
- The steps that reproduce the problem, and what the controller did.

Expect a first response within five working days.

## Supported versions

The controller's own releases are semantic versions, listed in `CHANGELOG.md`.
Do not confuse them with the DBOS application versions a WorkerVersionSet
carries, which are the deploying application's own strings.

The latest minor release receives security fixes. An older minor release does
not. Upgrade to the latest minor release before you report a problem, where the
upgrade is possible.

| Version | Supported |
| --- | --- |
| Latest minor release | Yes |
| Every earlier release | No |

## Scope

The controller reads the DBOS system database and writes Deployments,
ScaledObjects and PodDisruptionBudgets in one namespace. A report is in scope
when it shows one of these:

- The controller deletes a worker version that still holds work.
- A WorkerVersionSet spec makes the controller act outside `DVC_NAMESPACE`, or
  outside the RBAC rules in `config/rbac/role.yaml`.
- A template value reaches the KEDA query string, the database, or a manifest
  in a form the controller does not validate.
- The controller writes the database URL, or another credential, to its logs or
  to a resource status.

A misconfiguration of the cluster running the controller is out of scope. So is
a vulnerability in KEDA, in DBOS or in Kubernetes itself; report those to their
own maintainers.
