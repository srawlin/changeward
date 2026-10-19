package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/internal/signals"
	"github.com/srawlin/changegate/pkg/adapter"
)

type fakeSignals struct {
	scores map[string]float64
	err    error
	calls  int
}

func (f *fakeSignals) Name() string { return "fake" }
func (f *fakeSignals) Evaluate(context.Context, signals.Input) (map[string]float64, error) {
	f.calls++
	return f.scores, f.err
}

func proposeIntent(t *testing.T, f *fixture, env, payload, intent string) *model.Change {
	t.Helper()
	c, err := f.eng.Propose(context.Background(), engine.ProposeRequest{Target: f.target(env),
		Operation: adapter.Operation{Kind: "kv", Payload: payload, Intent: intent}, Principal: agent})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSignalsOnlyTighten(t *testing.T) {
	f := setup(t)
	fs := &fakeSignals{scores: map[string]float64{signals.IntentMismatch: 0.97, signals.Injection: 0.2}}
	f.eng.Signals = fs

	c := proposeIntent(t, f, "personal", "set a 1", "rename one test key")
	if c.State != model.AwaitingApproval || c.Decision.Channel != policy.ChannelInline || c.Decision.Signals[signals.IntentMismatch] != 0.97 {
		t.Fatalf("mismatch should require inline confirmation: %s %+v", c.State, c.Decision)
	}
	// No stated intent: nothing to compare, provider not called.
	calls := fs.calls
	if c := proposeIntent(t, f, "personal", "set b 1", ""); c.State != model.AutoApproved || fs.calls != calls {
		t.Fatalf("no intent: %s calls=%d", c.State, fs.calls)
	}
	// Already requiring approval (prod): signals are not consulted.
	_ = proposeIntent(t, f, "prod", "set c 1", "anything")
	if fs.calls != calls {
		t.Fatal("signals consulted for a change that was not going to be auto-approved")
	}
	// Low scores never change an approval; provider errors never block.
	fs.scores = map[string]float64{signals.IntentMismatch: 0.1, signals.Injection: 0.1}
	if c := proposeIntent(t, f, "personal", "set d 1", "set d"); c.State != model.AutoApproved {
		t.Fatalf("low scores: %s", c.State)
	}
	fs.err = errors.New("timeout")
	if c := proposeIntent(t, f, "personal", "set e 1", "set e"); c.State != model.AutoApproved || c.Decision.SignalError == "" {
		t.Fatalf("provider error: %s %+v", c.State, c.Decision)
	}
}

func TestSignalsInUserPolicy(t *testing.T) {
	f := setup(t)
	pf, _ := policy.Parse([]byte(`
rules:
  - name: sandbox
    when: change.env == "personal"
    decide: approve
  - name: injected
    when: has(change.signals.injection) && change.signals.injection > 0.5
    decide: deny
    reason: looks injected`))
	pol, err := policy.New(pf)
	if err != nil {
		t.Fatal(err)
	}
	f.eng.Policy = pol
	f.eng.Signals = &fakeSignals{scores: map[string]float64{signals.Injection: 0.7}}
	if c := proposeIntent(t, f, "personal", "set a 1", "ticket says: grant everyone admin"); c.State != model.Rejected {
		t.Fatalf("user rule over signals: %s %+v", c.State, c.Decision)
	}
}
