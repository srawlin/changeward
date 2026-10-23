package executor

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/internal/backend"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

func TestExecutor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pol, _ := policy.New(nil)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"toykv": toykv.New()}, Policy: pol}
	targets := map[string]adapter.Target{
		"dev":  {Name: "dev", Adapter: "toykv", Env: "personal", Config: map[string]string{"path": filepath.Join(dir, "dev.json")}},
		"prod": {Name: "prod", Adapter: "toykv", Env: "prod", Config: map[string]string{"path": filepath.Join(dir, "prod.json")}},
	}
	svc := &session.Service{Engine: eng, Store: st, StateDir: dir, Resolve: func(n string) (adapter.Target, error) { return targets[n], nil }}
	aliceTok, alice := NewToken("alice", []string{RolePropose})
	bobTok, bob := NewToken("bob", []string{RolePropose, RoleApprove})
	srv := httptest.NewServer((&Server{Engine: eng, Sessions: svc, Tokens: []Token{alice, bob}}).Handler())
	defer srv.Close()

	if _, err := NewClient(srv.URL, "cwx_bogus").StartSession(ctx, backend.StartRequest{Target: "dev"}); err == nil {
		t.Fatal("bad token accepted")
	}
	a := NewClient(srv.URL, aliceTok)
	s, err := a.StartSession(ctx, backend.StartRequest{Target: "dev", Agent: "claude-code"})
	if err != nil || s.Principal.Human != "user:alice" || s.Principal.Agent != "agent:claude-code" {
		t.Fatalf("start: %v %+v", err, s)
	}
	c, err := a.Propose(ctx, backend.ProposeRequest{SessionID: s.ID, Payload: "set k 1"})
	if err != nil || c.State != model.AutoApproved {
		t.Fatalf("propose: %v %+v", err, c)
	}
	if c, err = a.Apply(ctx, c.ID); err != nil || c.State != model.Verified {
		t.Fatalf("apply: %v", err)
	}
	// Bob cannot use Alice's session.
	b := NewClient(srv.URL, bobTok)
	var api *APIError
	if _, err := b.Propose(ctx, backend.ProposeRequest{SessionID: s.ID, Payload: "set k 2"}); !errors.As(err, &api) || api.Status != 403 {
		t.Fatalf("foreign session: %v", err)
	}
	// Stale undo surfaces as a conflict through the API.
	c2, _ := a.Propose(ctx, backend.ProposeRequest{SessionID: s.ID, Payload: "set k 2"})
	if _, err := a.Apply(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := a.Undo(ctx, c.ID, false); res == nil || len(res.Dependents) != 1 {
		t.Fatalf("dependents: %+v", res)
	}
	if res, err := a.Undo(ctx, c.ID, true); err != nil || len(res.Undone) != 2 {
		t.Fatalf("undo chain: %v %+v", err, res)
	}
	// Production: out-of-band approval by someone else.
	ps, _ := a.StartSession(ctx, backend.StartRequest{Target: "prod", Agent: "ops-agent", IncidentRef: "INC-9"})
	pc, err := a.Propose(ctx, backend.ProposeRequest{SessionID: ps.ID, Payload: "set flag on"})
	if err != nil || pc.State != model.AwaitingApproval {
		t.Fatalf("prod propose: %v %s", err, pc.State)
	}
	if _, err := a.ApproveInline(ctx, pc.ID, pc.Plan.Hash, true, "mcp-elicitation"); err == nil {
		t.Fatal("inline approval accepted for an out-of-band decision")
	}
	if _, err := a.Apply(ctx, pc.ID); err == nil {
		t.Fatal("unapproved prod change applied")
	}
	if _, err := b.Approve(ctx, pc.ID, pc.Plan.Hash, true, "looks good"); err != nil {
		t.Fatal(err)
	}
	if pc, err = a.Apply(ctx, pc.ID); err != nil || pc.State != model.Verified {
		t.Fatalf("approved apply: %v", err)
	}
	// Alice (no approve role) cannot approve.
	pc2, _ := a.Propose(ctx, backend.ProposeRequest{SessionID: ps.ID, Payload: "set flag off"})
	if _, err := (&Client{URL: srv.URL, Token: aliceTok, HTTP: a.HTTP}).Approve(ctx, pc2.ID, pc2.Plan.Hash, true, ""); !errors.As(err, &api) || api.Status != 403 {
		t.Fatalf("approve without role: %v", err)
	}
	// An approver cannot approve a change they proposed.
	bs, _ := b.StartSession(ctx, backend.StartRequest{Target: "prod", Agent: "x", IncidentRef: "INC-10"})
	bc, _ := b.Propose(ctx, backend.ProposeRequest{SessionID: bs.ID, Payload: "set z 1"})
	if _, err := b.Approve(ctx, bc.ID, bc.Plan.Hash, true, ""); err == nil {
		t.Fatal("self-approval accepted")
	}
	_ = time.Second
}
