# Flux

Flux has no pre-sync hooks, so gate where the change enters Git:

1. **On GitOps-repository pull requests**, run `changeward k8s gate --manifests <rendered dir> --target <cluster>
   --executor $CHANGEWARD_EXECUTOR_URL` in CI and make it a required status check. The plan is computed against the
   live cluster, and approvals bind to the exact plan hash.
2. **During incidents**, suspend the Kustomization before a direct change (`flux suspend kustomization <name>`) so
   Flux does not revert it. Make the change through Changeward (MCP, `changeward exec --target <cluster> ...`).
   Then open a reconciliation PR (`changeward reconcile --k8s-dir ... --pr`) and resume (`flux resume`).
   `changeward drift --max-age 24h` reports direct changes that have not been reconciled.

The Kubernetes adapter detects Flux ownership (`kustomize.toolkit.fluxcd.io/name`, `helm.toolkit.fluxcd.io/name`
labels) and warns in every plan that touches a Flux-managed object.
