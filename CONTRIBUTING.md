# Contributing

- Open an issue before a large change. Small fixes can go straight to a pull request.
- Run `make manifests generate` after any change to `api/v1alpha1/`, and commit both CRD copies.
- Run the full check set before you push: `go vet ./... && make test && make lint`.
- Add a test for every behaviour change. The reconcile loop deletes cluster objects, so an untested branch is a risk.
- Keep the change small. Touch only what the change requires.
- Write commit messages in the imperative mood.
- Record user-visible changes in `CHANGELOG.md`.
- The project targets the Go version in `go.mod`.
- The project is licensed Apache-2.0. A contribution carries the same license.
- Report a vulnerability through GitHub's private vulnerability reporting, not through an issue. See `SECURITY.md`.

## What you need

- The Go toolchain, at the version in `go.mod`.
- GNU Make.
- Docker, for `make docker-build` and `make test-e2e`.
- Helm 3, for `make chart-lint` and `make test-e2e`. CI uses v3.18.4, the version Argo CD v3.1.8 renders with.

`make` downloads every other tool it needs into `bin/`, which is gitignored.
That includes `controller-gen`, `kustomize`, `golangci-lint` and
`setup-envtest`. The first run of a target needs network access.

## Running the tests

```bash
make test   # envtest suite, and coverage in cover.out
make lint   # golangci-lint, with the plugins in .custom-gcl.yml
```

`make test` runs against a real API server. `setup-envtest` downloads the
`kube-apiserver` and `etcd` binaries for the Kubernetes version derived from
`k8s.io/api` in `go.mod`, and `make test` puts their path in
`KUBEBUILDER_ASSETS`. No cluster and no database are needed: the suite installs
the generated CRD and the KEDA ScaledObject CRD from `test/crds/`, and it
replaces the outstanding-workflow counter with a stub.

`make lint` builds a custom `golangci-lint` binary, because `.custom-gcl.yml`
adds the `logcheck` plugin. The first build takes a few minutes.

## Exercising a change against a cluster

The envtest suite does not run the controller's image, and it does not read a
real DBOS system database. Run the controller against a cluster of your own
when a change affects reconcile behaviour:

```bash
export DVC_NAMESPACE=dbos-workers
export DVC_DATABASE_URL="postgresql://user:pass@localhost:5432/dbos"
export DVC_DRY_RUN=true
make run
```

Start in dry run. Read the `Would create worker object`, `Would patch worker
object` and `Would delete worker object` lines before you let the controller
write.
