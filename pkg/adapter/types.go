// Package adapter defines the Changegate adapter contract in Go: the types
// exchanged with adapters, the Adapter interface, and the go-plugin transport
// used to run adapters out of process.
//
// The wire format is defined in proto/changegate/v1/adapter.proto. These Go
// types mirror it with JSON tags; they are what the engine and store use.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Reversibility classes, from most to least reversible.
type Reversibility string

const (
	Reversible     Reversibility = "reversible"
	Compensatable  Reversibility = "compensatable"
	CheckpointOnly Reversibility = "checkpoint-only"
	Irreversible   Reversibility = "irreversible"
)

// Rank orders reversibility classes; higher is worse. Unknown is treated as irreversible.
func (r Reversibility) Rank() int {
	switch r {
	case Reversible:
		return 0
	case Compensatable:
		return 1
	case CheckpointOnly:
		return 2
	default:
		return 3
	}
}

// Worst returns the less reversible of a and b.
func Worst(a, b Reversibility) Reversibility {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// Valid reports whether r is one of the defined classes.
func (r Reversibility) Valid() bool {
	switch r {
	case Reversible, Compensatable, CheckpointOnly, Irreversible:
		return true
	}
	return false
}

// Access classifies whether an operation reads, writes or destroys.
type Access string

const (
	Read        Access = "read"
	Write       Access = "write"
	Destructive Access = "destructive"
)

// Rank orders access levels; higher is more dangerous.
func (a Access) Rank() int {
	switch a {
	case Read:
		return 0
	case Write:
		return 1
	case Destructive:
		return 2
	}
	return 2
}

// MaxAccess returns the more dangerous of a and b.
func MaxAccess(a, b Access) Access {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// Environment classes.
const (
	EnvPersonal       = "personal"
	EnvSharedDev      = "shared-dev"
	EnvStaging        = "staging"
	EnvProd           = "prod"
	EnvProdRestricted = "prod-restricted"
)

// ValidEnv reports whether env is a known environment class.
func ValidEnv(env string) bool {
	switch env {
	case EnvPersonal, EnvSharedDev, EnvStaging, EnvProd, EnvProdRestricted:
		return true
	}
	return false
}

// Capabilities an adapter may declare.
const (
	CapClassify      = "classify"
	CapPlan          = "plan"
	CapApply         = "apply"
	CapVerify        = "verify"
	CapUndo          = "undo"
	CapRehearse      = "rehearse"
	CapCheckpoint    = "checkpoint"
	CapRestore       = "restore"
	CapProvisionCopy = "provision_copy"
)

// Target is one connection to a system. Config carries adapter-specific details.
type Target struct {
	Name    string            `json:"name"`
	Adapter string            `json:"adapter"`
	Env     string            `json:"env"`
	Config  map[string]string `json:"config,omitempty"`
}

// Redacted returns a copy of t without its config, which may hold credentials.
func (t Target) Redacted() Target {
	t.Config = nil
	return t
}

// Operation is one intended mutation (or read) of a target.
type Operation struct {
	Kind    string            `json:"kind"`
	Payload string            `json:"payload"`
	Params  map[string]string `json:"params,omitempty"`
	Intent  string            `json:"intent,omitempty"`
}

// StatementClass is the classification of one statement within an operation.
type StatementClass struct {
	Text    string   `json:"text"`
	Kind    string   `json:"kind"`
	Access  Access   `json:"access"`
	Objects []string `json:"objects,omitempty"`
	Flags   []string `json:"flags,omitempty"`
}

// Classification is the result of Classify.
type Classification struct {
	Access     Access           `json:"access"`
	Statements []StatementClass `json:"statements,omitempty"`
	Objects    []string         `json:"objects,omitempty"`
	Flags      []string         `json:"flags,omitempty"`
}

// Effect is one predicted or observed effect of a change.
type Effect struct {
	Kind        string `json:"kind"`
	Object      string `json:"object,omitempty"`
	Description string `json:"description"`
	Rows        int64  `json:"rows,omitempty"`
}

// Plan is an adapter-computed effect preview.
type Plan struct {
	ID               string            `json:"id"`
	Hash             string            `json:"hash"`
	Adapter          string            `json:"adapter"`
	Target           string            `json:"target"`
	Env              string            `json:"env"`
	Operation        Operation         `json:"operation"`
	Classification   Classification    `json:"classification"`
	Effects          []Effect          `json:"effects,omitempty"`
	Reversibility    Reversibility     `json:"reversibility"`
	Destructive      bool              `json:"destructive"`
	BlastRows        int64             `json:"blast_rows"`
	Warnings         []string          `json:"warnings,omitempty"`
	Facts            map[string]string `json:"facts,omitempty"`
	Diff             string            `json:"diff,omitempty"`
	PreconditionHash string            `json:"precondition_hash,omitempty"`
	RehearsalTier    string            `json:"rehearsal_tier,omitempty"`
	AdapterData      json.RawMessage   `json:"adapter_data,omitempty"`
	CreatedAt        string            `json:"created_at,omitempty"`
	ExpiresAt        string            `json:"expires_at,omitempty"`
}

// Receipt records what happened at apply (or undo) time.
type Receipt struct {
	ID            string          `json:"id"`
	PlanHash      string          `json:"plan_hash"`
	AppliedAt     string          `json:"applied_at"`
	Output        string          `json:"output,omitempty"`
	RowsAffected  int64           `json:"rows_affected"`
	UndoHandle    json.RawMessage `json:"undo_handle,omitempty"`
	Reversibility Reversibility   `json:"reversibility"`
	Warnings      []string        `json:"warnings,omitempty"`
}

// Check is one named verification or rehearsal check.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Verification is the result of post-condition checks against the target.
type Verification struct {
	Passed bool    `json:"passed"`
	Checks []Check `json:"checks,omitempty"`
}

// RehearsalResult is the evidence produced by Rehearse.
type RehearsalResult struct {
	Passed  bool     `json:"passed"`
	Tier    string   `json:"tier"`
	Checks  []Check  `json:"checks,omitempty"`
	Effects []Effect `json:"effects,omitempty"`
	Report  string   `json:"report,omitempty"`
}

// Info describes an adapter.
type Info struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Kinds        []string `json:"kinds"`
	Capabilities []string `json:"capabilities"`
}

// Has reports whether the adapter declared capability c.
func (i Info) Has(c string) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// CheckpointRef identifies a checkpoint taken by an adapter.
type CheckpointRef struct {
	ID        string          `json:"id"`
	Adapter   string          `json:"adapter"`
	Target    string          `json:"target"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// CopyRef is a provisioned copy of a target, usable as a rehearsal or working target.
type CopyRef struct {
	Target Target          `json:"target"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// QueryResult is a capped, redacted read result (the read pass-through path).
type QueryResult struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Truncated bool       `json:"truncated"`
	Redacted  []string   `json:"redacted,omitempty"`
}

// Querier is implemented by adapters that serve read-only queries.
type Querier interface {
	Query(ctx context.Context, t Target, query string, maxRows int) (*QueryResult, error)
}

// ErrUnsupported is returned for capabilities an adapter does not implement.
var ErrUnsupported = errors.New("operation not supported by adapter")

// ConflictError is returned when apply or undo finds the target no longer
// matches the state the plan or receipt assumed. Conflicts are never
// resolved by overwriting; they go to a human.
type ConflictError struct {
	Reason    string
	Conflicts []string
}

func (e *ConflictError) Error() string {
	if len(e.Conflicts) == 0 {
		return e.Reason
	}
	return fmt.Sprintf("%s (%d conflicts; first: %s)", e.Reason, len(e.Conflicts), e.Conflicts[0])
}

// Adapter is the contract every adapter implements. Optional RPCs return
// ErrUnsupported when the capability was not declared.
type Adapter interface {
	Describe(ctx context.Context) (Info, error)
	Classify(ctx context.Context, t Target, op Operation) (Classification, error)
	Plan(ctx context.Context, t Target, op Operation) (*Plan, error)
	Apply(ctx context.Context, t Target, p *Plan) (*Receipt, error)
	Verify(ctx context.Context, t Target, p *Plan, r *Receipt) (*Verification, error)
	Undo(ctx context.Context, t Target, p *Plan, r *Receipt) (*Receipt, error)
	Rehearse(ctx context.Context, copy Target, op Operation) (*RehearsalResult, error)
	Checkpoint(ctx context.Context, t Target, label string) (*CheckpointRef, error)
	Restore(ctx context.Context, t Target, cp *CheckpointRef) (*Receipt, error)
	ProvisionCopy(ctx context.Context, t Target, name string) (*CopyRef, error)
	ReleaseCopy(ctx context.Context, c *CopyRef) error
}

// Unimplemented can be embedded by adapters to get ErrUnsupported for every
// optional RPC they do not implement.
type Unimplemented struct{}

func (Unimplemented) Verify(context.Context, Target, *Plan, *Receipt) (*Verification, error) {
	return &Verification{Passed: true, Checks: []Check{{Name: "verify", Passed: true, Detail: "adapter has no post-conditions"}}}, nil
}
func (Unimplemented) Undo(context.Context, Target, *Plan, *Receipt) (*Receipt, error) {
	return nil, ErrUnsupported
}
func (Unimplemented) Rehearse(context.Context, Target, Operation) (*RehearsalResult, error) {
	return nil, ErrUnsupported
}
func (Unimplemented) Checkpoint(context.Context, Target, string) (*CheckpointRef, error) {
	return nil, ErrUnsupported
}
func (Unimplemented) Restore(context.Context, Target, *CheckpointRef) (*Receipt, error) {
	return nil, ErrUnsupported
}
func (Unimplemented) ProvisionCopy(context.Context, Target, string) (*CopyRef, error) {
	return nil, ErrUnsupported
}
func (Unimplemented) ReleaseCopy(context.Context, *CopyRef) error { return ErrUnsupported }
