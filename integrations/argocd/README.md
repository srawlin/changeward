# Argo CD

`presync-gate.yaml` is a PreSync hook Job. Before Argo CD syncs, it runs `changeward k8s gate` on the shared
executor:

- The rendered manifests are planned against the live cluster (server-side dry run, managed fields stripped).
  The plan records which objects are created or changed, and flags objects other controllers manage.
- Policy decides in the CI lane. Auto-approved syncs proceed. Others fail the hook with "approval required"
  until someone other than the proposer approves that exact plan hash (`changeward approve`, Slack, web UI).
  Argo CD's retry then passes.
- Changed manifests or a changed live state produce a new plan, so a stale approval never carries over.

Rendered manifests are needed: with Helm or Kustomize, render in CI (`helm template`, `kustomize build`) and gate the
output, or point `MANIFESTS_PATH` at a pre-rendered directory. On GitOps-repository PRs, run the same command in CI so
reviewers see the plan before merge.

Direct changes to Argo-managed objects, for example during an incident, are flagged by the Kubernetes adapter
("managed by argocd"). Bring Git back in line afterwards with `changeward reconcile --k8s-dir ... --pr`.

## Verified end to end

Tested with Argo CD v3.5.5 in kind, with the executor deployed in-cluster from `deploy/helm/changeward-executor`
(`rbac.kubernetes.enabled=true`, `namespaces: [shop]`). Its policy required SRE approval for production
Kubernetes syncs. The GitOps repository was a private GitLab project.

1. A new ConfigMap went through `builtin/ci-auto` (reversible, dry-run verified) and synced.
2. A Git change to a feature flag failed the PreSync hook with the exact diff and "approval required".
   `CHANGEWARD_EXECUTOR_URL=… CHANGEWARD_EXECUTOR_TOKEN=<approve-role token> changeward approve c_…` showed
   the plan and approved it. Argo CD's next retry passed, and the new value was live.
3. The gate's own token could not approve (it has only the propose role).

Notes:

- The hook Job is part of the app's manifests. `changeward k8s gate` drops Argo CD and Helm hook resources
  from the plan, so the Job's `generateName` doesn't make every plan unique.
- For a private repository, clone with a read-only token from the hook's Secret, e.g.
  `https://oauth2:${GIT_TOKEN}@${REPO_URL#https://}`.
- Give the application `syncPolicy.retry` with enough attempts for a human to approve.
