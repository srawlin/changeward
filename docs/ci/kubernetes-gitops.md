# Kubernetes, Argo CD and Flux

`changegate k8s gate` plans a set of rendered manifests against the **live cluster**, using a server-side dry run
with managed fields and status stripped. Policy then decides. The command exits 0 only if that exact plan is
approved.

You can run it in three places:

- as a **PR check** on your GitOps repository;
- as an **Argo CD PreSync hook**, so a sync waits until its plan is approved;
- **locally** before a manual rollout.

```console
$ changegate k8s gate --manifests apps/shop --target cluster
skipped 1 hook resource(s) (Argo CD / Helm hooks are not desired state)
plan p_f88610636592 · cluster (prod) · reversible · 1 objects · server-side dry run
~ shop/ConfigMap/shop-config (update)
     data.feature_new_cart: "false" → "true"
change c_9dd33d8939b8 (#3) — awaiting-approval
policy: approval required (out-of-band; approvers: group:sre): production Kubernetes syncs need SRE approval
changegate: gate: approval required; approve with `changegate approve p_f88610636592` (or Slack/web), then retry the sync
```

- Approvals bind to the plan hash.
- A retried gate with the same manifests and the same live state reuses the decision, so it passes once
  approved.
- A different commit, or a cluster that drifted, needs a new approval.

Pass `--executor URL` (token in `$CHANGEGATE_EXECUTOR_TOKEN`) to run the gate on a shared executor. The executor
then holds the cluster credentials and the approvals. `--out plan.txt` writes the diff for a PR comment.

## Target

```sh
changegate connect cluster --adapter kubernetes --env prod --option context=prod-eu --option namespace=shop
```

Options: `context`, `kubeconfig`, `namespace`, `kubectl` (binary path) and `rollout_timeout`. Inside a cluster
the executor uses its service account. The [Helm chart](../executor.md#helm) can create one with namespaced or
cluster-wide RBAC (`rbac.kubernetes.enabled`).

Manifests must be rendered YAML. For Helm or Kustomize, render first (`helm template`, `kustomize build`).

## Argo CD (PreSync hook)

**Verified** with Argo CD v3.5.5 in kind, using the in-cluster executor and a private GitLab repository:

1. A reversible create was auto-approved.
2. A feature-flag change failed the hook with the exact diff.
3. An SRE ran `changegate approve --executor` and saw the plan before approving.
4. Argo's retry passed and the change was live.
5. The gate's own token couldn't approve.

Add [`integrations/argocd/presync-gate.yaml`](../../integrations/argocd/presync-gate.yaml) to the app's
manifests:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  generateName: changegate-gate-
  annotations:
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: gate
          image: ghcr.io/srawlin/changegate:latest
          env:
            - { name: REPO_URL, value: "https://github.com/your-org/gitops.git" }
            - { name: REVISION, value: "main" }
            - { name: MANIFESTS_PATH, value: "apps/shop" }
            - { name: CHANGEGATE_EXECUTOR_URL, value: "http://changegate-executor.changegate.svc:8443" }
            - name: CHANGEGATE_EXECUTOR_TOKEN
              valueFrom: { secretKeyRef: { name: changegate-gate-token, key: token } }
          command: ["sh", "-c"]
          args:
            - git clone --quiet --depth 1 --branch "$REVISION" "$REPO_URL" /tmp/repo &&
              changegate k8s gate --manifests "/tmp/repo/$MANIFESTS_PATH" --target cluster
```

Then:

- **Retries.** Give the Application `syncPolicy.retry` with enough attempts and backoff for a human to approve.
- **Tokens.** Create two executor tokens:
  - one for the gate (`--roles propose`), stored in the `changegate-gate-token` Secret;
  - one per approver (`--roles approve`).

  Approve with:

  ```sh
  CHANGEGATE_EXECUTOR_URL=https://changegate.internal:8443 CHANGEGATE_EXECUTOR_TOKEN=<approver token> \
    changegate approve c_9dd33d8939b8
  ```

  The approval can also come from Slack or the web view.
- **Private repositories.** Clone with a read-only token from the Secret:
  `https://oauth2:${GIT_TOKEN}@${REPO_URL#https://}`.
- **Hook resources.** The gate drops Argo CD and Helm hook resources from the plan, including its own Job.

## Flux

Flux has no pre-sync hook, so gate where changes enter Git:

1. **GitOps PRs.** Run `changegate k8s gate --manifests <rendered> --target <cluster> --executor …` in CI, and
   make it a required check. The plan is computed against the live cluster.
2. **Incidents.** `flux suspend kustomization <name>`, then change through Changegate, then
   `changegate reconcile --k8s-dir … --pr` to bring Git in line, then `flux resume`.

The adapter flags objects owned by Argo CD or Flux in direct-lane plans: "a direct change will be reverted on the
next sync". `changegate drift --max-age 24h` exits 3 if direct changes stay unreconciled.

## GitHub Actions example (PR check)

```yaml
  k8s-gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: kustomize build overlays/prod > rendered.yaml
      - run: changegate k8s gate --manifests rendered.yaml --target cluster --executor "$CHANGEGATE_EXECUTOR_URL" --out plan.txt
        env:
          CHANGEGATE_EXECUTOR_URL: ${{ vars.CHANGEGATE_EXECUTOR_URL }}
          CHANGEGATE_EXECUTOR_TOKEN: ${{ secrets.CHANGEGATE_GATE_TOKEN }}
```

The executor must be reachable from the runner, e.g. through a self-hosted runner in the cluster's network.
