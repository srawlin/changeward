# Shared executor

On a laptop, the MCP server runs the engine in-process with your own credentials. For anything shared, run an
**executor** next to the systems instead. Teams want this for three reasons:

- **Agents never hold credentials.** The executor has the DSNs and kubeconfigs. Agents, SDKs and CI gates send
  it proposals.
- **Policy is enforced where it can't be bypassed.** A developer can't edit the policy file on the executor or
  approve their own change.
- **One audit log** for the whole team, exported to OpenTelemetry if you like.

## Run it

```sh
changegate executor token alice --roles propose     # prints the token once, and the config entry to add
changegate executor token sre-oncall --roles approve
changegate executor serve --addr 0.0.0.0:8443 --tls-cert cert.pem --tls-key key.pem
```

`changegate.yaml` on the executor:

```yaml
targets:
  app:  { adapter: postgres, env: prod, dsn: "${APP_DATABASE_URL}" }
  cluster: { adapter: kubernetes, env: prod, options: { namespace: shop } }
executor:
  tokens:
    - { name: alice,      sha256: "…", roles: [propose] }
    - { name: sre-oncall, sha256: "…", roles: [approve] }
    - { name: argocd-gate, sha256: "…", roles: [propose] }
```

Only the SHA-256 of each token is stored. A token's **name** is the delegating human or service, recorded as
`user:<name>`. Roles:

| Role | Can |
| --- | --- |
| `propose` | Start sessions, propose, apply what policy allows, undo, query, checkpoint, confirm inline approvals on its own changes, run the GitOps gate and gateway authz |
| `approve` | Read changes and approve or reject out-of-band, but never its own changes (separation of duties) |
| `admin` | Everything |

## Clients

```sh
# MCP server for a coding agent, backed by the executor:
changegate mcp postgres --target app --executor https://changegate.internal:8443   # token in $CHANGEGATE_EXECUTOR_TOKEN

# Approvals:
CHANGEGATE_EXECUTOR_URL=https://changegate.internal:8443 CHANGEGATE_EXECUTOR_TOKEN=$SRE_TOKEN changegate approve c_…

# GitOps gate:
changegate k8s gate --manifests rendered/ --target cluster --executor https://changegate.internal:8443
```

[Python and TypeScript SDKs](agents/sdks.md) call the same API.

## Helm

```sh
helm install changegate deploy/helm/changegate-executor -n changegate --create-namespace -f values.yaml
```

```yaml
image: { repository: ghcr.io/srawlin/changegate, tag: latest }
existingSecret: changegate-env           # env vars referenced as ${VAR} in config (DSNs, Slack tokens)
persistence: { enabled: true, size: 1Gi } # SQLite audit log; one replica
rbac:
  kubernetes:                            # for kubernetes targets, using the in-cluster service account
    enabled: true
    namespaces: [shop, payments]         # or clusterWide: true
config: |
  targets:
    app: { adapter: postgres, env: prod, dsn: "${APP_DATABASE_URL}" }
    cluster: { adapter: kubernetes, env: prod }
  executor:
    tokens: [ … ]
policy: |                                # optional organization policy
  version: 1
  rules:
    - name: prod-k8s-needs-sre
      when: change.lane == "ci" && change.env == "prod" && change.adapter == "kubernetes"
      decide: require
      channel: out-of-band
      approvers: ["group:sre"]
```

The pod runs as non-root with a read-only root filesystem. Liveness and readiness use `/healthz`. The chart was
deployed in kind and used by the Python SDK and the Argo CD PreSync gate.

## systemd

[`deploy/systemd/changegate-executor.service`](../deploy/systemd/changegate-executor.service) runs it as a
`changegate` user, with `/etc/changegate/changegate.yaml` and secrets in `/etc/changegate/env`.

## API

All routes need `Authorization: Bearer <token>`. Gateways may send `X-Changegate-Token` instead and keep the
client's own `Authorization` header.

| Route | Role |
| --- | --- |
| `POST /v1/sessions`, `GET /v1/sessions/{id}`, `POST /v1/sessions/{id}/end` | propose |
| `GET /v1/sessions/{id}/changes`, `POST /v1/sessions/{id}/checkpoint`, `POST /v1/sessions/{id}/query` | propose |
| `POST /v1/changes` (propose), `POST /v1/changes/{ref}/apply`, `POST /v1/changes/{ref}/undo` | propose |
| `POST /v1/changes/{ref}/approve-inline` | propose (only where policy allows inline) |
| `GET /v1/changes/{ref}` | propose or approve |
| `POST /v1/changes/{ref}/approve` | approve |
| `POST /v1/gitops/gate` | propose |
| `/v1/authz/mcp`, `POST /v1/authz/tool` | any valid token ([gateways.md](gateways.md)) |
| `GET /healthz` | none |

**Limitation:** the store is SQLite, so run a single replica. A Postgres-backed store for high availability is
planned.
