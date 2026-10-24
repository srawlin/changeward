# Development

```sh
make pg            # Postgres 18 (:55418) and 16 (:55416) in Docker for integration tests
make test          # go test ./...  (integration tests run when CW_TEST_PG18 / CW_TEST_PG16 etc. are set)
make lint          # golangci-lint
make build         # bin/changeward
make proto         # regenerate gen/ from proto/
scripts/gen-cli-docs.sh   # regenerate docs/cli.md
changeward adapter test postgres --dsn postgres://postgres:pg@localhost:55418/postgres   # adapter conformance suite
```

`pg_query_go` uses cgo, so release binaries are built natively per platform (`.github/workflows/release.yml`).
Linux binaries for Alpine and GitLab are built with the `Dockerfile` (`--target build`, musl). CI
(`.github/workflows/ci.yml`) runs against Postgres 16/18, MySQL 8.4, a kind cluster, Terraform and NGINX.

## Code map

```
proto/changeward/v1/   Change record and adapter service (protobuf); gen/ holds the generated Go
spec/                  Human-readable spec and JSON Schema
cmd/changeward/        CLI
pkg/adapter/           Adapter SDK: types, plan hashing, go-plugin gRPC transport
conformance/           Adapter conformance suite (adapters/toykv is the reference adapter)
adapters/              postgres, mysql, kubernetes, terraform, configfile, httpapi
internal/engine        State machine and changesets      internal/store     SQLite and the hash-chained audit log
internal/policy        CEL policy and built-in defaults  internal/session   Sessions, dependency-aware undo, rewind, branches
internal/backend       Local and executor backends       internal/executor  Shared executor HTTP API, gateway authz
internal/mcpserver     MCP server                        internal/mcpproxy  MCP proxy
internal/hooks         Coding-agent hook decisions       internal/migrations Framework detection and writers
internal/lanea         Rehearsal report, gate, provenance, GitHub/GitLab posting and approvals
internal/gitops        Kubernetes GitOps gate            internal/telemetry OpenTelemetry export
internal/approval      Slack approvals                   internal/signals   Jev signals
internal/webui         `changeward ui`
integrations/          GitHub Action, GitLab component, Claude Code plugin, OpenCode, Argo CD, Flux, Envoy,
                       agentgateway, ContextForge
sdk/python, sdk/typescript   Executor SDKs
deploy/                Helm chart and systemd unit
packaging/             npm (npx) and PyPI (uvx) launchers, Homebrew formula
demo/, bench/          Replit-style demo, unsafe-action benchmark
```

Status by milestone: [M0/M1](m0-m1-status.md), [M2](m2-status.md). Design: [agent-change-control-plan.md](agent-change-control-plan.md).
