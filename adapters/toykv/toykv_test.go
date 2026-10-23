package toykv_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/conformance"
	"github.com/srawlin/changeward/pkg/adapter"
)

func newTarget(t *testing.T) func(context.Context) (adapter.Target, func(), error) {
	return func(context.Context) (adapter.Target, func(), error) {
		dir := t.TempDir()
		return adapter.Target{Name: "kv", Adapter: "toykv", Env: "personal",
			Config: map[string]string{"path": filepath.Join(dir, "kv.json")}}, func() {}, nil
	}
}

func TestConformanceInProcess(t *testing.T) {
	rep := conformance.Run(context.Background(), conformance.Driver{Adapter: toykv.New(), NewTarget: newTarget(t), N: 40})
	if !rep.Passed {
		t.Fatal(rep.String())
	}
	if len(rep.Results) < 5 {
		t.Fatalf("expected all checks to run:\n%s", rep)
	}
}

func TestConformanceOverPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary")
	}
	bin := filepath.Join(t.TempDir(), "changeward-adapter-toykv")
	out, err := exec.Command("go", "build", "-o", bin, "./cmd/changeward-adapter-toykv").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	p, err := adapter.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rep := conformance.Run(context.Background(), conformance.Driver{Adapter: p, NewTarget: newTarget(t)})
	if !rep.Passed {
		t.Fatal(rep.String())
	}
}

// A broken adapter (undo does nothing) must fail conformance.
type brokenUndo struct{ *toykv.Adapter }

func (b brokenUndo) Undo(context.Context, adapter.Target, *adapter.Plan, *adapter.Receipt) (*adapter.Receipt, error) {
	return &adapter.Receipt{Reversibility: adapter.Reversible}, nil
}

func TestConformanceCatchesBrokenUndo(t *testing.T) {
	rep := conformance.Run(context.Background(), conformance.Driver{Adapter: brokenUndo{toykv.New()}, NewTarget: newTarget(t)})
	if rep.Passed {
		t.Fatalf("broken adapter passed:\n%s", rep)
	}
}
