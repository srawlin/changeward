# MCP gateways: Envoy, agentgateway, IBM ContextForge

If your agents reach MCP servers through a gateway, the gateway can ask the Changegate executor about **every
tool call**:

- reads pass;
- SQL writes to database tools are blocked, with instructions to use Changegate's own tools (preview and undo);
- tools you declare read-only pass and are audited;
- every other tool call is decided by policy, as an approval-only change, and audited (`gateway.*` events).

The executor never changes anything on the gateway path; it only says allow or deny. Gateways can't preview or
undo. For that, route agents to Changegate's MCP server behind the gateway, and keep the gateway check as the
backstop.

**Verified:**

- Envoy 1.36 and agentgateway v1.6.0, in front of the reference "everything" MCP server, driven by the official
  Python MCP client.
- ContextForge 1.0.11 with the plugin in enforce mode.

In each, a declared read-only tool ran, and another tool was blocked with Changegate's reason shown to the client.

## Endpoints

`/v1/authz/mcp` takes the raw MCP JSON-RPC request. It accepts any method: Envoy forwards the client's own, and
MCP clients also send `GET` (SSE) and `DELETE`.

- **Allow:** 200.
- **Deny:** 403. The body is a JSON-RPC error for the call (`code -32001`, message `Changegate: <reason>`), so
  the agent sees why.

| Header (set by the gateway) | Meaning |
| --- | --- |
| `x-changegate-token` | An executor token |
| `x-changegate-server` | Logical upstream name, for records and policy (`change.target` is `mcp:<server>`) |
| `x-changegate-env` | Environment class of the upstream (default `prod`) |
| `x-changegate-agent` | Agent identity, if the gateway knows it |
| `x-changegate-read-only-tools` | Comma-separated globs, e.g. `list_*,get_*,describe_*` |

These headers must come from the gateway's configuration, never from the client. Envoy and agentgateway forward
only `Authorization` (plus Host, Method and Path) from the client by default.

`POST /v1/authz/tool` takes `{server, env, agent, tool, arguments}` for plugin-style gateways.

## Envoy

[`integrations/envoy/ext_authz.yaml`](../integrations/envoy/ext_authz.yaml) has the `http_filters` block:

```yaml
- name: envoy.filters.http.ext_authz
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz
    transport_api_version: V3
    failure_mode_allow: false                 # fail closed
    with_request_body: { max_request_bytes: 1048576, allow_partial_message: false }
    http_service:
      server_uri: { uri: http://changegate-executor.changegate.svc:8443, cluster: changegate_executor, timeout: 2s }
      path_prefix: /v1/authz/mcp
      authorization_request:
        headers_to_add:
          - { key: x-changegate-token, value: "%ENVIRONMENT(CHANGEGATE_GATEWAY_TOKEN)%" }
          - { key: x-changegate-server, value: postgres-mcp }
          - { key: x-changegate-env, value: prod }
          - { key: x-changegate-read-only-tools, value: "query,list_*,get_*,describe_*" }
      authorization_response:
        allowed_client_headers:
          patterns: [{ exact: x-changegate-decision }, { exact: content-type }]
```

If the cluster uses `STRICT_DNS` to reach `host.docker.internal`, set `dns_lookup_family: V4_ONLY`.

## agentgateway

See [`integrations/agentgateway/README.md`](../integrations/agentgateway/README.md) for the verified v1.6 config.
In short: under `mcp.policies.extAuthz`, set `protocol.http.path: '"/v1/authz/mcp"'`,
`includeRequestBody`, `failureMode: deny`, and `addRequestHeaders` for the `x-changegate-*` headers. The values
are CEL, so string literals need inner quotes.

## IBM ContextForge

Install the plugin from [`integrations/contextforge`](../integrations/contextforge) into the gateway's Python
environment (on `PYTHONPATH`). Then add it to `plugins/config.yaml` and start ContextForge with
`PLUGINS_ENABLED=true`:

```yaml
plugins:
  - name: "Changegate"
    kind: "changegate_contextforge.plugin.ChangegatePlugin"
    hooks: ["tool_pre_invoke"]
    mode: "enforce"
    priority: 10
    config:
      executor_url: "http://changegate-executor.changegate.svc:8443"
      token_env: CHANGEGATE_GATEWAY_TOKEN
      env: prod
      fail_closed: true
      read_only_tools: ["*-list-*", "*-get-*"]
```

The plugin works with ContextForge 1.x (the `cpex` plugin framework) and 0.x. Blocked calls return a
`Plugin Violation` with Changegate's reason.
