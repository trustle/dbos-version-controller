## Description
<!-- Context → Changes → Result. Context: a short narrative for a reviewer with no prior knowledge.
     Changes: what and why, not a file-by-file diff. Result: what is now true, risks, limits. -->

## Testing
- [ ] `make test` passes (envtest)
- [ ] Exercised against a real cluster where the change affects reconcile behaviour

## Checklist
- [ ] CRD changes: `make manifests` run and `crd/` copy refreshed
- [ ] README and CHANGELOG updated where behaviour or configuration changed
