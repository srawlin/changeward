package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/srawlin/changegate/adapters/mysql"
	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/pkg/adapter"
)

// SQLTools are MCP tool names whose argument is SQL (popular database servers).
var SQLTools = map[string]bool{"execute_sql": true, "query": true, "run_sql": true, "sql": true, "execute_query": true, "run_query": true, "postgres_query": true, "pg_query": true, "execute": true}

var sqlArgs = []string{"sql", "query", "statement", "sql_query", "command"}

// AuthzDecision is the response body of the authorization endpoints.
type AuthzDecision struct {
	Allow    bool   `json:"allow"`
	Reason   string `json:"reason"`
	ChangeID string `json:"change_id,omitempty"`
	PlanID   string `json:"plan_id,omitempty"`
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

// authzMCP decides one forwarded MCP request (agentgateway extAuthz, Envoy
// ext_authz HTTP, or any gateway that can call out). Headers:
//
//	x-changegate-server  logical name of the upstream MCP server (default "mcp")
//	x-changegate-env     environment class of the upstream (default "prod")
//	x-changegate-agent   agent identity, if the gateway knows it
//	x-changegate-read-only-tools  comma-separated globs of tools that only read (allowed and audited)
func (s *Server) authzMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("x-changegate-decision", "allow")
		writeJSON(w, http.StatusOK, AuthzDecision{Allow: true, Reason: "not a tool call"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusForbidden, AuthzDecision{Reason: "unreadable request"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method != "tools/call" {
		writeJSON(w, http.StatusOK, AuthzDecision{Allow: true, Reason: "not a tool call"})
		return
	}
	d := s.decideTool(r, header(r, "x-changegate-server", "mcp"), header(r, "x-changegate-env", adapter.EnvProd),
		header(r, "x-changegate-agent", "agent:via-gateway"), req.Params.Name, req.Params.Arguments)
	w.Header().Set("x-changegate-decision", map[bool]string{true: "allow", false: "deny"}[d.Allow])
	if d.Allow {
		writeJSON(w, http.StatusOK, d)
		return
	}
	// Gateways relay a denial's body to the MCP client, so it is also a
	// JSON-RPC error for the call: the agent sees why it was blocked.
	writeJSON(w, http.StatusForbidden, rpcDenial{JSONRPC: "2.0", ID: req.ID,
		Error: rpcError{Code: -32001, Message: "Changegate: " + d.Reason, Data: d}, AuthzDecision: d})
}

type rpcError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    AuthzDecision `json:"data"`
}

// rpcDenial is a denial that is both a JSON-RPC error and an AuthzDecision.
type rpcDenial struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Error   rpcError        `json:"error"`
	AuthzDecision
}

// authzTool is the same decision for plugin-style gateways (e.g. ContextForge)
// that send the tool call as JSON: {server, env, agent, tool, arguments}.
func (s *Server) authzTool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server    string         `json:"server"`
		Env       string         `json:"env"`
		Agent     string         `json:"agent"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorBody{Error: err.Error()})
		return
	}
	if in.Server == "" {
		in.Server = "mcp"
	}
	if in.Env == "" {
		in.Env = adapter.EnvProd
	}
	if in.Agent == "" {
		in.Agent = "agent:via-gateway"
	}
	writeJSON(w, http.StatusOK, s.decideTool(r, in.Server, in.Env, in.Agent, in.Tool, in.Arguments))
}

func header(r *http.Request, k, def string) string {
	if v := r.Header.Get(k); v != "" {
		return v
	}
	return def
}

func (s *Server) decideTool(r *http.Request, server, env, agent, tool string, args map[string]any) AuthzDecision {
	if !adapter.ValidEnv(env) {
		return AuthzDecision{Reason: "invalid environment class " + env}
	}
	if !strings.HasPrefix(agent, "agent:") {
		agent = "agent:" + agent
	}
	base := tool
	if i := strings.LastIndexAny(tool, "._"); i >= 0 && !SQLTools[tool] {
		base = tool[i+1:]
	}
	if SQLTools[tool] || SQLTools[base] {
		for _, a := range sqlArgs {
			sql, ok := args[a].(string)
			if !ok {
				continue
			}
			readOnly := false
			if _, st, err := postgres.Classify(sql); err == nil {
				readOnly = allRead(st)
			} else if _, ms, merr := mysql.Classify(sql); merr == nil {
				readOnly = true
				for _, x := range ms {
					if x.Access != adapter.Read {
						readOnly = false
					}
				}
			}
			if readOnly {
				return AuthzDecision{Allow: true, Reason: "read-only SQL"}
			}
			return AuthzDecision{Reason: "SQL writes must go through Changegate (propose_change → apply_change) for a preview, policy check and undo; this direct call was blocked"}
		}
	}
	argsJSON, _ := json.Marshal(args)
	if readOnlyTool(r.Header.Get("x-changegate-read-only-tools"), tool) {
		_ = s.Engine.Store.AppendEvent(r.Context(), &model.Event{Type: "gateway.read", Actor: agent,
			Reason: fmt.Sprintf("%s on %s (%s): declared read-only by the gateway", tool, server, env), Data: argsJSON})
		return AuthzDecision{Allow: true, Reason: "read-only tool"}
	}
	// Approval-only handling for other tools: record a decision, never apply here.
	t := adapter.Target{Name: "mcp:" + server, Adapter: "mcp-gateway", Env: env}
	plan := &adapter.Plan{Adapter: "mcp-gateway", Target: t.Name, Env: env, Reversibility: adapter.Irreversible, Destructive: true, BlastRows: 1,
		Operation: adapter.Operation{Kind: "mcp-tool", Payload: string(argsJSON), Params: map[string]string{"tool": tool}}, RehearsalTier: "static",
		Classification: adapter.Classification{Access: adapter.Destructive, Objects: []string{tool}}}
	d := s.Engine.Policy.Decide(policy.Input{
		Change:    policy.ChangeInput(plan, model.LaneDirect, false, map[string]any{"tool": tool, "server": server}),
		Principal: map[string]any{"kind": "agent", "agent": agent, "human": identity(caller(r))},
		Session:   map[string]any{"incident_ref": "", "has_checkpoint": false, "spend_usd": 0.0},
	})
	_ = s.Engine.Store.AppendEvent(r.Context(), &model.Event{Type: "gateway." + d.Decision, Actor: agent, Reason: fmt.Sprintf("%s on %s (%s): %s", tool, server, env, strings.Join(d.Reasons, "; ")),
		Data: argsJSON})
	if d.Decision == policy.Approve {
		return AuthzDecision{Allow: true, Reason: "allowed by policy (" + strings.Join(d.Rules, ", ") + ")"}
	}
	return AuthzDecision{Reason: fmt.Sprintf("%s requires %s: %s", tool, map[string]string{"deny": "a policy exception", "require": "approval"}[d.Decision], strings.Join(d.Reasons, "; "))}
}

// readOnlyTool matches tool against the gateway operator's comma-separated
// glob list (x-changegate-read-only-tools, e.g. "echo,get-*,list_*"). The
// gateway sets this header itself; it must not forward a client's copy.
func readOnlyTool(globs, tool string) bool {
	for _, g := range strings.Split(globs, ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if ok, _ := path.Match(g, tool); ok {
			return true
		}
	}
	return false
}

func allRead(st []*postgres.Stmt) bool {
	for _, x := range st {
		if x.Access != adapter.Read {
			return false
		}
	}
	return len(st) > 0
}
