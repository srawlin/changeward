// Package backend is the API that agent-facing front ends (the MCP server,
// SDKs) use to make changes. Local runs the engine in-process with the
// developer's own credentials; the executor client (internal/executor)
// talks to a shared executor that holds credentials and enforces policy
// next to the target. Both implement Backend.
package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

// StartRequest starts a session.
type StartRequest struct {
	Target      string            `json:"target"`
	Agent       string            `json:"agent"` // e.g. "agent:claude-code/2.1"
	IncidentRef string            `json:"incident_ref,omitempty"`
	Checkpoint  bool              `json:"checkpoint,omitempty"`
	Branch      bool              `json:"branch,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

// ProposeRequest proposes a change within a session.
type ProposeRequest struct {
	SessionID string `json:"session_id"`
	Payload   string `json:"payload"`
	Intent    string `json:"intent,omitempty"`
	Kind      string `json:"kind,omitempty"` // default: the target adapter's kind
	Host      string `json:"host,omitempty"`
}

// UndoResult reports an undo.
type UndoResult struct {
	Undone     []*model.Change `json:"undone"`
	Dependents []*model.Change `json:"dependents,omitempty"` // set when dependents block the undo
	Error      string          `json:"error,omitempty"`
}

// Backend is implemented in-process (Local) and over HTTP (executor client).
type Backend interface {
	StartSession(ctx context.Context, r StartRequest) (*model.Session, error)
	GetSession(ctx context.Context, id string) (*model.Session, error)
	EndSession(ctx context.Context, id string) error
	TargetAdapter(ctx context.Context, sessionID string) (string, error)
	Propose(ctx context.Context, r ProposeRequest) (*model.Change, error)
	Get(ctx context.Context, ref string) (*model.Change, error)
	// ApproveInline records the delegating human's in-band confirmation
	// (only accepted where policy allows an inline channel).
	ApproveInline(ctx context.Context, ref, planHash string, approved bool, channel string) (*model.Change, error)
	Apply(ctx context.Context, ref string) (*model.Change, error)
	Undo(ctx context.Context, ref string, withDependents bool) (*UndoResult, error)
	Changes(ctx context.Context, sessionID string) ([]*model.Change, error)
	Checkpoint(ctx context.Context, sessionID, label string) (string, error)
	Query(ctx context.Context, sessionID, query string, maxRows int) (*adapter.QueryResult, error)
}

// Local runs everything in-process.
type Local struct {
	Engine   *engine.Engine
	Sessions *session.Service
	// Human is the delegating human for changes and inline approvals.
	Human string
}

var _ Backend = (*Local)(nil)

// StartSession implements Backend.
func (l *Local) StartSession(ctx context.Context, r StartRequest) (*model.Session, error) {
	p := model.Principal{Kind: "agent", Agent: r.Agent, Human: l.Human}
	if r.Agent == "" {
		p = model.Principal{Kind: "human", Human: l.Human}
	}
	s, err := l.Sessions.Start(ctx, session.StartOptions{Target: r.Target, Principal: p, IncidentRef: r.IncidentRef, Checkpoint: r.Checkpoint, Meta: r.Meta})
	if err != nil && s == nil {
		return nil, err
	}
	if r.Branch {
		if s, err = l.Sessions.CreateBranch(ctx, s.ID, ""); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// GetSession implements Backend.
func (l *Local) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return l.Sessions.Store.GetSession(ctx, id)
}

// EndSession implements Backend.
func (l *Local) EndSession(ctx context.Context, id string) error {
	_, err := l.Sessions.End(ctx, id)
	return err
}

func (l *Local) target(ctx context.Context, sessionID string) (*model.Session, adapter.Target, error) {
	s, err := l.Sessions.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, adapter.Target{}, err
	}
	t, err := l.Sessions.TargetFor(ctx, s)
	return s, t, err
}

// TargetAdapter implements Backend.
func (l *Local) TargetAdapter(ctx context.Context, sessionID string) (string, error) {
	_, t, err := l.target(ctx, sessionID)
	return t.Adapter, err
}

// KindFor maps an adapter to its operation kind.
func KindFor(adapterName string) string {
	switch adapterName {
	case "postgres", "mysql":
		return "sql"
	case "kubernetes":
		return "k8s"
	case "configfile":
		return "file"
	case "httpapi":
		return "http"
	case "toykv":
		return "kv"
	}
	return adapterName
}

// Propose implements Backend.
func (l *Local) Propose(ctx context.Context, r ProposeRequest) (*model.Change, error) {
	s, t, err := l.target(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	kind := r.Kind
	if kind == "" {
		kind = KindFor(t.Adapter)
	}
	return l.Engine.Propose(ctx, engine.ProposeRequest{Target: t, Operation: adapter.Operation{Kind: kind, Payload: r.Payload, Intent: r.Intent},
		Principal: s.Principal, SessionID: s.ID, Provenance: model.Provenance{Host: r.Host}})
}

// Get implements Backend.
func (l *Local) Get(ctx context.Context, ref string) (*model.Change, error) {
	return l.Engine.Get(ctx, ref)
}

// ApproveInline implements Backend.
func (l *Local) ApproveInline(ctx context.Context, ref, planHash string, approved bool, channel string) (*model.Change, error) {
	c, err := l.Engine.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if c.Decision == nil || c.Decision.Channel != "inline" {
		return c, errors.New("this change needs out-of-band approval")
	}
	return l.Engine.Approve(ctx, c.ID, engine.ApproveRequest{PlanHash: planHash, Approver: l.Human, Channel: channel, Approved: approved,
		Reason: "confirmed in the agent client"})
}

func (l *Local) targetForChange(ctx context.Context, c *model.Change) (adapter.Target, error) {
	if c.SessionID != "" {
		if s, err := l.Sessions.Store.GetSession(ctx, c.SessionID); err == nil {
			if s.Branch != nil && c.Target.Env == s.Branch.Target.Env && c.Target.Name == s.Branch.Target.Name {
				return s.Branch.Target, nil
			}
		}
	}
	return l.Sessions.Resolve(c.Target.Name)
}

// Apply implements Backend.
func (l *Local) Apply(ctx context.Context, ref string) (*model.Change, error) {
	c, err := l.Engine.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	t, err := l.targetForChange(ctx, c)
	if err != nil {
		return c, err
	}
	return l.Engine.Apply(ctx, c.ID, t, c.Principal.String())
}

// Undo implements Backend.
func (l *Local) Undo(ctx context.Context, ref string, withDependents bool) (*UndoResult, error) {
	c, err := l.Engine.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !session.Applied(c) {
		return nil, fmt.Errorf("change %s is %s; nothing to undo", c.ID, c.State)
	}
	all, err := l.Engine.Store.ListChanges(ctx, store.ChangeFilter{Target: c.Target.Name})
	if err != nil {
		return nil, err
	}
	if deps := session.Dependents(all, c); len(deps) > 0 && !withDependents {
		return &UndoResult{Dependents: deps}, nil
	}
	res := l.Sessions.UndoChain(ctx, c, withDependents, l.Human)
	out := &UndoResult{Undone: res.Undone}
	if res.Err != nil {
		out.Error = res.Err.Error()
		return out, res.Err
	}
	return out, nil
}

// Changes implements Backend.
func (l *Local) Changes(ctx context.Context, sessionID string) ([]*model.Change, error) {
	return l.Sessions.Changes(ctx, sessionID)
}

// Checkpoint implements Backend.
func (l *Local) Checkpoint(ctx context.Context, sessionID, label string) (string, error) {
	cp, err := l.Sessions.Checkpoint(ctx, sessionID, label)
	if err != nil {
		return "", err
	}
	return cp.Ref.ID, nil
}

// Query implements Backend.
func (l *Local) Query(ctx context.Context, sessionID, q string, maxRows int) (*adapter.QueryResult, error) {
	_, t, err := l.target(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	ad, err := l.Engine.Adapters.Adapter(t.Adapter)
	if err != nil {
		return nil, err
	}
	qr, ok := ad.(adapter.Querier)
	if !ok {
		return nil, fmt.Errorf("adapter %s does not serve queries", t.Adapter)
	}
	return qr.Query(ctx, t, q, maxRows)
}
