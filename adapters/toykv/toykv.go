// Package toykv is a minimal reference adapter over a JSON key-value file.
// It exists to exercise the adapter contract and the conformance suite, and
// as a template for writing new adapters.
//
// Operations (kind "kv"): "set <key> <value>" and "del <key>".
// Target config: path (the JSON file).
package toykv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srawlin/changeward/pkg/adapter"
)

// Adapter implements adapter.Adapter for toy KV files.
type Adapter struct {
	adapter.Unimplemented
	mu sync.Mutex
}

// New returns a toy KV adapter.
func New() *Adapter { return &Adapter{} }

type op struct {
	verb  string
	key   string
	value string
}

func parse(o adapter.Operation) (op, error) {
	if o.Kind != "kv" {
		return op{}, fmt.Errorf("toykv: unsupported kind %q", o.Kind)
	}
	f := strings.Fields(o.Payload)
	switch {
	case len(f) == 3 && f[0] == "set":
		return op{"set", f[1], f[2]}, nil
	case len(f) == 2 && f[0] == "del":
		return op{"del", f[1], ""}, nil
	case len(f) == 2 && f[0] == "get":
		return op{"get", f[1], ""}, nil
	}
	return op{}, fmt.Errorf("toykv: cannot parse %q", o.Payload)
}

// state is a value plus presence; nil means absent.
type entry struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

type data struct {
	Key    string `json:"key"`
	Before entry  `json:"before"`
	After  entry  `json:"after"`
}

func load(t adapter.Target) (map[string]string, error) {
	path := t.Config["path"]
	if path == "" {
		return nil, errors.New("toykv: target config.path is required")
	}
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return m, nil
	}
	return m, json.Unmarshal(b, &m)
}

func save(t adapter.Target, m map[string]string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := t.Config["path"] + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.Config["path"])
}

func get(m map[string]string, k string) entry {
	v, ok := m[k]
	return entry{Present: ok, Value: v}
}

func put(m map[string]string, k string, e entry) {
	if e.Present {
		m[k] = e.Value
	} else {
		delete(m, k)
	}
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "toykv", Version: "0.1.0", Kinds: []string{"kv"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo, adapter.CapCheckpoint, adapter.CapRestore}}, nil
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, _ adapter.Target, o adapter.Operation) (adapter.Classification, error) {
	p, err := parse(o)
	if err != nil {
		return adapter.Classification{}, err
	}
	acc := adapter.Write
	switch p.verb {
	case "del":
		acc = adapter.Destructive
	case "get":
		acc = adapter.Read
	}
	return adapter.Classification{Access: acc, Objects: []string{p.key},
		Statements: []adapter.StatementClass{{Text: o.Payload, Kind: strings.ToUpper(p.verb), Access: acc, Objects: []string{p.key}}}}, nil
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, o adapter.Operation) (*adapter.Plan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if o.Kind == "changeward.fingerprint" {
		return a.fingerprint(t)
	}
	if o.Kind == "changeward.corpus" {
		return corpus(o)
	}
	p, err := parse(o)
	if err != nil {
		return nil, err
	}
	if p.verb == "get" {
		return nil, errors.New("toykv: reads are not planned")
	}
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	d := data{Key: p.key, Before: get(m, p.key)}
	if p.verb == "set" {
		d.After = entry{Present: true, Value: p.value}
	}
	cls, _ := a.Classify(ctx, t, o)
	raw, _ := json.Marshal(d)
	pre := sha256.Sum256([]byte(fmt.Sprintf("%v", d.Before)))
	desc := fmt.Sprintf("%s: %s -> %s", p.key, show(d.Before), show(d.After))
	plan := &adapter.Plan{
		ID: adapter.NewID("p"), Adapter: "toykv", Target: t.Name, Env: t.Env, Operation: o,
		Classification: cls, Reversibility: adapter.Reversible, Destructive: p.verb == "del",
		BlastRows: 1, Effects: []adapter.Effect{{Kind: "key-" + p.verb, Object: p.key, Description: desc, Rows: 1}},
		Diff: desc, PreconditionHash: hex.EncodeToString(pre[:]), RehearsalTier: "live-dry-run",
		AdapterData: raw, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	plan.Hash = adapter.HashPlan(plan)
	return plan, nil
}

func show(e entry) string {
	if !e.Present {
		return "(absent)"
	}
	return fmt.Sprintf("%q", e.Value)
}

// Apply implements adapter.Adapter.
func (a *Adapter) Apply(_ context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var d data
	if err := json.Unmarshal(p.AdapterData, &d); err != nil {
		return nil, err
	}
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	if cur := get(m, d.Key); cur != d.Before {
		return nil, &adapter.ConflictError{Reason: "data changed since plan; re-plan required", Conflicts: []string{d.Key}}
	}
	put(m, d.Key, d.After)
	if err := save(t, m); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339),
		RowsAffected: 1, UndoHandle: p.AdapterData, Reversibility: adapter.Reversible}, nil
}

// Verify implements adapter.Adapter.
func (a *Adapter) Verify(_ context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Verification, error) {
	var d data
	if err := json.Unmarshal(r.UndoHandle, &d); err != nil {
		return nil, err
	}
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	ok := get(m, d.Key) == d.After
	return &adapter.Verification{Passed: ok, Checks: []adapter.Check{{Name: "value", Passed: ok}}}, nil
}

// Undo implements adapter.Adapter.
func (a *Adapter) Undo(_ context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var d data
	if err := json.Unmarshal(r.UndoHandle, &d); err != nil {
		return nil, err
	}
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	if cur := get(m, d.Key); cur != d.After {
		return nil, &adapter.ConflictError{Reason: "value no longer matches its after-image", Conflicts: []string{d.Key}}
	}
	put(m, d.Key, d.Before)
	if err := save(t, m); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339), RowsAffected: 1, Reversibility: adapter.Reversible}, nil
}

// Checkpoint implements adapter.Adapter by copying the file.
func (a *Adapter) Checkpoint(_ context.Context, t adapter.Target, label string) (*adapter.CheckpointRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	id := adapter.NewID("cp")
	b, _ := json.Marshal(m)
	return &adapter.CheckpointRef{ID: id, Adapter: "toykv", Target: t.Name, Data: b, CreatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// Restore implements adapter.Adapter.
func (a *Adapter) Restore(_ context.Context, t adapter.Target, cp *adapter.CheckpointRef) (*adapter.Receipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := map[string]string{}
	if err := json.Unmarshal(cp.Data, &m); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(t.Config["path"]), 0o700); err != nil {
		return nil, err
	}
	if err := save(t, m); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), AppliedAt: time.Now().UTC().Format(time.RFC3339), Reversibility: adapter.Reversible}, nil
}

// fingerprint implements the conformance convention: a Plan of kind
// "changeward.fingerprint" returns facts["fingerprint"], a hash of state.
func (a *Adapter) fingerprint(t adapter.Target) (*adapter.Plan, error) {
	m, err := load(t)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q;", k, m[k])
	}
	return &adapter.Plan{Facts: map[string]string{"fingerprint": hex.EncodeToString(h.Sum(nil))}}, nil
}

// corpus implements the conformance convention: a Plan of kind
// "changeward.corpus" with params seed and n returns facts["corpus"], a JSON
// list of generated operations.
func corpus(o adapter.Operation) (*adapter.Plan, error) {
	var seed, n int64 = 1, 20
	_, _ = fmt.Sscan(o.Params["seed"], &seed)
	_, _ = fmt.Sscan(o.Params["n"], &n)
	rng := rand.New(rand.NewSource(seed))
	var ops []adapter.Operation
	for i := int64(0); i < n; i++ {
		k := fmt.Sprintf("k%d", rng.Intn(5))
		if rng.Intn(3) == 0 {
			ops = append(ops, adapter.Operation{Kind: "kv", Payload: "del " + k})
		} else {
			ops = append(ops, adapter.Operation{Kind: "kv", Payload: fmt.Sprintf("set %s v%d", k, rng.Intn(100))})
		}
	}
	b, _ := json.Marshal(ops)
	return &adapter.Plan{Facts: map[string]string{"corpus": string(b)}}, nil
}
