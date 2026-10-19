// Package mcpproxy wraps any MCP server (`changegate proxy -- <command>`).
//
//   - Tools a known adapter understands (e.g. a third-party Postgres server's
//     execute_sql) are rewritten: reads pass through to the upstream server;
//     writes become plan → approve → apply through Changegate's executor,
//     which holds its own credentials for the target.
//   - Unknown tools are classified by their MCP annotations. Read-only tools
//     pass through. Others get approval-only handling: they are recorded as
//     irreversible changes (no preview is possible), decided by policy, and
//     forwarded upstream only once approved. Every call is logged.
package mcpproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/mcpserver"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/pkg/adapter"
)

// DefaultSQLTools are tool names of popular database MCP servers whose
// argument is SQL (Postgres MCP Pro, MCP Toolbox, DBHub, Supabase MCP and
// the reference Postgres server).
var DefaultSQLTools = []string{"execute_sql", "query", "run_sql", "sql", "execute_query", "run_query", "postgres_query", "pg_query", "execute"}

var sqlArgNames = []string{"sql", "query", "statement", "sql_query", "command"}

// Proxy is an MCP proxy in front of one upstream server.
type Proxy struct {
	Inner   *mcpserver.Server // Changegate tools and the SQL target ("" target disables SQL rewriting)
	Command []string          // upstream server command
	// SQLTools overrides DefaultSQLTools.
	SQLTools []string
	// Env is the environment class for unknown upstream tools.
	Env string
	// Name identifies the upstream in change records ("mcp:<name>").
	Name    string
	Version string

	upstream *mcp.ClientSession
	tools    map[string]*mcp.Tool
}

// Start connects to the upstream server and builds the proxy server.
func (p *Proxy) Start(ctx context.Context) (*mcp.Server, error) {
	if len(p.Command) == 0 {
		return nil, errors.New("proxy: upstream command is required (changegate proxy -- <command> [args])")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "changegate-proxy", Version: p.Version}, nil)
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: start upstream %q: %w", strings.Join(p.Command, " "), err)
	}
	p.upstream = cs
	if p.Name == "" {
		p.Name = sanitize(p.Command[len(p.Command)-1])
		if r := cs.InitializeResult(); r != nil && r.ServerInfo != nil && r.ServerInfo.Name != "" {
			p.Name = sanitize(r.ServerInfo.Name)
		}
	}
	if p.Env == "" {
		p.Env = adapter.EnvPersonal
	}
	p.tools = map[string]*mcp.Tool{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "changegate-proxy", Title: "Changegate proxy for " + p.Name, Version: p.Version}, &mcp.ServerOptions{
		Instructions: "This server is wrapped by Changegate. Write operations are previewed, decided by policy and may need approval; " +
			"every change is logged and SQL changes can be undone with changegate_undo_change.",
	})
	var cursor string
	for {
		res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("proxy: list upstream tools: %w", err)
		}
		for _, t := range res.Tools {
			p.tools[t.Name] = t
			srv.AddTool(p.wrapTool(t), p.handler(t))
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if p.Inner != nil {
		p.Inner.AddChangegateTools(srv, "changegate_")
	}
	return srv, nil
}

// Run serves the proxy over stdio.
func (p *Proxy) Run(ctx context.Context) error {
	srv, err := p.Start(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = p.upstream.Close() }()
	err = srv.Run(ctx, &mcp.StdioTransport{})
	if p.Inner != nil {
		p.Inner.End(context.Background())
	}
	return err
}

var nonWord = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func sanitize(s string) string {
	s = nonWord.ReplaceAllString(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func (p *Proxy) isSQLTool(t *mcp.Tool) (string, bool) {
	if p.Inner == nil || p.Inner.Target == "" {
		return "", false
	}
	names := p.SQLTools
	if len(names) == 0 {
		names = DefaultSQLTools
	}
	match := false
	for _, n := range names {
		if t.Name == n || strings.HasSuffix(t.Name, "_"+n) || strings.HasSuffix(t.Name, "."+n) {
			match = true
		}
	}
	if !match {
		return "", false
	}
	schema, _ := t.InputSchema.(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	for _, a := range sqlArgNames {
		if _, ok := props[a]; ok {
			return a, true
		}
	}
	return "", false
}

func (p *Proxy) wrapTool(t *mcp.Tool) *mcp.Tool {
	w := *t
	if _, ok := p.isSQLTool(t); ok {
		w.Description = strings.TrimSpace(t.Description + " (Changegate: writes are previewed, policy-checked and undoable; reads pass through.)")
		return &w
	}
	if !readOnly(t) {
		w.Description = strings.TrimSpace(t.Description + " (Changegate: requires approval per policy; logged; not undoable.)")
	}
	return &w
}

func readOnly(t *mcp.Tool) bool { return t.Annotations != nil && t.Annotations.ReadOnlyHint }

func destructive(t *mcp.Tool) bool {
	if t.Annotations == nil || t.Annotations.DestructiveHint == nil {
		return true // MCP default
	}
	return *t.Annotations.DestructiveHint
}

func (p *Proxy) forward(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	var a any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
	}
	return p.upstream.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: a})
}

func errResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

const maxPassThroughBytes = 200_000

func capResult(r *mcp.CallToolResult) *mcp.CallToolResult {
	total := 0
	for i, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			total += len(tc.Text)
			if total > maxPassThroughBytes {
				tc.Text = tc.Text[:max(0, len(tc.Text)-(total-maxPassThroughBytes))] + "\n[changegate: result truncated]"
				r.Content = r.Content[:i+1]
				break
			}
		}
	}
	return r
}

func (p *Proxy) handler(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if arg, ok := p.isSQLTool(t); ok {
			var m map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &m)
			sql, _ := m[arg].(string)
			if strings.HasPrefix(req.Params.RequestState, "changegate:") {
				return p.Inner.ProposeAndApply(ctx, req, sql, "via "+t.Name)
			}
			_, stmts, err := postgres.Classify(sql)
			if err != nil {
				return errResult("changegate: %v", err), nil
			}
			reads := true
			for _, s := range stmts {
				if s.Access != adapter.Read {
					reads = false
				}
			}
			if reads {
				r, err := p.forward(ctx, t.Name, req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				return capResult(r), nil
			}
			return p.Inner.ProposeAndApply(ctx, req, sql, "via "+t.Name)
		}
		if readOnly(t) {
			r, err := p.forward(ctx, t.Name, req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			return capResult(r), nil
		}
		return p.gated(ctx, req, t)
	}
}

// gated handles a non-read-only tool without a known adapter: approval-only.
func (p *Proxy) gated(ctx context.Context, req *mcp.CallToolRequest, t *mcp.Tool) (*mcp.CallToolResult, error) {
	if p.Inner == nil {
		return errResult("changegate: no engine configured"), nil
	}
	eng := p.Inner.Engine
	var c *model.Change
	var err error
	if st := req.Params.RequestState; strings.HasPrefix(st, "changegate:") {
		c, err = eng.Get(ctx, strings.TrimPrefix(st, "changegate:"))
	} else {
		sess, serr := p.Inner.EnsureSession(ctx, req)
		if serr != nil {
			return nil, serr
		}
		c, err = eng.Propose(ctx, engine.ProposeRequest{
			Target:    p.target(),
			Operation: adapter.Operation{Kind: "mcp-tool", Payload: string(req.Params.Arguments), Params: map[string]string{"tool": t.Name, "destructive": fmt.Sprint(destructive(t))}},
			Principal: p.Inner.Principal(req), SessionID: sess.ID, Provenance: model.Provenance{Host: "mcp-proxy:" + p.Name},
		})
	}
	if err != nil {
		return errResult("changegate: %v", err), nil
	}
	switch c.State {
	case model.Rejected:
		return errResult("changegate: %s denied by policy: %s", t.Name, strings.Join(c.Decision.Reasons, "; ")), nil
	case model.AwaitingApproval:
		if c.Decision.Channel != policy.ChannelInline {
			return errResult("changegate: %s needs out-of-band approval: `changegate approve %s` by someone other than the proposer, then call %s again",
				t.Name, c.Plan.ID, "changegate_apply_change"), nil
		}
		approved, asked, need := p.Inner.Elicit(req, c)
		if need != nil {
			need.RequestState = "changegate:" + c.Plan.ID
			return need, nil
		}
		if !asked {
			return errResult("changegate: %s needs confirmation: run `changegate approve %s`, then call changegate_apply_change", t.Name, c.Plan.ID), nil
		}
		if c, err = eng.Approve(ctx, c.ID, engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: p.Inner.Human, Channel: "mcp-elicitation", Approved: approved}); err != nil {
			return errResult("changegate: %v", err), nil
		}
		if !approved {
			return errResult("changegate: the user declined %s", t.Name), nil
		}
	}
	c, err = eng.Apply(ctx, c.ID, p.target(), p.Inner.Principal(req).Agent)
	if err != nil {
		return errResult("changegate: %v", err), nil
	}
	var r mcp.CallToolResult
	if err := json.Unmarshal([]byte(c.Receipt.Output), &r); err != nil {
		return errResult("changegate: upstream result: %v", err), nil
	}
	return capResult(&r), nil
}

func (p *Proxy) target() adapter.Target {
	return adapter.Target{Name: "mcp:" + p.Name, Adapter: "mcp-tool:" + p.Name, Env: p.Env}
}

// Adapter returns the approval-only adapter for this proxy's upstream tools.
// Register it under "mcp-tool:<name>".
func (p *Proxy) Adapter() adapter.Adapter { return &toolAdapter{p: p} }

// AdapterName is the registry name of the proxy's tool adapter.
func (p *Proxy) AdapterName() string { return "mcp-tool:" + p.Name }

// toolAdapter records upstream tool calls as changes. Plan is static (no
// preview exists); Apply forwards the call; there is no Undo.
type toolAdapter struct {
	adapter.Unimplemented
	p *Proxy
}

func (a *toolAdapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "mcp-tool", Version: "0.1.0", Kinds: []string{"mcp-tool"}, Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply}}, nil
}

func (a *toolAdapter) Classify(_ context.Context, _ adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	acc := adapter.Write
	if op.Params["destructive"] != "false" {
		acc = adapter.Destructive
	}
	return adapter.Classification{Access: acc, Objects: []string{op.Params["tool"]},
		Statements: []adapter.StatementClass{{Kind: "TOOL " + op.Params["tool"], Access: acc, Text: op.Payload}}}, nil
}

func (a *toolAdapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	cls, _ := a.Classify(ctx, t, op)
	args := op.Payload
	if len(args) > 500 {
		args = args[:497] + "..."
	}
	p := &adapter.Plan{ID: adapter.NewID("p"), Adapter: t.Adapter, Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Reversibility: adapter.Irreversible, Destructive: cls.Access == adapter.Destructive, RehearsalTier: "static",
		Effects:   []adapter.Effect{{Kind: "tool-call", Object: op.Params["tool"], Description: "call " + op.Params["tool"] + " on " + a.p.Name}},
		Warnings:  []string{"no preview available for this tool; it is treated as irreversible (add a compensation manifest to change this)"},
		Diff:      fmt.Sprintf("tool call %s on %s (%s)\n  args: %s\n  no preview available; not undoable\n", op.Params["tool"], a.p.Name, t.Env, args),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

func (a *toolAdapter) Apply(ctx context.Context, _ adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	r, err := a.p.forward(ctx, p.Operation.Params["tool"], json.RawMessage(p.Operation.Payload))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(r)
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: string(b), Reversibility: adapter.Irreversible}, nil
}

func (a *toolAdapter) Verify(_ context.Context, _ adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Verification, error) {
	var res mcp.CallToolResult
	if err := json.Unmarshal([]byte(r.Output), &res); err != nil {
		return nil, err
	}
	return &adapter.Verification{Passed: !res.IsError, Checks: []adapter.Check{{Name: "tool result", Passed: !res.IsError}}}, nil
}
