# Argo CD

`presync-gate.yaml` is a PreSync hook Job. Before Argo CD syncs, it runs `changegate k8s gate` on the shared
executor:

- The rendered manifests are planned against the live cluster (server-side dry run, managed fields stripped).
  The plan records which objects are created or changed, and flags objects other controllers manage.
- Policy decides in the CI lane. Auto-approved syncs proceed. Others fail the hook with "approval required"
  until someone other than the proposer approves that exact plan hash (`changegate approve`, Slack, web UI).
  Argo CD's retry then passes.
- Changed manifests or a changed live state produce a new plan, so a stale approval never carries over.

Rendered manifests are needed: with Helm or Kustomize, render in CI (`helm template`, `kustomize build`) and gate the
output, or point `MANIFESTS_PATH` at a pre-rendered directory. On GitOps-repository PRs, run the same command in CI so
reviewers see the plan before merge.

Direct changes to Argo-managed objects, for example during an incident, are flagged by the Kubernetes adapter
("managed by argocd"). Bring Git back in line afterwards with `changegate reconcile --k8s-dir ... --pr`.
