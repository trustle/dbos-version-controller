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
