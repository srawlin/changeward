package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/changegate/changegate/adapters/toykv"
	"github.com/changegate/changegate/internal/engine"
	"github.com/changegate/changegate/internal/model"
	"github.com/changegate/changegate/internal/policy"
	"github.com/changegate/changegate/internal/store"
	"github.com/changegate/changegate/pkg/adapter"
)

type fixture struct {
	eng *engine.Engine
	st  *store.SQLite
	dir string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "cg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol, err := policy.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{eng: &engine.Engine{Store: st, Adapters: engine.MapRegistry{"toykv": toykv.New()}, Policy: pol}, st: st, dir: dir}
}

func (f *fixture) target(env string) adapter.Target {
	return adapter.Target{Name: "kv", Adapter: "toykv", Env: env, Config: map[string]string{"path": filepath.Join(f.dir, "kv.json")}}
}

var agent = model.Principal{Kind: "agent", Agent: "agent:test", Human: "user:steve"}

func (f *fixture) propose(t *testing.T, env, payload, session string) *model.Change {
	t.Helper()
	c, err := f.eng.Propose(context.Background(), engine.ProposeRequest{
		Target: f.target(env), Operation: adapter.Operation{Kind: "kv", Payload: payload}, Principal: agent, SessionID: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPersonalLifecycle(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	c := f.propose(t, "personal", "set a 1", "")
	if c.State != model.AutoApproved {
		t.Fatalf("state = %s (%+v)", c.State, c.Decision)
	}
	c, err := f.eng.Apply(ctx, c.Plan.ID, f.target("personal"), "test")
	if err != nil || c.State != model.Verified {
		t.Fatalf("apply: %v %s", err, c.State)
	}
	if _, err := f.eng.Apply(ctx, c.ID, f.target("personal"), "test"); err == nil {
		t.Fatal("double apply should fail")
	}
	c, err = f.eng.Undo(ctx, c.ID, f.target("personal"), "test")
	if err != nil || c.State != model.Undone {
		t.Fatalf("undo: %v %s", err, c.State)
	}
	evs, _ := f.st.ListEvents(ctx, store.EventFilter{ChangeID: c.ID})
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := []string{"change.proposed", "change.planned", "change.rehearsed", "change.decided", "change.applying", "change.applied", "change.verifying", "change.verified", "change.undoing", "change.undone"}
	if len(types) != len(want) {
		t.Fatalf("events %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events %v", types)
		}
	}
	if _, err := store.VerifyChain(ctx, f.st); err != nil {
		t.Fatal(err)
	}
}

func TestProdApprovalRules(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	// No incident reference: denied.
	if c := f.propose(t, "prod", "set a 1", ""); c.State != model.Rejected {
		t.Fatalf("prod without incident: %s", c.State)
	}
	sess := &model.Session{ID: "s_inc", Env: "prod", Target: "kv", IncidentRef: "INC-42", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := f.st.CreateSession(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	c := f.propose(t, "prod", "set a 1", "s_inc")
	if c.State != model.AwaitingApproval || c.Decision.Channel != policy.ChannelOutOfBand {
		t.Fatalf("prod with incident: %s %+v", c.State, c.Decision)
	}
	if _, err := f.eng.Apply(ctx, c.ID, f.target("prod"), "agent"); !errors.Is(err, engine.ErrNotApproved) {
		t.Fatalf("apply before approval: %v", err)
	}
	try := func(req engine.ApproveRequest) error {
		_, err := f.eng.Approve(ctx, c.ID, req)
		return err
	}
	if err := try(engine.ApproveRequest{PlanHash: "sha256:other", Approver: "user:alice", Channel: "slack", Approved: true}); err == nil {
		t.Fatal("approval of a different plan hash must fail")
	}
	if err := try(engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: "user:alice", Channel: "mcp-elicitation", Approved: true}); err == nil {
		t.Fatal("in-band approval must fail for out-of-band policy")
	}
	if err := try(engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: "user:steve", Channel: "slack", Approved: true}); err == nil {
		t.Fatal("self-approval must fail")
	}
	if err := try(engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: "user:alice", Channel: "slack", Approved: true}); err != nil {
		t.Fatal(err)
	}
	c, err := f.eng.Apply(ctx, c.ID, f.target("prod"), "executor")
	if err != nil || c.State != model.Verified {
		t.Fatalf("apply: %v %s", err, c.State)
	}
	// Wrong target binding is refused.
	if _, err := f.eng.Undo(ctx, c.ID, f.target("personal"), "x"); err == nil {
		t.Fatal("undo against a different env must fail")
	}
}

func TestDriftAndStaleUndo(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.propose(t, "personal", "set k 1", "")
	b := f.propose(t, "personal", "set k 2", "")
	if _, err := f.eng.Apply(ctx, b.ID, f.target("personal"), "t"); err != nil {
		t.Fatal(err)
	}
	a, err := f.eng.Apply(ctx, a.ID, f.target("personal"), "t")
	var ce *adapter.ConflictError
	if !errors.As(err, &ce) || a.State != model.Failed {
		t.Fatalf("drift: %v %s", err, a.State)
	}
	c := f.propose(t, "personal", "set k 3", "")
	if _, err := f.eng.Apply(ctx, c.ID, f.target("personal"), "t"); err != nil {
		t.Fatal(err)
	}
	// b's after-image no longer holds, so undoing b is refused and b stays verified.
	b, err = f.eng.Undo(ctx, b.ID, f.target("personal"), "t")
	if !errors.As(err, &ce) || b.State != model.Verified {
		t.Fatalf("stale undo: %v %s", err, b.State)
	}
}

func TestPlanExpiry(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	now := time.Now()
	f.eng.Now = func() time.Time { return now }
	c := f.propose(t, "personal", "set x 1", "")
	f.eng.Now = func() time.Time { return now.Add(time.Hour) }
	if _, err := f.eng.Apply(ctx, c.ID, f.target("personal"), "t"); !errors.Is(err, engine.ErrExpired) {
		t.Fatalf("expected expiry, got %v", err)
	}
}
