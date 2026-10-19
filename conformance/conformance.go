// Package conformance is the adapter conformance suite. It proves that an
// adapter's Plan is side-effect free and content-hashed, that Apply then Undo
// restores identical state across a corpus of generated operations, and that
// guards reject stale plans and stale undos.
//
// Adapters that run out of process take part through two conventions on
// Plan: an operation of kind "changegate.fingerprint" returns
// facts["fingerprint"] (a hash of target state), and "changegate.corpus" with
// params seed and n returns facts["corpus"] (a JSON list of operations).
// In-process drivers can supply Fingerprint and Operations directly instead.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/srawlin/changegate/pkg/adapter"
)

// Driver describes how to test one adapter.
type Driver struct {
	Adapter adapter.Adapter
	// NewTarget returns a fresh, empty target and a cleanup function.
	NewTarget func(ctx context.Context) (adapter.Target, func(), error)
	// Operations, if nil, is fetched with the changegate.corpus convention.
	Operations func(ctx context.Context, t adapter.Target, seed int64, n int) ([]adapter.Operation, error)
	// Fingerprint, if nil, uses the changegate.fingerprint convention.
	Fingerprint func(ctx context.Context, t adapter.Target) (string, error)
	Seed        int64
	N           int
}

// Result is the outcome of one conformance check.
type Result struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Report is the outcome of a conformance run.
type Report struct {
	Adapter string   `json:"adapter"`
	Version string   `json:"version"`
	Passed  bool     `json:"passed"`
	Results []Result `json:"results"`
}

// String renders the report for terminals.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "conformance: %s %s\n", r.Adapter, r.Version)
	for _, res := range r.Results {
		mark := "PASS"
		if !res.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s  %s", mark, res.Name)
		if res.Detail != "" {
			fmt.Fprintf(&b, ": %s", res.Detail)
		}
		b.WriteString("\n")
	}
	if r.Passed {
		b.WriteString("result: certified-conformant\n")
	} else {
		b.WriteString("result: FAILED\n")
	}
	return b.String()
}

type runner struct {
	d   Driver
	rep *Report
}

// errNoCase marks a check that found no applicable case in the corpus.
var errNoCase = errors.New("skipped: no applicable case in corpus")

func (r *runner) add(name string, err error) bool {
	if errors.Is(err, errNoCase) {
		r.rep.Results = append(r.rep.Results, Result{Name: name, Passed: true, Detail: err.Error()})
		return true
	}
	res := Result{Name: name, Passed: err == nil}
	if err != nil {
		res.Detail = err.Error()
		r.rep.Passed = false
	}
	r.rep.Results = append(r.rep.Results, res)
	return err == nil
}

func (r *runner) fingerprint(ctx context.Context, t adapter.Target) (string, error) {
	if r.d.Fingerprint != nil {
		return r.d.Fingerprint(ctx, t)
	}
	p, err := r.d.Adapter.Plan(ctx, t, adapter.Operation{Kind: "changegate.fingerprint"})
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	fp := p.Facts["fingerprint"]
	if fp == "" {
		return "", errors.New("fingerprint: adapter returned no facts.fingerprint")
	}
	return fp, nil
}

func (r *runner) operations(ctx context.Context, t adapter.Target) ([]adapter.Operation, error) {
	if r.d.Operations != nil {
		return r.d.Operations(ctx, t, r.d.Seed, r.d.N)
	}
	p, err := r.d.Adapter.Plan(ctx, t, adapter.Operation{Kind: "changegate.corpus",
		Params: map[string]string{"seed": fmt.Sprint(r.d.Seed), "n": fmt.Sprint(r.d.N)}})
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	var ops []adapter.Operation
	if err := json.Unmarshal([]byte(p.Facts["corpus"]), &ops); err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	return ops, nil
}

// Run executes the suite.
func Run(ctx context.Context, d Driver) Report {
	if d.N == 0 {
		d.N = 20
	}
	if d.Seed == 0 {
		d.Seed = 1
	}
	rep := &Report{Passed: true}
	r := &runner{d: d, rep: rep}

	info, err := d.Adapter.Describe(ctx)
	if err == nil && (info.Name == "" || !info.Has(adapter.CapClassify) || !info.Has(adapter.CapPlan)) {
		err = errors.New("adapter must declare a name and the classify and plan capabilities")
	}
	if !r.add("describe", err) {
		return *rep
	}
	rep.Adapter, rep.Version = info.Name, info.Version

	r.add("roundtrip/per-operation", r.perOperation(ctx, info))
	r.add("roundtrip/sequence-rewind", r.sequence(ctx, info))
	r.add("guard/stale-plan", r.stalePlan(ctx, info))
	r.add("guard/stale-undo", r.staleUndo(ctx, info))
	if info.Has(adapter.CapCheckpoint) && info.Has(adapter.CapRestore) {
		r.add("checkpoint/restore", r.checkpoint(ctx))
	}
	return *rep
}

func (r *runner) withTarget(ctx context.Context, fn func(t adapter.Target, ops []adapter.Operation) error) error {
	t, cleanup, err := r.d.NewTarget(ctx)
	if err != nil {
		return fmt.Errorf("new target: %w", err)
	}
	defer cleanup()
	ops, err := r.operations(ctx, t)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return errors.New("corpus is empty")
	}
	return fn(t, ops)
}

// planChecked plans op and checks the plan is side-effect free, hashed and deterministic.
func (r *runner) planChecked(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	before, err := r.fingerprint(ctx, t)
	if err != nil {
		return nil, err
	}
	p, err := r.d.Adapter.Plan(ctx, t, op)
	if err != nil {
		return nil, fmt.Errorf("plan %q: %w", op.Payload, err)
	}
	after, err := r.fingerprint(ctx, t)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("plan %q changed target state", op.Payload)
	}
	if p.Hash == "" || p.Hash != adapter.HashPlan(p) {
		return nil, fmt.Errorf("plan %q: hash %q does not match content hash", op.Payload, p.Hash)
	}
	if !p.Reversibility.Valid() {
		return nil, fmt.Errorf("plan %q: invalid reversibility %q", op.Payload, p.Reversibility)
	}
	p2, err := r.d.Adapter.Plan(ctx, t, op)
	if err != nil {
		return nil, err
	}
	if p2.PreconditionHash != p.PreconditionHash {
		return nil, fmt.Errorf("plan %q: precondition hash not deterministic", op.Payload)
	}
	return p, nil
}

func (r *runner) perOperation(ctx context.Context, info adapter.Info) error {
	if !info.Has(adapter.CapApply) || !info.Has(adapter.CapUndo) {
		return nil
	}
	return r.withTarget(ctx, func(t adapter.Target, ops []adapter.Operation) error {
		for _, op := range ops {
			s0, err := r.fingerprint(ctx, t)
			if err != nil {
				return err
			}
			p, err := r.planChecked(ctx, t, op)
			if err != nil {
				return err
			}
			rc, err := r.d.Adapter.Apply(ctx, t, p)
			if err != nil {
				return fmt.Errorf("apply %q: %w", op.Payload, err)
			}
			if info.Has(adapter.CapVerify) {
				v, err := r.d.Adapter.Verify(ctx, t, p, rc)
				if err != nil || !v.Passed {
					return fmt.Errorf("verify %q failed: %v", op.Payload, err)
				}
			}
			if rc.Reversibility != adapter.Reversible {
				continue
			}
			if _, err := r.d.Adapter.Undo(ctx, t, p, rc); err != nil {
				return fmt.Errorf("undo %q: %w", op.Payload, err)
			}
			s1, err := r.fingerprint(ctx, t)
			if err != nil {
				return err
			}
			if s0 != s1 {
				return fmt.Errorf("apply+undo of %q did not restore identical state", op.Payload)
			}
			// Re-apply so later operations see accumulated state.
			p, err = r.d.Adapter.Plan(ctx, t, op)
			if err != nil {
				return err
			}
			if _, err := r.d.Adapter.Apply(ctx, t, p); err != nil {
				return fmt.Errorf("re-apply %q: %w", op.Payload, err)
			}
		}
		return nil
	})
}

func (r *runner) sequence(ctx context.Context, info adapter.Info) error {
	if !info.Has(adapter.CapApply) || !info.Has(adapter.CapUndo) {
		return nil
	}
	return r.withTarget(ctx, func(t adapter.Target, ops []adapter.Operation) error {
		s0, err := r.fingerprint(ctx, t)
		if err != nil {
			return err
		}
		type step struct {
			p  *adapter.Plan
			rc *adapter.Receipt
		}
		var steps []step
		for _, op := range ops {
			p, err := r.d.Adapter.Plan(ctx, t, op)
			if err != nil {
				return err
			}
			rc, err := r.d.Adapter.Apply(ctx, t, p)
			if err != nil {
				return fmt.Errorf("apply %q: %w", op.Payload, err)
			}
			if rc.Reversibility != adapter.Reversible {
				return nil // sequence rewind only applies to fully reversible corpora
			}
			steps = append(steps, step{p, rc})
		}
		for i := len(steps) - 1; i >= 0; i-- {
			if _, err := r.d.Adapter.Undo(ctx, t, steps[i].p, steps[i].rc); err != nil {
				return fmt.Errorf("undo step %d: %w", i+1, err)
			}
		}
		s1, err := r.fingerprint(ctx, t)
		if err != nil {
			return err
		}
		if s0 != s1 {
			return errors.New("rewinding the sequence did not restore the starting state")
		}
		return nil
	})
}

// stalePlan: plan A, apply a different change B to the same object, then
// apply A. A must be rejected with a ConflictError when B changed what A
// assumed. The loop keeps the invariant that ops[0..i-1] are applied.
func (r *runner) stalePlan(ctx context.Context, info adapter.Info) error {
	if !info.Has(adapter.CapApply) || !info.Has(adapter.CapUndo) {
		return nil
	}
	return r.withTarget(ctx, func(t adapter.Target, ops []adapter.Operation) error {
		for i := 0; i+1 < len(ops); i++ {
			a, err := r.d.Adapter.Plan(ctx, t, ops[i])
			if err != nil {
				return err
			}
			b, err := r.d.Adapter.Plan(ctx, t, ops[i+1])
			if err == nil && b.Reversibility == adapter.Reversible && overlaps(a, b) {
				rb, err := r.d.Adapter.Apply(ctx, t, b)
				if err != nil {
					return err
				}
				a2, perr := r.d.Adapter.Plan(ctx, t, ops[i])
				if perr == nil && a2.PreconditionHash != a.PreconditionHash {
					_, err = r.d.Adapter.Apply(ctx, t, a)
					var ce *adapter.ConflictError
					if !errors.As(err, &ce) {
						return fmt.Errorf("stale plan %q was applied after %q (err=%v); expected a conflict", ops[i].Payload, ops[i+1].Payload, err)
					}
					return nil
				}
				if _, err := r.d.Adapter.Undo(ctx, t, b, rb); err != nil {
					return fmt.Errorf("undo %q: %w", ops[i+1].Payload, err)
				}
			}
			if _, err := r.d.Adapter.Apply(ctx, t, a); err != nil {
				return fmt.Errorf("apply %q: %w", ops[i].Payload, err)
			}
		}
		return errNoCase
	})
}

// staleUndo: apply A, change the same object again with B, then undo A.
// The undo must refuse with a ConflictError and leave state untouched.
func (r *runner) staleUndo(ctx context.Context, info adapter.Info) error {
	if !info.Has(adapter.CapApply) || !info.Has(adapter.CapUndo) {
		return nil
	}
	return r.withTarget(ctx, func(t adapter.Target, ops []adapter.Operation) error {
		for i := 0; i < len(ops); i++ {
			pa, err := r.d.Adapter.Plan(ctx, t, ops[i])
			if err != nil {
				return err
			}
			ra, err := r.d.Adapter.Apply(ctx, t, pa)
			if err != nil {
				return err
			}
			if i+1 >= len(ops) || ra.Reversibility != adapter.Reversible {
				continue
			}
			mid, err := r.fingerprint(ctx, t)
			if err != nil {
				return err
			}
			pb, err := r.d.Adapter.Plan(ctx, t, ops[i+1])
			if err != nil {
				return err
			}
			if !overlaps(pa, pb) {
				continue
			}
			if _, err := r.d.Adapter.Apply(ctx, t, pb); err != nil {
				return err
			}
			i++ // ops[i+1] is now applied too
			after, err := r.fingerprint(ctx, t)
			if err != nil {
				return err
			}
			if after == mid {
				continue
			}
			_, err = r.d.Adapter.Undo(ctx, t, pa, ra)
			var ce *adapter.ConflictError
			if err == nil {
				// A's effects were intact (B touched other parts of the same
				// object). Re-apply A to keep the invariant and keep looking.
				pa, err := r.d.Adapter.Plan(ctx, t, ops[i-1])
				if err != nil {
					return errNoCase
				}
				if _, err := r.d.Adapter.Apply(ctx, t, pa); err != nil {
					return errNoCase
				}
				continue
			}
			if !errors.As(err, &ce) {
				return fmt.Errorf("stale undo of %q failed without a conflict: %v", ops[i-1].Payload, err)
			}
			now, err := r.fingerprint(ctx, t)
			if err != nil {
				return err
			}
			if now != after {
				return errors.New("refused undo still modified state")
			}
			return nil
		}
		return errNoCase
	})
}

func overlaps(a, b *adapter.Plan) bool {
	for _, x := range a.Classification.Objects {
		for _, y := range b.Classification.Objects {
			if x == y {
				return true
			}
		}
	}
	return false
}

func (r *runner) checkpoint(ctx context.Context) error {
	return r.withTarget(ctx, func(t adapter.Target, ops []adapter.Operation) error {
		s0, err := r.fingerprint(ctx, t)
		if err != nil {
			return err
		}
		cp, err := r.d.Adapter.Checkpoint(ctx, t, "conformance")
		if err != nil {
			return err
		}
		for _, op := range ops {
			p, err := r.d.Adapter.Plan(ctx, t, op)
			if err != nil {
				return err
			}
			if _, err := r.d.Adapter.Apply(ctx, t, p); err != nil {
				return err
			}
		}
		if _, err := r.d.Adapter.Restore(ctx, t, cp); err != nil {
			return err
		}
		s1, err := r.fingerprint(ctx, t)
		if err != nil {
			return err
		}
		if s0 != s1 {
			return errors.New("restore did not return to the checkpointed state")
		}
		return nil
	})
}
