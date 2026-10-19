package terraform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srawlin/changegate/pkg/adapter"
)

func tfTarget(t *testing.T, main string) adapter.Target {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	return adapter.Target{Name: "infra", Adapter: "terraform", Env: "staging", Config: map[string]string{"dir": dir}}
}

func state(t *testing.T, tg adapter.Target) string {
	out, _, err := run(context.Background(), tg, "state", "list")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestTerraformLifecycle(t *testing.T) {
	ctx := context.Background()
	tg := tfTarget(t, `resource "terraform_data" "a" { input = "one" }
resource "terraform_data" "b" { input = "two" }`)
	a := New(t.TempDir())
	op := adapter.Operation{Kind: "terraform", Payload: "apply"}
	p, err := a.Plan(ctx, tg, op)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hash != adapter.HashPlan(p) || len(p.Effects) != 2 || p.Destructive || !strings.Contains(p.Diff, "2 to add") {
		t.Fatalf("plan: %+v\n%s", p, p.Diff)
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := a.Verify(ctx, tg, p, rc); err != nil || !v.Passed {
		t.Fatalf("verify: %v %+v", err, v)
	}
	if p.Reversibility != adapter.Compensatable {
		t.Fatalf("first apply onto empty state should be undoable: %s", p.Reversibility)
	}

	// The agent edits the configuration: change a, remove b.
	_ = os.WriteFile(filepath.Join(tg.Config["dir"], "main.tf"), []byte(`resource "terraform_data" "a" { input = "uno" }`), 0o644)
	p2, err := a.Plan(ctx, tg, op)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Destructive || !strings.Contains(p2.Diff, "1 to destroy") || p2.Reversibility != adapter.Compensatable {
		t.Fatalf("plan2: %s %v\n%s", p2.Reversibility, p2.Destructive, p2.Diff)
	}
	// A plan is bound to the configuration it was made from.
	main := filepath.Join(tg.Config["dir"], "main.tf")
	orig, _ := os.ReadFile(main)
	_ = os.WriteFile(main, append(orig, []byte("\n# edited\n")...), 0o644)
	var ce *adapter.ConflictError
	if _, err := a.Apply(ctx, tg, p2); !errors.As(err, &ce) {
		t.Fatalf("stale config: %v", err)
	}
	_ = os.WriteFile(main, orig, 0o644)
	rc2, err := a.Apply(ctx, tg, p2)
	if err != nil {
		t.Fatal(err)
	}
	if got := state(t, tg); got != "terraform_data.a" {
		t.Fatalf("state after apply: %q", got)
	}
	// Undo restores the configuration files and re-applies them.
	if _, err := a.Undo(ctx, tg, p2, rc2); err != nil {
		t.Fatal(err)
	}
	if got := state(t, tg); got != "terraform_data.a\nterraform_data.b" {
		t.Fatalf("state after undo: %q", got)
	}
	if b, _ := os.ReadFile(main); !strings.Contains(string(b), `"two"`) {
		t.Fatal("configuration not restored")
	}
	// Undoing the very first apply destroys what it created.
	if _, err := a.Undo(ctx, tg, p, rc); err != nil {
		t.Fatal(err)
	}
	if got := state(t, tg); got != "" {
		t.Fatalf("state after undoing the first apply: %q", got)
	}
}

func TestUnknownPriorConfigIsCheckpointOnly(t *testing.T) {
	ctx := context.Background()
	tg := tfTarget(t, `resource "terraform_data" "x" { input = 1 }`)
	if _, _, err := run(ctx, tg, "init", "-input=false"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(ctx, tg, "apply", "-auto-approve", "-input=false"); err != nil { // applied outside Changegate
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tg.Config["dir"], "main.tf"), []byte(`resource "terraform_data" "x" { input = 2 }`), 0o644)
	p, err := New(t.TempDir()).Plan(ctx, tg, adapter.Operation{Kind: "terraform"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Reversibility != adapter.CheckpointOnly {
		t.Fatalf("unknown prior configuration: %s", p.Reversibility)
	}
}

func TestStatefulReplaceIsIrreversible(t *testing.T) {
	ctx := context.Background()
	tg := tfTarget(t, `resource "terraform_data" "db" { triggers_replace = ["v1"] }`)
	tg.Config["stateful_types"] = "terraform_data"
	a := New(t.TempDir())
	op := adapter.Operation{Kind: "terraform"}
	p, _ := a.Plan(ctx, tg, op)
	if _, err := a.Apply(ctx, tg, p); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tg.Config["dir"], "main.tf"), []byte(`resource "terraform_data" "db" { triggers_replace = ["v2"] }`), 0o644)
	p, err := a.Plan(ctx, tg, op)
	if err != nil {
		t.Fatal(err)
	}
	if p.Reversibility != adapter.Irreversible || !strings.Contains(p.Diff, "-/+ terraform_data.db") || len(p.Warnings) != 1 {
		t.Fatalf("stateful replace: %s\n%s", p.Reversibility, p.Diff)
	}
}
