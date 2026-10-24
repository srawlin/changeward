package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/pkg/adapter"
)

// ProposeChangeset proposes every step (each gets its own plan, evidence
// and policy decision) and groups them. The changeset's decision is the
// strictest of its members. Plans are computed against current state, so
// steps that depend on earlier steps' effects on the same target belong in
// one multi-statement change instead.
func (e *Engine) ProposeChangeset(ctx context.Context, name string, steps []ProposeRequest) (*model.Changeset, []*model.Change, error) {
	if len(steps) == 0 {
		return nil, nil, errors.New("changeset has no steps")
	}
	cs := &model.Changeset{ID: adapter.NewID("cs"), Name: name, State: model.CSPlanned, CreatedAt: e.ts(), SessionID: steps[0].SessionID}
	var changes []*model.Change
	agg := model.Decision{Decision: policy.Approve}
	for i, req := range steps {
		c, err := e.Propose(ctx, req)
		if err != nil {
			return nil, changes, fmt.Errorf("step %d: %w", i+1, err)
		}
		changes = append(changes, c)
		cs.Changes = append(cs.Changes, c.ID)
		if c.Decision != nil && policy.Stricter(c.Decision.Decision, agg.Decision) {
			agg.Decision = c.Decision.Decision
		}
		if c.Decision != nil {
			for _, r := range c.Decision.Reasons {
				agg.Reasons = append(agg.Reasons, fmt.Sprintf("step %d: %s", i+1, r))
			}
		}
	}
	cs.Decision = &agg
	switch agg.Decision {
	case policy.Deny:
		cs.State = model.CSRejected
	case policy.Require:
		cs.State = model.CSAwaiting
	default:
		cs.State = model.CSApproved
	}
	cs.UpdatedAt = e.ts()
	err := e.Store.SaveChangeset(ctx, cs, &model.Event{Type: "changeset." + string(cs.State), SessionID: cs.SessionID, Actor: "policy",
		Reason: fmt.Sprintf("%d steps", len(steps)), Data: jsonData(cs)})
	return cs, changes, err
}

// TargetFunc resolves the connection for a change at apply/undo time.
type TargetFunc func(*model.Change) (adapter.Target, error)

// ApplyChangeset applies every step in order once all are approved. If a
// step fails (apply error or failed verification), the steps already
// applied are undone in reverse order (saga compensation).
func (e *Engine) ApplyChangeset(ctx context.Context, id string, targets TargetFunc, actor string) (*model.Changeset, error) {
	cs, err := e.Store.GetChangeset(ctx, id)
	if err != nil {
		return nil, err
	}
	if cs.State != model.CSApproved && cs.State != model.CSAwaiting {
		return cs, fmt.Errorf("changeset %s is %s", cs.ID, cs.State)
	}
	// Every member must be approved (auto or by a human) before anything runs.
	for i, cid := range cs.Changes {
		c, err := e.Store.GetChange(ctx, cid)
		if err != nil {
			return cs, err
		}
		if c.State != model.AutoApproved && c.State != model.Approved {
			return cs, fmt.Errorf("step %d (%s) is %s; every step must be approved before the changeset is applied", i+1, c.ID, c.State)
		}
	}
	save := func(st model.State, reason string) {
		cs.State, cs.UpdatedAt = st, e.ts()
		_ = e.Store.SaveChangeset(ctx, cs, &model.Event{Type: "changeset." + string(st), SessionID: cs.SessionID, Actor: actor, Reason: reason})
	}
	save(model.CSApplying, "")
	var applied []*model.Change
	for i, cid := range cs.Changes {
		c, err := e.Store.GetChange(ctx, cid)
		if err != nil {
			return cs, err
		}
		t, err := targets(c)
		if err == nil {
			c, err = e.Apply(ctx, cid, t, actor)
		}
		if err == nil && c.State == model.Verified {
			applied = append(applied, c)
			continue
		}
		// Step failed. A failed verification was already rolled back by Apply
		// when reversible; compensate the earlier steps.
		cs.FailedAt, cs.Error = i+1, fmt.Sprintf("step %d: %v", i+1, err)
		save(model.CSCompensating, cs.Error)
		for j := len(applied) - 1; j >= 0; j-- {
			p := applied[j]
			t, terr := targets(p)
			if terr == nil {
				_, terr = e.Undo(ctx, p.ID, t, "engine:saga")
			}
			if terr != nil {
				cs.Error += fmt.Sprintf("; compensation of step %d failed: %v", j+1, terr)
				save(model.CSFailed, cs.Error)
				return cs, errors.New(cs.Error)
			}
		}
		save(model.CSCompensated, cs.Error)
		return cs, errors.New(cs.Error + " (earlier steps were undone)")
	}
	save(model.CSApplied, "")
	return cs, nil
}

// UndoChangeset undoes an applied changeset, last step first.
func (e *Engine) UndoChangeset(ctx context.Context, id string, targets TargetFunc, actor string) (*model.Changeset, error) {
	cs, err := e.Store.GetChangeset(ctx, id)
	if err != nil {
		return nil, err
	}
	if cs.State != model.CSApplied {
		return cs, fmt.Errorf("changeset %s is %s", cs.ID, cs.State)
	}
	for i := len(cs.Changes) - 1; i >= 0; i-- {
		c, err := e.Store.GetChange(ctx, cs.Changes[i])
		if err != nil {
			return cs, err
		}
		t, err := targets(c)
		if err == nil {
			_, err = e.Undo(ctx, c.ID, t, actor)
		}
		if err != nil {
			cs.Error = fmt.Sprintf("undo of step %d failed: %v", i+1, err)
			cs.UpdatedAt = e.ts()
			_ = e.Store.SaveChangeset(ctx, cs, &model.Event{Type: "changeset.undo_failed", SessionID: cs.SessionID, Actor: actor, Reason: cs.Error})
			return cs, errors.New(cs.Error)
		}
	}
	cs.State, cs.UpdatedAt = model.CSCompensated, e.ts()
	return cs, e.Store.SaveChangeset(ctx, cs, &model.Event{Type: "changeset.undone", SessionID: cs.SessionID, Actor: actor})
}
