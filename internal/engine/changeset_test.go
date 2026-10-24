package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/pkg/adapter"
)

func TestChangesetSaga(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	kv2 := adapter.Target{Name: "kv2", Adapter: "toykv", Env: "personal", Config: map[string]string{"path": filepath.Join(f.dir, "kv2.json")}}
	targets := func(c *model.Change) (adapter.Target, error) {
		if c.Target.Name == "kv2" {
			return kv2, nil
		}
		return f.target("personal"), nil
	}
	req := func(t adapter.Target, p string) engine.ProposeRequest {
		return engine.ProposeRequest{Target: t, Operation: adapter.Operation{Kind: "kv", Payload: p}, Principal: agent}
	}
	cs, _, err := f.eng.ProposeChangeset(ctx, "deploy", []engine.ProposeRequest{
		req(f.target("personal"), "set flag on"), req(kv2, "set schema v2"), req(f.target("personal"), "set other x")})
	if err != nil || cs.State != model.CSApproved {
		t.Fatalf("propose: %v %+v", err, cs)
	}
	// Make step 3 fail at apply time (drift on its key).
	if err := os.WriteFile(f.target("personal").Config["path"], []byte(`{"other":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err = f.eng.ApplyChangeset(ctx, cs.ID, targets, "test")
	if err == nil || cs.State != model.CSCompensated || cs.FailedAt != 3 {
		t.Fatalf("saga: %v %+v", err, cs)
	}
	for i, id := range cs.Changes[:2] {
		c, _ := f.eng.Get(ctx, id)
		if c.State != model.Undone {
			t.Fatalf("step %d not compensated: %s", i+1, c.State)
		}
	}
	b, _ := os.ReadFile(kv2.Config["path"])
	if strings.Contains(string(b), "v2") {
		t.Fatalf("kv2 still has step 2's effect: %s", b)
	}

	// A clean changeset applies fully and can be undone as a unit.
	cs, _, _ = f.eng.ProposeChangeset(ctx, "deploy2", []engine.ProposeRequest{req(f.target("personal"), "set a 1"), req(kv2, "set b 2")})
	if cs, err = f.eng.ApplyChangeset(ctx, cs.ID, targets, "test"); err != nil || cs.State != model.CSApplied {
		t.Fatalf("apply: %v %+v", err, cs)
	}
	if cs, err = f.eng.UndoChangeset(ctx, cs.ID, targets, "test"); err != nil || cs.State != model.CSCompensated {
		t.Fatalf("undo: %v %+v", err, cs)
	}
	// A changeset with an unapproved step does not start.
	cs, _, _ = f.eng.ProposeChangeset(ctx, "prod", []engine.ProposeRequest{req(f.target("personal"), "set c 1"), req(f.target("prod"), "set d 1")})
	if cs.State != model.CSRejected {
		t.Fatalf("prod step without incident should reject the changeset: %s", cs.State)
	}
	if _, err := f.eng.ApplyChangeset(ctx, cs.ID, targets, "test"); err == nil {
		t.Fatal("rejected changeset applied")
	}
}
