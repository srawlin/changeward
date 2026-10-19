// Package engine drives the change state machine shared by both lanes:
// proposed → planned → rehearsed → (auto-approved | awaiting-approval →
// approved | rejected) → applying → applied → verifying → verified, with
// failed → undoing → undone and manual undo from applied or verified.
//
// Every transition is persisted together with an append-only, hash-chained
// event. The engine never holds target locks while waiting for a human:
// plans are computed in rolled-back transactions and apply re-checks
// preconditions.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/changegate/changegate/internal/model"
	"github.com/changegate/changegate/internal/policy"
	"github.com/changegate/changegate/internal/signals"
	"github.com/changegate/changegate/internal/store"
	"github.com/changegate/changegate/pkg/adapter"
)

// Registry resolves adapters by name.
type Registry interface {
	Adapter(name string) (adapter.Adapter, error)
}

// MapRegistry is a Registry backed by a map.
type MapRegistry map[string]adapter.Adapter

// Adapter implements Registry.
func (m MapRegistry) Adapter(name string) (adapter.Adapter, error) {
	a, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("no adapter %q", name)
	}
	return a, nil
}

// Notifier is told when a change needs approval (e.g. to post to Slack).
type Notifier interface {
	NotifyPending(ctx context.Context, c *model.Change) error
}

// Engine drives changes through their lifecycle.
type Engine struct {
	Store    store.Store
	Adapters Registry
	Policy   *policy.Engine
	Notifier Notifier
	PlanTTL  time.Duration
	Now      func() time.Time
	// Signals, if set, is consulted when policy would auto-approve a change
	// that carries a stated intent. Signals can only tighten the decision.
	Signals          signals.Provider
	SignalThresholds signals.Thresholds
	// SignalEnvs limits where signals are evaluated (nil: every environment
	// except prod-restricted).
	SignalEnvs map[string]bool
}

// Errors returned by the engine.
var (
	ErrExpired      = errors.New("plan expired; propose the change again to re-plan")
	ErrPlanTampered = errors.New("plan content does not match its hash")
	ErrNotApproved  = errors.New("change is not approved")
)

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) ts() string { return e.now().UTC().Format(time.RFC3339Nano) }

func (e *Engine) transition(ctx context.Context, c *model.Change, to model.State, ev model.Event) error {
	if !model.CanTransition(c.State, to) {
		return fmt.Errorf("change %s: cannot move from %s to %s", c.ID, c.State, to)
	}
	ev.FromState, ev.ToState = c.State, to
	ev.ChangeID, ev.SessionID = c.ID, c.SessionID
	if ev.Type == "" {
		ev.Type = "change." + string(to)
	}
	if ev.PlanHash == "" && c.Plan != nil {
		ev.PlanHash = c.Plan.Hash
	}
	prev := c.State
	c.State = to
	c.UpdatedAt = e.ts()
	if err := e.Store.UpdateChange(ctx, c, &ev); err != nil {
		c.State = prev
		return err
	}
	return nil
}

func jsonData(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ProposeRequest asks the engine to plan and decide one change.
type ProposeRequest struct {
	Target     adapter.Target
	Operation  adapter.Operation
	Principal  model.Principal
	Lane       string
	SessionID  string
	Provenance model.Provenance
	// PolicyExtra adds fields to the policy `change` variable (e.g. spend_usd).
	PolicyExtra map[string]any
	// RehearsalPassed overrides the rehearsal result (Lane A supplies it).
	RehearsalPassed *bool
}

// Propose creates a change, plans it, and runs policy. The returned change
// is auto-approved, awaiting approval, or rejected.
func (e *Engine) Propose(ctx context.Context, req ProposeRequest) (*model.Change, error) {
	if !adapter.ValidEnv(req.Target.Env) {
		return nil, fmt.Errorf("target %q has invalid environment class %q", req.Target.Name, req.Target.Env)
	}
	ad, err := e.Adapters.Adapter(req.Target.Adapter)
	if err != nil {
		return nil, err
	}
	lane := req.Lane
	if lane == "" {
		lane = model.LaneDirect
	}
	prov := req.Provenance
	prov.Lane, prov.SessionID = lane, req.SessionID
	c := &model.Change{
		ID: adapter.NewID("c"), SessionID: req.SessionID, State: model.Proposed,
		Principal: req.Principal, Provenance: prov, Target: req.Target.Redacted(),
		Operation: req.Operation, CreatedAt: e.ts(),
	}
	c.UpdatedAt = c.CreatedAt
	actor := req.Principal.String()
	if err := e.Store.CreateChange(ctx, c, &model.Event{
		Type: "change.proposed", ChangeID: c.ID, SessionID: c.SessionID, ToState: model.Proposed,
		Actor: actor, Reason: req.Operation.Intent, Data: jsonData(map[string]any{"operation": req.Operation, "target": c.Target, "principal": c.Principal, "provenance": prov}),
	}); err != nil {
		return nil, err
	}

	plan, err := ad.Plan(ctx, req.Target, req.Operation)
	if err != nil {
		c.Error = err.Error()
		_ = e.transition(ctx, c, model.Rejected, model.Event{Actor: "engine", Reason: "plan failed: " + err.Error()})
		return c, fmt.Errorf("plan: %w", err)
	}
	if plan.Env != req.Target.Env || plan.Target != req.Target.Name {
		return nil, fmt.Errorf("adapter returned a plan for %s/%s, expected %s/%s", plan.Target, plan.Env, req.Target.Name, req.Target.Env)
	}
	if plan.Hash != adapter.HashPlan(plan) {
		return nil, ErrPlanTampered
	}
	if plan.ExpiresAt == "" {
		ttl := e.PlanTTL
		if ttl == 0 {
			ttl = policy.DefaultPlanExpiry()
		}
		plan.ExpiresAt = e.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	c.Plan = plan
	if err := e.transition(ctx, c, model.Planned, model.Event{Actor: "adapter:" + plan.Adapter,
		Data: jsonData(map[string]any{"plan_id": plan.ID, "reversibility": plan.Reversibility, "blast_rows": plan.BlastRows, "effects": plan.Effects, "warnings": plan.Warnings, "tier": plan.RehearsalTier})}); err != nil {
		return nil, err
	}

	rehearsed := plan.RehearsalTier != "" && plan.RehearsalTier != "static"
	if req.RehearsalPassed != nil {
		rehearsed = *req.RehearsalPassed
	}
	if rehearsed {
		if err := e.transition(ctx, c, model.Rehearsed, model.Event{Actor: "adapter:" + plan.Adapter, Reason: "rehearsal tier " + plan.RehearsalTier}); err != nil {
			return nil, err
		}
	}

	sessIn, err := e.sessionInput(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	d := e.Policy.Decide(policy.Input{
		Change:    policy.ChangeInput(plan, lane, rehearsed, req.PolicyExtra),
		Principal: map[string]any{"kind": req.Principal.Kind, "agent": req.Principal.Agent, "human": req.Principal.Human},
		Session:   sessIn,
	})
	if d.Decision == policy.Approve {
		d = e.applySignals(ctx, d, plan, lane, rehearsed, req, sessIn)
	}
	c.Decision = &d
	to := model.AwaitingApproval
	switch d.Decision {
	case policy.Approve:
		to = model.AutoApproved
	case policy.Deny:
		to = model.Rejected
	}
	if err := e.transition(ctx, c, to, model.Event{Type: "change.decided", Actor: "policy", Reason: join(d.Reasons), Data: jsonData(d)}); err != nil {
		return nil, err
	}
	if to == model.AwaitingApproval && e.Notifier != nil {
		if err := e.Notifier.NotifyPending(ctx, c); err != nil {
			_ = e.Store.AppendEvent(ctx, &model.Event{Type: "notify.failed", ChangeID: c.ID, SessionID: c.SessionID, Actor: "engine", Reason: err.Error()})
		}
	}
	return c, nil
}

// applySignals asks the signal provider about an auto-approved change and
// tightens the decision if a signal is over its threshold (or if a user
// policy rule over change.signals says so). It never loosens a decision,
// and a provider failure leaves the decision unchanged.
func (e *Engine) applySignals(ctx context.Context, d model.Decision, plan *adapter.Plan, lane string, rehearsed bool, req ProposeRequest, sessIn map[string]any) model.Decision {
	if e.Signals == nil || strings.TrimSpace(req.Operation.Intent) == "" {
		return d
	}
	env := req.Target.Env
	if e.SignalEnvs != nil && !e.SignalEnvs[env] || e.SignalEnvs == nil && env == adapter.EnvProdRestricted {
		return d
	}
	var kinds []string
	for _, s := range plan.Classification.Statements {
		kinds = append(kinds, s.Kind)
	}
	sig, err := e.Signals.Evaluate(ctx, signals.Input{Intent: req.Operation.Intent, SQL: req.Operation.Payload, Effect: plan.Diff, Env: env,
		Reversibility: string(plan.Reversibility), Statements: kinds})
	if err != nil {
		d.SignalError = e.Signals.Name() + ": " + err.Error()
		return d
	}
	d.Signals = sig
	// User rules may reference change.signals.
	sm := map[string]any{}
	for k, v := range sig {
		sm[k] = v
	}
	extra := map[string]any{"signals": sm}
	for k, v := range req.PolicyExtra {
		extra[k] = v
	}
	d2 := e.Policy.Decide(policy.Input{
		Change:    policy.ChangeInput(plan, lane, rehearsed, extra),
		Principal: map[string]any{"kind": req.Principal.Kind, "agent": req.Principal.Agent, "human": req.Principal.Human},
		Session:   sessIn,
	})
	if policy.Stricter(d2.Decision, d.Decision) {
		d2.Signals = sig
		d = d2
	}
	th := e.SignalThresholds
	if th == (signals.Thresholds{}) {
		th = signals.DefaultThresholds
	}
	if reasons := th.Tighten(sig); len(reasons) > 0 && d.Decision == policy.Approve {
		d.Decision = policy.Require
		d.Rules = append(d.Rules, "signals/"+e.Signals.Name())
		d.Reasons = append(d.Reasons, reasons...)
		d.Approvers = []string{"owner"}
		d.Quorum = 1
		d.Channel = policy.DefaultChannel(env)
	}
	return d
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}

func (e *Engine) sessionInput(ctx context.Context, id string) (map[string]any, error) {
	m := map[string]any{"id": id, "incident_ref": "", "has_checkpoint": false, "spend_usd": 0.0, "changes": int64(0)}
	if id == "" {
		return m, nil
	}
	s, err := e.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	m["incident_ref"] = s.IncidentRef
	m["has_checkpoint"] = s.CheckpointID != ""
	cs, err := e.Store.ListChanges(ctx, store.ChangeFilter{SessionID: id})
	if err != nil {
		return nil, err
	}
	m["changes"] = int64(len(cs))
	spend := 0.0
	for _, c := range cs {
		if c.State == model.Verified || c.State == model.Applied {
			if v, ok := c.Plan.Facts["spend_usd"]; ok {
				var f float64
				_, _ = fmt.Sscan(v, &f)
				spend += f
			}
		}
	}
	m["spend_usd"] = spend
	return m, nil
}

// ApproveRequest records a human decision on a plan.
type ApproveRequest struct {
	PlanHash string // must equal the change's current plan hash
	Approver string // identity, e.g. "user:alice" or "slack:U123"
	Channel  string // terminal, mcp-elicitation, slack, web, github
	Approved bool
	Reason   string
}

// InBandChannels are rendered by a client the agent may influence (§6.6).
var InBandChannels = map[string]bool{"mcp-elicitation": true, "agent-prompt": true}

// Approve records an approval or rejection bound to the exact plan hash.
func (e *Engine) Approve(ctx context.Context, ref string, req ApproveRequest) (*model.Change, error) {
	c, err := e.Store.GetChange(ctx, ref)
	if err != nil {
		return nil, err
	}
	if c.State != model.AwaitingApproval {
		return c, fmt.Errorf("change %s is %s, not awaiting approval", c.ID, c.State)
	}
	if req.PlanHash == "" || req.PlanHash != c.Plan.Hash {
		return c, errors.New("approval is for a different plan; approvals bind to the exact plan hash")
	}
	if adapter.HashPlan(c.Plan) != c.Plan.Hash {
		return c, ErrPlanTampered
	}
	if e.expired(c.Plan) {
		return c, ErrExpired
	}
	if req.Approver == "" {
		return c, errors.New("approver identity is required")
	}
	if c.Decision != nil && c.Decision.Channel == policy.ChannelOutOfBand && InBandChannels[req.Channel] {
		return c, fmt.Errorf("policy requires out-of-band approval; %s cannot approve this change", req.Channel)
	}
	// The agent can never approve its own change. Inline confirmation by the
	// delegating human is the point of an inline decision; out-of-band
	// approval must come from someone other than the proposer (§6.6).
	outOfBand := c.Decision != nil && c.Decision.Channel == policy.ChannelOutOfBand
	if req.Approver == c.Principal.Agent || (outOfBand && req.Approver == c.Principal.Human) {
		return c, errors.New("separation of duties: the proposer cannot approve its own change")
	}
	for _, a := range c.Approvals {
		if a.Approver == req.Approver && a.PlanHash == c.Plan.Hash {
			return c, fmt.Errorf("%s has already decided on this plan", req.Approver)
		}
	}
	ap := model.Approval{ID: adapter.NewID("a"), ChangeID: c.ID, PlanHash: req.PlanHash, Approver: req.Approver,
		Channel: req.Channel, Approved: req.Approved, Reason: req.Reason, At: e.ts()}
	c.Approvals = append(c.Approvals, ap)
	if !req.Approved {
		return c, e.transition(ctx, c, model.Rejected, model.Event{Type: "change.rejected", Actor: req.Approver, Reason: req.Reason, Data: jsonData(ap)})
	}
	quorum := 1
	if c.Decision != nil && c.Decision.Quorum > 1 {
		quorum = c.Decision.Quorum
	}
	if countApprovals(c) < quorum {
		ev := &model.Event{Type: "change.approval", ChangeID: c.ID, SessionID: c.SessionID, FromState: c.State, ToState: c.State,
			Actor: req.Approver, PlanHash: c.Plan.Hash, Reason: fmt.Sprintf("approval %d of %d", countApprovals(c), quorum), Data: jsonData(ap)}
		c.UpdatedAt = e.ts()
		return c, e.Store.UpdateChange(ctx, c, ev)
	}
	return c, e.transition(ctx, c, model.Approved, model.Event{Actor: req.Approver, Reason: req.Reason, Data: jsonData(ap)})
}

func countApprovals(c *model.Change) int {
	seen := map[string]bool{}
	for _, a := range c.Approvals {
		if a.Approved && a.PlanHash == c.Plan.Hash {
			seen[a.Approver] = true
		}
	}
	return len(seen)
}

func (e *Engine) expired(p *adapter.Plan) bool {
	if p.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, p.ExpiresAt)
	return err != nil || e.now().After(t)
}

// Apply applies an approved change through its adapter, then verifies it.
// A failed verification triggers automatic undo for reversible receipts.
// target must carry the connection config (the stored change does not).
func (e *Engine) Apply(ctx context.Context, ref string, target adapter.Target, actor string) (*model.Change, error) {
	c, err := e.Store.GetChange(ctx, ref)
	if err != nil {
		return nil, err
	}
	if c.State != model.AutoApproved && c.State != model.Approved {
		if c.State == model.AwaitingApproval {
			return c, ErrNotApproved
		}
		return c, fmt.Errorf("change %s is %s; only approved changes can be applied", c.ID, c.State)
	}
	if adapter.HashPlan(c.Plan) != c.Plan.Hash {
		return c, ErrPlanTampered
	}
	if c.State == model.Approved {
		quorum := 1
		if c.Decision != nil && c.Decision.Quorum > 1 {
			quorum = c.Decision.Quorum
		}
		if countApprovals(c) < quorum {
			return c, ErrNotApproved
		}
	}
	if e.expired(c.Plan) {
		return c, ErrExpired
	}
	if err := checkTarget(c, target); err != nil {
		return c, err
	}
	ad, err := e.Adapters.Adapter(c.Plan.Adapter)
	if err != nil {
		return c, err
	}
	if err := e.transition(ctx, c, model.Applying, model.Event{Actor: actor}); err != nil {
		return c, err
	}
	rc, err := ad.Apply(ctx, target, c.Plan)
	if err != nil {
		c.Error = err.Error()
		var ce *adapter.ConflictError
		reason := "apply failed: " + err.Error()
		if errors.As(err, &ce) {
			reason = "precondition failed (drift): " + err.Error()
		}
		_ = e.transition(ctx, c, model.Failed, model.Event{Actor: "adapter:" + c.Plan.Adapter, Reason: reason})
		return c, err
	}
	c.Receipt = rc
	c.Error = ""
	if err := e.transition(ctx, c, model.Applied, model.Event{Actor: "adapter:" + c.Plan.Adapter,
		Data: jsonData(map[string]any{"receipt_id": rc.ID, "rows_affected": rc.RowsAffected, "reversibility": rc.Reversibility})}); err != nil {
		return c, err
	}
	if err := e.transition(ctx, c, model.Verifying, model.Event{Actor: "engine"}); err != nil {
		return c, err
	}
	v, err := ad.Verify(ctx, target, c.Plan, rc)
	if err != nil {
		v = &adapter.Verification{Passed: false, Checks: []adapter.Check{{Name: "verify", Passed: false, Detail: err.Error()}}}
	}
	c.Verification = v
	if v.Passed {
		return c, e.transition(ctx, c, model.Verified, model.Event{Actor: "adapter:" + c.Plan.Adapter, Data: jsonData(v)})
	}
	if err := e.transition(ctx, c, model.Failed, model.Event{Actor: "adapter:" + c.Plan.Adapter, Reason: "verification failed", Data: jsonData(v)}); err != nil {
		return c, err
	}
	if rc.Reversibility == adapter.Reversible {
		uc, uerr := e.Undo(ctx, c.ID, target, "engine:auto-rollback")
		if uerr != nil {
			return uc, fmt.Errorf("verification failed and automatic undo failed: %w", uerr)
		}
		return uc, errors.New("verification failed; change was rolled back automatically")
	}
	return c, errors.New("verification failed; change is not automatically reversible")
}

func checkTarget(c *model.Change, t adapter.Target) error {
	if t.Name != c.Target.Name || t.Env != c.Target.Env || t.Adapter != c.Target.Adapter {
		return fmt.Errorf("change %s was planned for target %s (%s), not %s (%s)", c.ID, c.Target.Name, c.Target.Env, t.Name, t.Env)
	}
	return nil
}

// Undo reverses an applied change using its receipt's undo handle. Undo is
// guarded: the adapter refuses with a ConflictError if the target no longer
// matches the after-image, and the change stays in its previous state.
func (e *Engine) Undo(ctx context.Context, ref string, target adapter.Target, actor string) (*model.Change, error) {
	c, err := e.Store.GetChange(ctx, ref)
	if err != nil {
		return nil, err
	}
	switch c.State {
	case model.Applied, model.Verified, model.Failed:
	default:
		return c, fmt.Errorf("change %s is %s; only applied changes can be undone", c.ID, c.State)
	}
	if c.Receipt == nil {
		return c, fmt.Errorf("change %s has no receipt; nothing was applied", c.ID)
	}
	if err := checkTarget(c, target); err != nil {
		return c, err
	}
	ad, err := e.Adapters.Adapter(c.Plan.Adapter)
	if err != nil {
		return c, err
	}
	prev := c.State
	if err := e.transition(ctx, c, model.Undoing, model.Event{Actor: actor}); err != nil {
		return c, err
	}
	rc, err := ad.Undo(ctx, target, c.Plan, c.Receipt)
	if err != nil {
		c.Error = "undo failed: " + err.Error()
		_ = e.transition(ctx, c, prev, model.Event{Type: "change.undo_failed", Actor: "adapter:" + c.Plan.Adapter, Reason: err.Error()})
		return c, err
	}
	c.UndoReceipt = rc
	c.Error = ""
	return c, e.transition(ctx, c, model.Undone, model.Event{Actor: actor, Data: jsonData(map[string]any{"receipt_id": rc.ID, "rows_affected": rc.RowsAffected})})
}

// Get returns a change by reference.
func (e *Engine) Get(ctx context.Context, ref string) (*model.Change, error) {
	return e.Store.GetChange(ctx, ref)
}
