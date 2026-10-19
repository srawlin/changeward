// Package model holds the Change Record types shared by the engine and store.
// They mirror proto/changegate/v1/change.proto.
package model

import (
	"encoding/json"

	"github.com/srawlin/changegate/pkg/adapter"
)

// State is a change lifecycle state.
type State string

// Lifecycle states (spec §4.2).
const (
	Proposed         State = "proposed"
	Planned          State = "planned"
	Rehearsed        State = "rehearsed"
	AutoApproved     State = "auto-approved"
	AwaitingApproval State = "awaiting-approval"
	Approved         State = "approved"
	Rejected         State = "rejected"
	Applying         State = "applying"
	Applied          State = "applied"
	Verifying        State = "verifying"
	Verified         State = "verified"
	Failed           State = "failed"
	Undoing          State = "undoing"
	Undone           State = "undone"
)

// transitions lists every allowed state transition.
var transitions = map[State][]State{
	"":               {Proposed},
	Proposed:         {Planned, Rejected},
	Planned:          {Rehearsed, AutoApproved, AwaitingApproval, Rejected},
	Rehearsed:        {AutoApproved, AwaitingApproval, Rejected},
	AwaitingApproval: {Approved, Rejected},
	AutoApproved:     {Applying},
	Approved:         {Applying},
	Applying:         {Applied, Failed},
	Applied:          {Verifying, Undoing},
	Verifying:        {Verified, Failed},
	Verified:         {Undoing},
	Failed:           {Undoing},
	Undoing:          {Undone, Verified, Applied, Failed},
}

// CanTransition reports whether from → to is allowed.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool { return len(transitions[s]) == 0 }

// Lanes.
const (
	LaneCI     = "ci"
	LaneDirect = "direct"
)

// Principal is who proposed a change: the agent and the delegating human.
type Principal struct {
	Kind  string `json:"kind"` // "agent" or "human"
	Agent string `json:"agent,omitempty"`
	Human string `json:"human,omitempty"`
}

// String renders the principal for logs.
func (p Principal) String() string {
	if p.Agent != "" {
		return p.Agent + " for " + p.Human
	}
	return p.Human
}

// Provenance records where a change came from.
type Provenance struct {
	Lane      string `json:"lane"`
	SessionID string `json:"session_id,omitempty"`
	PR        string `json:"pr,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Host      string `json:"host,omitempty"`
}

// Decision is the policy outcome for a plan.
type Decision struct {
	Decision  string   `json:"decision"` // approve, require, deny
	Rules     []string `json:"rules,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
	Channel   string   `json:"channel,omitempty"` // inline or out-of-band
	Quorum    int      `json:"quorum,omitempty"`
	// Signals are advisory model scores (0..1) that can only tighten a decision.
	Signals     map[string]float64 `json:"signals,omitempty"`
	SignalError string             `json:"signal_error,omitempty"`
}

// Approval is a human decision bound to an exact plan hash.
type Approval struct {
	ID       string `json:"id"`
	ChangeID string `json:"change_id"`
	PlanHash string `json:"plan_hash"`
	Approver string `json:"approver"`
	Channel  string `json:"channel"`
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
	At       string `json:"at"`
}

// Change is the Change Record.
type Change struct {
	ID           string                `json:"id"`
	Seq          int64                 `json:"seq"`
	SessionID    string                `json:"session_id,omitempty"`
	State        State                 `json:"state"`
	Principal    Principal             `json:"principal"`
	Provenance   Provenance            `json:"provenance"`
	Target       adapter.Target        `json:"target"`
	Operation    adapter.Operation     `json:"operation"`
	Plan         *adapter.Plan         `json:"plan,omitempty"`
	Decision     *Decision             `json:"decision,omitempty"`
	Approvals    []Approval            `json:"approvals,omitempty"`
	Receipt      *adapter.Receipt      `json:"receipt,omitempty"`
	Verification *adapter.Verification `json:"verification,omitempty"`
	UndoReceipt  *adapter.Receipt      `json:"undo_receipt,omitempty"`
	Error        string                `json:"error,omitempty"`
	CreatedAt    string                `json:"created_at"`
	UpdatedAt    string                `json:"updated_at"`
}

// Event is one append-only audit event.
type Event struct {
	Seq       int64           `json:"seq"`
	ChangeID  string          `json:"change_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Type      string          `json:"type"`
	FromState State           `json:"from_state,omitempty"`
	ToState   State           `json:"to_state,omitempty"`
	Actor     string          `json:"actor"`
	PlanHash  string          `json:"plan_hash,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	At        string          `json:"at"`
	PrevHash  string          `json:"prev_hash"`
	Hash      string          `json:"hash"`
}

// Session is a Lane B working session.
type Session struct {
	ID           string            `json:"id"`
	Principal    Principal         `json:"principal"`
	Target       string            `json:"target"`
	Env          string            `json:"env"`
	CheckpointID string            `json:"checkpoint_id,omitempty"`
	IncidentRef  string            `json:"incident_ref,omitempty"`
	Branch       *adapter.CopyRef  `json:"branch,omitempty"`
	Meta         map[string]string `json:"meta,omitempty"`
	StartedAt    string            `json:"started_at"`
	EndedAt      string            `json:"ended_at,omitempty"`
}

// Checkpoint is a stored checkpoint reference.
type Checkpoint struct {
	SessionID string                `json:"session_id,omitempty"`
	Ref       adapter.CheckpointRef `json:"ref"`
}

// Changeset states.
const (
	CSPlanned      State = "planned"
	CSAwaiting     State = "awaiting-approval"
	CSApproved     State = "approved"
	CSRejected     State = "rejected"
	CSApplying     State = "applying"
	CSApplied      State = "applied"
	CSCompensating State = "compensating"
	CSCompensated  State = "compensated"
	CSFailed       State = "failed" // a step failed and compensation did not complete
)

// Changeset is an ordered group of changes, possibly across adapters,
// applied as a saga: if step k fails, steps k-1..1 are undone in reverse.
type Changeset struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Changes   []string  `json:"changes"` // change IDs in apply order
	State     State     `json:"state"`
	Decision  *Decision `json:"decision,omitempty"` // strictest of the members
	FailedAt  int       `json:"failed_at,omitempty"` // 1-based step that failed
	Error     string    `json:"error,omitempty"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}
