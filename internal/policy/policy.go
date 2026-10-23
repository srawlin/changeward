// Package policy evaluates CEL change-control policy. One policy file covers
// both lanes; rules can target a lane, an environment class, or both.
//
// Every rule whose condition matches contributes its decision. They combine
// as deny > require > approve. If no user rule matches, the built-in
// environment defaults (builtin.yaml) apply; if nothing matches at all the
// decision is "require" (fail safe). A rule that fails to evaluate counts as
// "require", never as a silent pass.
package policy

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// Decisions.
const (
	Approve = "approve"
	Require = "require"
	Deny    = "deny"
)

// Channels.
const (
	ChannelInline     = "inline"
	ChannelOutOfBand  = "out-of-band"
	defaultPlanExpiry = 15 * time.Minute
)

//go:embed builtin.yaml
var builtinYAML []byte

// Rule is one policy rule.
type Rule struct {
	Name      string   `yaml:"name"`
	When      string   `yaml:"when"`
	Decide    string   `yaml:"decide"`
	Approvers []string `yaml:"approvers,omitempty"`
	Channel   string   `yaml:"channel,omitempty"`
	Quorum    int      `yaml:"quorum,omitempty"`
	Reason    string   `yaml:"reason,omitempty"`
}

// Freeze is a named change freeze or window, checked by calendar.freeze(name).
type Freeze struct {
	Name  string `yaml:"name"`
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

// File is a policy file.
type File struct {
	Version int      `yaml:"version"`
	Rules   []Rule   `yaml:"rules"`
	Freezes []Freeze `yaml:"freezes,omitempty"`
}

// LoadFile parses a policy file from disk.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses policy YAML.
func Parse(b []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return &f, nil
}

// BuiltinYAML returns the built-in default policy, for `changeward policy init`.
func BuiltinYAML() []byte { return builtinYAML }

type compiled struct {
	Rule
	prg cel.Program
}

type window struct{ start, end time.Time }

// Engine is a compiled policy.
type Engine struct {
	rules   []compiled
	builtin []compiled
	freezes map[string]window
	now     func() time.Time
}

// Input is everything a rule can see.
type Input struct {
	Change    map[string]any
	Principal map[string]any
	Session   map[string]any
}

// New compiles user rules (f may be nil) on top of the built-in defaults.
func New(f *File) (*Engine, error) {
	e := &Engine{freezes: map[string]window{}, now: time.Now}
	b, err := Parse(builtinYAML)
	if err != nil {
		return nil, err
	}
	env, err := e.celEnv()
	if err != nil {
		return nil, err
	}
	if e.builtin, err = compileRules(env, b.Rules); err != nil {
		return nil, fmt.Errorf("builtin policy: %w", err)
	}
	if f == nil {
		return e, nil
	}
	for _, fr := range f.Freezes {
		s, err1 := time.Parse(time.RFC3339, fr.Start)
		en, err2 := time.Parse(time.RFC3339, fr.End)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("policy: freeze %q needs RFC3339 start and end", fr.Name)
		}
		e.freezes[fr.Name] = window{s, en}
	}
	if e.rules, err = compileRules(env, f.Rules); err != nil {
		return nil, err
	}
	return e, nil
}

// SetClock overrides the clock (tests).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func (e *Engine) celEnv() (*cel.Env, error) {
	m := cel.MapType(cel.StringType, cel.DynType)
	return cel.NewEnv(
		cel.Variable("change", m),
		cel.Variable("principal", m),
		cel.Variable("session", m),
		cel.Variable("now", cel.TimestampType),
		cel.Function("calendar.freeze",
			cel.Overload("calendar_freeze_string", []*cel.Type{cel.StringType}, cel.BoolType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					w, ok := e.freezes[string(v.(types.String))]
					if !ok {
						return types.False
					}
					now := e.now()
					return types.Bool(!now.Before(w.start) && now.Before(w.end))
				}))),
	)
}

func compileRules(env *cel.Env, rules []Rule) ([]compiled, error) {
	var out []compiled
	seen := map[string]bool{}
	for i, r := range rules {
		if r.Name == "" {
			r.Name = fmt.Sprintf("rule-%d", i+1)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("policy: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
		switch r.Decide {
		case Approve, Require, Deny:
		default:
			return nil, fmt.Errorf("policy: rule %q: decide must be approve, require or deny", r.Name)
		}
		if r.Channel != "" && r.Channel != ChannelInline && r.Channel != ChannelOutOfBand {
			return nil, fmt.Errorf("policy: rule %q: channel must be inline or out-of-band", r.Name)
		}
		when := strings.TrimSpace(r.When)
		if when == "" {
			when = "true"
		}
		ast, iss := env.Compile(when)
		if iss.Err() != nil {
			return nil, fmt.Errorf("policy: rule %q: %w", r.Name, iss.Err())
		}
		if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
			return nil, fmt.Errorf("policy: rule %q: condition must be boolean", r.Name)
		}
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("policy: rule %q: %w", r.Name, err)
		}
		out = append(out, compiled{Rule: r, prg: prg})
	}
	return out, nil
}

type match struct {
	rule compiled
	err  error
}

func (e *Engine) eval(rules []compiled, vars map[string]any) []match {
	var out []match
	for _, r := range rules {
		v, _, err := r.prg.Eval(vars)
		if err != nil {
			out = append(out, match{rule: r, err: err})
			continue
		}
		if b, ok := v.Value().(bool); ok && b {
			out = append(out, match{rule: r})
		}
	}
	return out
}

// Decide evaluates the policy for one change.
func (e *Engine) Decide(in Input) model.Decision {
	vars := map[string]any{
		"change":    orEmpty(in.Change),
		"principal": orEmpty(in.Principal),
		"session":   orEmpty(in.Session),
		"now":       e.now(),
	}
	matches := e.eval(e.rules, vars)
	if len(matches) == 0 {
		matches = e.eval(e.builtin, vars)
	}
	env, _ := in.Change["env"].(string)
	d := model.Decision{Decision: Require, Channel: defaultChannel(env)}
	if len(matches) == 0 {
		d.Reasons = []string{"no policy rule matched; approval required"}
		d.Approvers = []string{"owner"}
		d.Quorum = 1
		return d
	}
	best := ""
	approvers := map[string]bool{}
	anyOOB := false
	for _, m := range matches {
		decide := m.rule.Decide
		reason := m.rule.Reason
		if m.err != nil {
			decide = Require
			reason = fmt.Sprintf("policy rule %q failed to evaluate: %v", m.rule.Name, m.err)
		}
		if rank(decide) > rank(best) {
			best = decide
		}
		d.Rules = append(d.Rules, m.rule.Name)
		if reason != "" {
			d.Reasons = append(d.Reasons, reason)
		}
		if decide == Require {
			for _, a := range m.rule.Approvers {
				approvers[a] = true
			}
			if m.rule.Quorum > d.Quorum {
				d.Quorum = m.rule.Quorum
			}
			if m.rule.Channel == ChannelOutOfBand {
				anyOOB = true
			}
		}
	}
	d.Decision = best
	if best == Require {
		for a := range approvers {
			d.Approvers = append(d.Approvers, a)
		}
		sort.Strings(d.Approvers)
		if len(d.Approvers) == 0 {
			d.Approvers = []string{"owner"}
		}
		if d.Quorum == 0 {
			d.Quorum = 1
		}
		if anyOOB {
			d.Channel = ChannelOutOfBand
		}
	} else {
		d.Approvers = nil
		d.Quorum = 0
	}
	return d
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func rank(d string) int {
	switch d {
	case Approve:
		return 1
	case Require:
		return 2
	case Deny:
		return 3
	}
	return 0
}

// DefaultChannel is the approval channel an environment class uses by default.
func DefaultChannel(env string) string { return defaultChannel(env) }

// Stricter reports whether decision a is stricter than b (deny > require > approve).
func Stricter(a, b string) bool { return rank(a) > rank(b) }

// defaultChannel: in-band approval is acceptable only where the human at the
// keyboard owns the system (§6.6).
func defaultChannel(env string) string {
	switch env {
	case adapter.EnvPersonal, adapter.EnvSharedDev, "":
		return ChannelInline
	}
	return ChannelOutOfBand
}

// DefaultPlanExpiry is how long a plan stays valid for approval and apply.
func DefaultPlanExpiry() time.Duration { return defaultPlanExpiry }

// ChangeInput builds the `change` variable from a plan and context.
func ChangeInput(p *adapter.Plan, lane string, rehearsalPassed bool, extra map[string]any) map[string]any {
	var kinds []any
	for _, s := range p.Classification.Statements {
		kinds = append(kinds, s.Kind)
	}
	flags := []any{}
	for _, f := range p.Classification.Flags {
		flags = append(flags, f)
	}
	objects := []any{}
	for _, o := range p.Classification.Objects {
		objects = append(objects, o)
	}
	facts := map[string]any{}
	for k, v := range p.Facts {
		facts[k] = v
	}
	if kinds == nil {
		kinds = []any{}
	}
	m := map[string]any{
		"lane":          lane,
		"env":           p.Env,
		"target":        p.Target,
		"adapter":       p.Adapter,
		"kind":          p.Operation.Kind,
		"access":        string(p.Classification.Access),
		"reversibility": string(p.Reversibility),
		"destructive":   p.Destructive,
		"blast":         map[string]any{"rows": p.BlastRows},
		"rehearsal":     map[string]any{"passed": rehearsalPassed, "tier": p.RehearsalTier},
		"statements":    kinds,
		"flags":         flags,
		"objects":       objects,
		"facts":         facts,
		"warnings":      int64(len(p.Warnings)),
		"signals":       map[string]any{},
		"spend_usd":     0.0,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}
