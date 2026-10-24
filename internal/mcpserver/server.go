// Package mcpserver is Changeward's own MCP server for an adapter
// (`changeward mcp postgres`). It gives agents plan → approve → apply → undo
// tools instead of raw SQL execution. The server holds the credentials; the
// agent never sees them.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srawlin/changeward/adapters/postgres"
	"github.com/srawlin/changeward/internal/backend"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/pkg/adapter"
)

// Querier runs read-only queries (implemented by the SQL adapters).
type Querier = adapter.Querier

// Server is the Changeward MCP server for one target.
type Server struct {
	// Backend, if set, is where changes are made (e.g. a shared executor).
	// Otherwise a local backend is built from Engine and Sessions.
	Backend  backend.Backend
	Engine   *engine.Engine
	Sessions *session.Service
	Querier  Querier
	Target   string // configured target name
	Human    string // delegating human
	// SessionID reuses an existing session instead of starting one.
	SessionID string
	// Checkpoint takes a checkpoint when the session starts.
	Checkpoint bool
	// Branch makes the session work on a private branch of the target.
	Branch  bool
	Version string

	mu      sync.Mutex
	session *model.Session
	agent   string
	local   backend.Backend
}

func (s *Server) be() backend.Backend {
	if s.Backend != nil {
		return s.Backend
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.local == nil {
		s.local = &backend.Local{Engine: s.Engine, Sessions: s.Sessions, Human: s.Human}
	}
	return s.local
}

// Run serves MCP over stdio until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	srv := s.New()
	err := srv.Run(ctx, &mcp.StdioTransport{})
	s.endSession(context.Background())
	return err
}

// New builds the MCP server (exposed for tests and other transports).
func (s *Server) New() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "changeward", Title: "Changeward", Version: s.Version}, &mcp.ServerOptions{
		Instructions: `Changeward guards this database. Read with "query". To change anything, call "propose_change" with SQL: ` +
			`it returns an exact preview (rows affected, before/after values, locks, reversibility) and a policy decision without ` +
			`changing data. Then call "apply_change" with the plan_id. If approval is pending, tell the user and poll ` +
			`"get_change_status" or continue other work; do not try to bypass it. Every applied change can be undone with "undo_change"; ` +
			`"list_session_changes" shows this session's timeline.`,
	})
	t, f := true, false
	mcp.AddTool(srv, &mcp.Tool{Name: "query", Description: "Run a read-only SQL query (SELECT, SHOW, EXPLAIN). Results are capped and sensitive columns redacted.",
		Annotations: &mcp.ToolAnnotations{Title: "Read-only query", ReadOnlyHint: true, OpenWorldHint: &f}}, s.query)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_tables", Description: "List tables with columns and approximate row counts.",
		Annotations: &mcp.ToolAnnotations{Title: "List tables", ReadOnlyHint: true, OpenWorldHint: &f}}, s.listTables)
	mcp.AddTool(srv, &mcp.Tool{Name: "propose_change", Description: "Preview a change (INSERT/UPDATE/DELETE/DDL; several statements allowed) " +
		"with a dry run that is always rolled back. Returns plan_id, the exact effect, reversibility and the policy decision. Changes nothing.",
		Annotations: &mcp.ToolAnnotations{Title: "Propose change (dry run)", ReadOnlyHint: true, OpenWorldHint: &f}}, s.propose)
	mcp.AddTool(srv, &mcp.Tool{Name: "apply_change", Description: "Apply a proposed change by plan_id. Applies only if policy approved it " +
		"(possibly after asking the user). Fails safely if the data changed since the plan.",
		Annotations: &mcp.ToolAnnotations{Title: "Apply change", DestructiveHint: &t, OpenWorldHint: &f}}, s.apply)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_change_status", Description: "Get the state of a change (e.g. whether a pending approval was granted).",
		Annotations: &mcp.ToolAnnotations{Title: "Change status", ReadOnlyHint: true, OpenWorldHint: &f}}, s.status)
	mcp.AddTool(srv, &mcp.Tool{Name: "undo_change", Description: "Undo an applied change (by change_id, plan_id or session step number). " +
		"Refuses, without changing anything, if the affected rows were modified since.",
		Annotations: &mcp.ToolAnnotations{Title: "Undo change", DestructiveHint: &t, OpenWorldHint: &f}}, s.undo)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_session_changes", Description: "List the changes made in this session, in order.",
		Annotations: &mcp.ToolAnnotations{Title: "Session timeline", ReadOnlyHint: true, OpenWorldHint: &f}}, s.list)
	mcp.AddTool(srv, &mcp.Tool{Name: "checkpoint", Description: "Take a checkpoint of the database that the session can be restored to.",
		Annotations: &mcp.ToolAnnotations{Title: "Checkpoint", DestructiveHint: &f, OpenWorldHint: &f}}, s.checkpoint)
	return srv
}

func (s *Server) agentName(req *mcp.CallToolRequest) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent != "" {
		return s.agent
	}
	name := os.Getenv("CHANGEWARD_AGENT")
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil && p.ClientInfo.Name != "" {
			name = p.ClientInfo.Name
			if p.ClientInfo.Version != "" {
				name += "/" + p.ClientInfo.Version
			}
		}
	}
	if name == "" {
		name = "mcp-client"
	}
	s.agent = "agent:" + name
	return s.agent
}

func (s *Server) principal(req *mcp.CallToolRequest) model.Principal {
	return model.Principal{Kind: "agent", Agent: s.agentName(req), Human: s.Human}
}

// ensureSession starts (or reuses) the session lazily, so the agent identity
// from the MCP handshake is known.
func (s *Server) ensureSession(ctx context.Context, req *mcp.CallToolRequest) (*model.Session, error) {
	agent := s.agentName(req)
	be := s.be()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		return s.session, nil
	}
	if s.SessionID != "" {
		sess, err := be.GetSession(ctx, s.SessionID)
		if err != nil {
			return nil, err
		}
		s.session = sess
		return sess, nil
	}
	sess, err := be.StartSession(ctx, backend.StartRequest{Target: s.Target, Agent: agent, Checkpoint: s.Checkpoint, Branch: s.Branch,
		Meta: map[string]string{"via": "mcp"}})
	if err != nil {
		return nil, err
	}
	s.session = sess
	return sess, nil
}

func (s *Server) endSession(ctx context.Context) {
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess != nil && s.SessionID == "" {
		_ = s.be().EndSession(ctx, sess.ID)
	}
}

func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func toolError(err error) (*mcp.CallToolResult, ChangeOut, error) {
	r := text("error: %v", err)
	r.IsError = true
	return r, ChangeOut{}, nil
}

// QueryIn is the input of the query tool.
type QueryIn struct {
	SQL     string `json:"sql" jsonschema:"a single read-only SQL statement"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"maximum rows to return (default 100, max 1000)"`
}

func (s *Server) query(ctx context.Context, req *mcp.CallToolRequest, in QueryIn) (*mcp.CallToolResult, *adapter.QueryResult, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if in.MaxRows <= 0 {
		in.MaxRows = 100
	}
	if in.MaxRows > 1000 {
		in.MaxRows = 1000
	}
	res, err := s.be().Query(ctx, sess.ID, in.SQL, in.MaxRows)
	if err != nil {
		r := text("error: %v", err)
		r.IsError = true
		return r, nil, nil
	}
	return text("%s", formatTable(res)), res, nil
}

func formatTable(r *adapter.QueryResult) string {
	var b strings.Builder
	b.WriteString(strings.Join(r.Columns, " | "))
	b.WriteString("\n")
	for _, row := range r.Rows {
		b.WriteString(strings.Join(row, " | "))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "(%d rows", len(r.Rows))
	if r.Truncated {
		b.WriteString(", truncated")
	}
	if len(r.Redacted) > 0 {
		b.WriteString("; redacted: " + strings.Join(r.Redacted, ", "))
	}
	b.WriteString(")")
	return b.String()
}

// Empty is an empty input.
type Empty struct{}

func (s *Server) listTables(ctx context.Context, req *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, *adapter.QueryResult, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if name, _ := s.be().TargetAdapter(ctx, sess.ID); name == "mysql" {
		return s.query(ctx, req, QueryIn{MaxRows: 1000, SQL: `SELECT CONCAT(t.TABLE_SCHEMA, '.', t.TABLE_NAME) AS table_name, t.TABLE_ROWS AS approx_rows,
			GROUP_CONCAT(CONCAT(c.COLUMN_NAME, ' ', c.COLUMN_TYPE) ORDER BY c.ORDINAL_POSITION SEPARATOR ', ') AS columns
			FROM information_schema.TABLES t JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
			WHERE t.TABLE_SCHEMA = DATABASE() AND c.COLUMN_NAME NOT LIKE '\_cw\_%' GROUP BY t.TABLE_SCHEMA, t.TABLE_NAME, t.TABLE_ROWS ORDER BY 1`})
	}
	return s.query(ctx, req, QueryIn{MaxRows: 1000, SQL: `SELECT n.nspname || '.' || c.relname AS table_name,
		  greatest(c.reltuples, 0)::bigint AS approx_rows,
		  string_agg(a.attname || ' ' || format_type(a.atttypid, a.atttypmod), ', ' ORDER BY a.attnum) AS columns
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attname NOT LIKE '\_cw\_%'
		WHERE c.relkind IN ('r','p','v','m') AND n.nspname NOT IN ('pg_catalog','information_schema','_changeward_trash') AND n.nspname NOT LIKE 'pg_%'
		GROUP BY 1, 2 ORDER BY 1`})
}

// ProposeIn is the input of propose_change.
type ProposeIn struct {
	SQL    string `json:"sql" jsonschema:"the SQL to run; several statements are applied together in one transaction"`
	Intent string `json:"intent,omitempty" jsonschema:"what the user asked for, in their words (Changeward checks that the effect matches it)"`
}

// ChangeOut is the structured result of change tools.
type ChangeOut struct {
	ChangeID      string   `json:"change_id,omitempty"`
	PlanID        string   `json:"plan_id,omitempty"`
	State         string   `json:"state,omitempty"`
	Decision      string   `json:"decision,omitempty"`
	Reversibility string   `json:"reversibility,omitempty"`
	RowsAffected  int64    `json:"rows_affected"`
	Warnings      []string `json:"warnings,omitempty"`
	Next          string   `json:"next,omitempty"`
}

func out(c *model.Change) ChangeOut {
	o := ChangeOut{ChangeID: c.ID, State: string(c.State)}
	if c.Plan != nil {
		o.PlanID, o.Reversibility, o.RowsAffected, o.Warnings = c.Plan.ID, string(c.Plan.Reversibility), c.Plan.BlastRows, c.Plan.Warnings
	}
	if c.Receipt != nil {
		o.RowsAffected = c.Receipt.RowsAffected
	}
	if c.Decision != nil {
		o.Decision = c.Decision.Decision
	}
	switch c.State {
	case model.AutoApproved, model.Approved:
		o.Next = "call apply_change with plan_id " + o.PlanID
	case model.AwaitingApproval:
		if c.Decision != nil && c.Decision.Channel == policy.ChannelInline {
			o.Next = "call apply_change; the user will be asked to confirm (or they can run: changeward approve " + o.PlanID + ")"
		} else {
			o.Next = "approval required out of band (Slack, web or `changeward approve " + o.PlanID + "` by someone other than the proposer); poll get_change_status"
		}
	case model.Rejected:
		o.Next = "denied by policy; do not retry the same change"
	case model.Verified, model.Applied:
		o.Next = "applied; undo with undo_change " + c.ID
	}
	return o
}

func (s *Server) propose(ctx context.Context, req *mcp.CallToolRequest, in ProposeIn) (*mcp.CallToolResult, ChangeOut, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return toolError(err)
	}
	c, err := s.be().Propose(ctx, backend.ProposeRequest{SessionID: sess.ID, Payload: in.SQL, Intent: in.Intent,
		Host: "mcp:" + strings.TrimPrefix(s.agentName(req), "agent:")})
	if err != nil {
		if errors.Is(err, postgres.ErrReadOnly) || strings.Contains(err.Error(), "read-only; use query") {
			return toolError(errors.New("that SQL only reads; use the query tool"))
		}
		return toolError(err)
	}
	o := out(c)
	reasons := ""
	if c.Decision != nil && len(c.Decision.Reasons) > 0 {
		reasons = " — " + strings.Join(c.Decision.Reasons, "; ")
	}
	return text("%s\npolicy: %s%s\nnext: %s", c.Plan.Diff, o.Decision, reasons, o.Next), o, nil
}

// ProposeAndApply plans sql and, if policy allows (possibly after asking the
// user), applies it. It backs MCP proxy mode, where the agent called a
// third-party "execute SQL" tool and expects execution.
func (s *Server) ProposeAndApply(ctx context.Context, req *mcp.CallToolRequest, sql, intent string) (*mcp.CallToolResult, error) {
	// On a multi-round-trip retry the change was already proposed.
	if st := req.Params.RequestState; strings.HasPrefix(st, "changeward:") {
		r, _, err := s.apply(ctx, req, RefIn{PlanID: strings.TrimPrefix(st, "changeward:")})
		return r, err
	}
	r, o, err := s.propose(ctx, req, ProposeIn{SQL: sql, Intent: intent})
	if err != nil || r.IsError || o.Decision == policy.Deny {
		return r, err
	}
	ar, ao, err := s.apply(ctx, req, RefIn{PlanID: o.PlanID})
	if err != nil {
		return nil, err
	}
	if ar.InputRequests != nil {
		ar.RequestState = "changeward:" + o.PlanID
		return ar, nil
	}
	if ao.State == string(model.Verified) || ao.State == string(model.Applied) {
		ar.Content = append([]mcp.Content{&mcp.TextContent{Text: planText(r)}}, ar.Content...)
	}
	return ar, nil
}

func planText(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*mcp.TextContent); ok {
		return tc.Text + "\n"
	}
	return ""
}

// AddChangewardTools registers the change-management tools on another
// server (proxy mode), with a name prefix.
func (s *Server) AddChangewardTools(srv *mcp.Server, prefix string) {
	t, f := true, false
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "apply_change", Description: "Apply a pending Changeward change by plan_id (after approval).",
		Annotations: &mcp.ToolAnnotations{Title: "Apply change", DestructiveHint: &t, OpenWorldHint: &f}}, s.apply)
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "get_change_status", Description: "Get the state of a Changeward change.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f}}, s.status)
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "undo_change", Description: "Undo a change made through this proxy (change_id, plan_id or session step).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &t, OpenWorldHint: &f}}, s.undo)
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "list_session_changes", Description: "List changes made in this session.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f}}, s.list)
}

// Principal exposes the session principal for proxy mode.
func (s *Server) Principal(req *mcp.CallToolRequest) model.Principal { return s.principal(req) }

// EnsureSession exposes the lazily started session for proxy mode.
func (s *Server) EnsureSession(ctx context.Context, req *mcp.CallToolRequest) (*model.Session, error) {
	return s.ensureSession(ctx, req)
}

// End ends the session the server started.
func (s *Server) End(ctx context.Context) { s.endSession(ctx) }

// Elicit exposes inline confirmation for proxy mode.
func (s *Server) Elicit(req *mcp.CallToolRequest, c *model.Change) (approved, asked bool, need *mcp.CallToolResult) {
	return s.elicit(req, c)
}

// RefIn names a change.
type RefIn struct {
	PlanID string `json:"plan_id" jsonschema:"plan_id (or change_id) returned by propose_change"`
}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, in RefIn) (*mcp.CallToolResult, ChangeOut, error) {
	c, err := s.be().Get(ctx, in.PlanID)
	if err != nil {
		return toolError(err)
	}
	o := out(c)
	return text("%s: %s\nnext: %s", c.ID, c.State, o.Next), o, nil
}

func (s *Server) apply(ctx context.Context, req *mcp.CallToolRequest, in RefIn) (*mcp.CallToolResult, ChangeOut, error) {
	c, err := s.be().Get(ctx, in.PlanID)
	if err != nil {
		return toolError(err)
	}
	if c.State == model.AwaitingApproval {
		if c.Decision == nil || c.Decision.Channel != policy.ChannelInline {
			o := out(c)
			return text("pending approval: %s\nnext: %s", c.Plan.ID, o.Next), o, nil
		}
		approved, asked, need := s.elicit(req, c)
		if need != nil {
			return need, ChangeOut{}, nil
		}
		if !asked {
			o := out(c)
			return text("pending approval: this client cannot ask the user inline. Ask the user to run `changeward approve %s`, then call apply_change again.", c.Plan.ID), o, nil
		}
		if c, err = s.be().ApproveInline(ctx, c.ID, c.Plan.Hash, approved, "mcp-elicitation"); err != nil {
			return toolError(err)
		}
		if !approved {
			return text("the user declined change %s; it was not applied", c.ID), out(c), nil
		}
	}
	if _, err := s.ensureSession(ctx, req); err != nil {
		return toolError(err)
	}
	c, err = s.be().Apply(ctx, c.ID)
	if err != nil {
		var ce *adapter.ConflictError
		if errors.As(err, &ce) {
			return toolError(fmt.Errorf("%v; call propose_change again to re-plan against current data", err))
		}
		if c != nil && c.State == model.Undone {
			return toolError(fmt.Errorf("%v (rolled back)", err))
		}
		return toolError(err)
	}
	o := out(c)
	notes := ""
	if c.Receipt != nil && len(c.Receipt.Warnings) > 0 {
		notes = "\nnotes: " + strings.Join(c.Receipt.Warnings, "; ")
	}
	return text("applied %s: %d rows, %s, verified%s\nundo: undo_change %s", c.ID, c.Receipt.RowsAffected, c.Receipt.Output, notes, c.ID), o, nil
}

const approveInputKey = "changeward_approve"

// elicit asks the user to confirm a change through the MCP client, using a
// multi-round-trip input request (SEP-2322; the SDK falls back to a classic
// elicitation/create call for older clients). It returns need != nil when
// the handler must return that input request, or the user's answer once the
// client retries with the response.
func (s *Server) elicit(req *mcp.CallToolRequest, c *model.Change) (approved, asked bool, need *mcp.CallToolResult) {
	if req == nil || req.Session == nil || req.Params == nil {
		return false, false, nil
	}
	if r, ok := req.Params.InputResponses[approveInputKey]; ok {
		er, ok := r.(*mcp.ElicitResult)
		if !ok || er.Action != "accept" {
			return false, true, nil
		}
		v, _ := er.Content["approve"].(bool)
		return v, true, nil
	}
	p := req.Session.InitializeParams()
	if p == nil || p.Capabilities == nil || p.Capabilities.Elicitation == nil {
		return false, false, nil
	}
	reason := ""
	if c.Decision != nil {
		reason = strings.Join(c.Decision.Reasons, "; ")
	}
	return false, false, &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{approveInputKey: &mcp.ElicitParams{
		Message: fmt.Sprintf("Changeward: approve this change?\n\n%s\n%s", c.Plan.Diff, reason),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approve": map[string]any{"type": "boolean", "title": "Apply this change", "description": "Plan " + c.Plan.ID},
			},
			"required": []string{"approve"},
		},
	}}}
}

// UndoIn names a change to undo.
type UndoIn struct {
	Change         string `json:"change" jsonschema:"change_id, plan_id, or the step number in this session (e.g. 3)"`
	WithDependents bool   `json:"with_dependents,omitempty" jsonschema:"also undo later changes that touch the same objects (newest first)"`
}

func (s *Server) undo(ctx context.Context, req *mcp.CallToolRequest, in UndoIn) (*mcp.CallToolResult, ChangeOut, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return toolError(err)
	}
	ref := in.Change
	var n int
	if _, scanErr := fmt.Sscan(in.Change, &n); scanErr == nil && n > 0 && !strings.HasPrefix(in.Change, "c_") {
		cs, err := s.be().Changes(ctx, sess.ID)
		if err != nil {
			return toolError(err)
		}
		if n > len(cs) {
			return toolError(fmt.Errorf("this session has %d changes", len(cs)))
		}
		ref = cs[n-1].ID
	}
	res, err := s.be().Undo(ctx, ref, in.WithDependents)
	if res != nil && len(res.Dependents) > 0 {
		var ids []string
		for _, d := range res.Dependents {
			ids = append(ids, d.ID)
		}
		return toolError(fmt.Errorf("later changes touch the same objects and must be undone first: %s; call again with with_dependents=true",
			strings.Join(ids, ", ")))
	}
	var done []string
	if res != nil {
		for _, u := range res.Undone {
			done = append(done, u.ID)
		}
	}
	if err != nil {
		return toolError(fmt.Errorf("undid [%s]; then: %v", strings.Join(done, ", "), err))
	}
	last := res.Undone[len(res.Undone)-1]
	return text("undone: %s", strings.Join(done, ", ")), out(last), nil
}

// ListOut is the session timeline.
type ListOut struct {
	SessionID string      `json:"session_id"`
	Changes   []ChangeOut `json:"changes"`
}

func (s *Server) list(ctx context.Context, req *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, ListOut, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return nil, ListOut{}, err
	}
	cs, err := s.be().Changes(ctx, sess.ID)
	if err != nil {
		return nil, ListOut{}, err
	}
	lo := ListOut{SessionID: sess.ID}
	var b strings.Builder
	fmt.Fprintf(&b, "session %s (%s)\n", sess.ID, sess.Env)
	for i, c := range cs {
		lo.Changes = append(lo.Changes, out(c))
		sql := strings.Join(strings.Fields(c.Operation.Payload), " ")
		if len(sql) > 80 {
			sql = sql[:77] + "..."
		}
		fmt.Fprintf(&b, "#%d %s %s [%s]\n", i+1, c.ID, sql, c.State)
	}
	return text("%s", b.String()), lo, nil
}

// CheckpointIn labels a checkpoint.
type CheckpointIn struct {
	Label string `json:"label,omitempty"`
}

func (s *Server) checkpoint(ctx context.Context, req *mcp.CallToolRequest, in CheckpointIn) (*mcp.CallToolResult, ChangeOut, error) {
	sess, err := s.ensureSession(ctx, req)
	if err != nil {
		return toolError(err)
	}
	id, err := s.be().Checkpoint(ctx, sess.ID, in.Label)
	if err != nil {
		return toolError(err)
	}
	return text("checkpoint %s taken; the user can restore it with `changeward restore %s --yes`", id, id), ChangeOut{}, nil
}
