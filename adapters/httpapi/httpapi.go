// Package httpapi is the Changegate adapter for REST APIs with compensation
// manifests (§7.6).
//
// Operation kind "http"; payload is JSON {"method", "path", "query", "body"}.
// A compensation manifest (YAML) declares, per operation, how to capture the
// resource's state before the call and which call undoes it:
//
//	api: tracker
//	operations:
//	  - match: {method: POST, path: /issues}
//	    inverse: {method: DELETE, path: "/issues/{response.id}"}
//	  - match: {method: PUT, path: "/issues/{id}"}
//	    before: {method: GET, path: "/issues/{id}"}
//	    inverse: {method: PUT, path: "/issues/{id}", body: "{before}"}
//	  - match: {method: DELETE, path: "/issues/{id}"}
//	    before: {method: GET, path: "/issues/{id}"}
//	    inverse: {method: POST, path: /issues, body: "{before}"}
//	    reversibility: compensatable     # re-created with a new id
//	  - match: {method: POST, path: /emails}
//	    reversibility: irreversible
//
// Templates: {name} path parameters, {response.field}, {body.field},
// {before} (the whole captured resource), {before.field}. Calls with no
// matching rule are irreversible. When a rule has `before`, the resource is
// re-read at apply (drift → conflict) and after apply; undo refuses if the
// resource changed since.
//
// Target config: base_url, manifest (path), header.<Name> (e.g.
// header.Authorization: "Bearer ${TOKEN}"), timeout.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/srawlin/changegate/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// Call is an HTTP request template.
type Call struct {
	Method string `yaml:"method" json:"method"`
	Path   string `yaml:"path" json:"path"`
	Body   any    `yaml:"body,omitempty" json:"body,omitempty"`
}

// Rule is one manifest entry.
type Rule struct {
	Match         Call                  `yaml:"match"`
	Before        *Call                 `yaml:"before,omitempty"`
	Inverse       *Call                 `yaml:"inverse,omitempty"`
	Reversibility adapter.Reversibility `yaml:"reversibility,omitempty"`
	Description   string                `yaml:"description,omitempty"`
	// Guard lists the top-level resource fields compared for drift (plan →
	// apply → undo). Default: the fields the request writes plus the fields
	// the inverse restores from {before.*}, so unrelated churn such as
	// updated_at or counters does not block an undo.
	Guard []string `yaml:"guard,omitempty"`
}

// Manifest is a compensation manifest.
type Manifest struct {
	API        string `yaml:"api"`
	Operations []Rule `yaml:"operations"`
}

// LoadManifest reads a manifest file.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	for i, r := range m.Operations {
		if r.Match.Method == "" || r.Match.Path == "" {
			return nil, fmt.Errorf("manifest %s: operation %d needs match.method and match.path", path, i+1)
		}
		if r.Reversibility == "" {
			if r.Inverse != nil {
				m.Operations[i].Reversibility = adapter.Reversible
			} else {
				m.Operations[i].Reversibility = adapter.Irreversible
			}
		}
	}
	return &m, nil
}

// Request is the operation payload.
type Request struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   any               `json:"body,omitempty"`
}

var paramRe = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// matchPath matches /issues/{id} against /issues/42 and returns params.
func matchPath(tmpl, path string) (map[string]string, bool) {
	ts, ps := strings.Split(strings.Trim(tmpl, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(ts) != len(ps) {
		return nil, false
	}
	params := map[string]string{}
	for i := range ts {
		if m := paramRe.FindStringSubmatch(ts[i]); m != nil && m[0] == ts[i] {
			params[m[1]] = ps[i]
			continue
		}
		if ts[i] != ps[i] {
			return nil, false
		}
	}
	return params, true
}

func (m *Manifest) find(req Request) (*Rule, map[string]string) {
	for i := range m.Operations {
		r := &m.Operations[i]
		if !strings.EqualFold(r.Match.Method, req.Method) {
			continue
		}
		if p, ok := matchPath(r.Match.Path, req.Path); ok {
			return r, p
		}
	}
	return nil, nil
}

type vars struct {
	params   map[string]string
	body     any
	before   any
	response any
}

func lookup(v any, path []string) (any, bool) {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return v, true
}

func (vs vars) value(name string) (any, bool) {
	parts := strings.Split(name, ".")
	switch parts[0] {
	case "response":
		return lookup(vs.response, parts[1:])
	case "body":
		return lookup(vs.body, parts[1:])
	case "before":
		return lookup(vs.before, parts[1:])
	}
	if v, ok := vs.params[name]; ok {
		return v, true
	}
	return nil, false
}

var (
	tmplRe        = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)
	beforeFieldRe = regexp.MustCompile(`\{before\.([a-zA-Z0-9_]+)`)
)

func (vs vars) expandString(s string) (string, error) {
	var err error
	out := tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		v, ok := vs.value(m[1 : len(m)-1])
		if !ok {
			err = fmt.Errorf("template %s is not available", m)
			return m
		}
		return url.PathEscape(fmt.Sprint(v))
	})
	return out, err
}

// expandBody replaces "{x}" strings (whole-value) and nested templates.
func (vs vars) expandBody(b any) (any, error) {
	switch x := b.(type) {
	case string:
		if m := tmplRe.FindStringSubmatch(x); m != nil && m[0] == x {
			v, ok := vs.value(m[1])
			if !ok {
				return nil, fmt.Errorf("template %s is not available", x)
			}
			return v, nil
		}
		return x, nil
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			e, err := vs.expandBody(v)
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case []any:
		var out []any
		for _, v := range x {
			e, err := vs.expandBody(v)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	}
	return b, nil
}

func (vs vars) resolve(c *Call) (*Request, error) {
	p, err := vs.expandString(c.Path)
	if err != nil {
		return nil, err
	}
	var body any
	if c.Body != nil {
		if body, err = vs.expandBody(normalize(c.Body)); err != nil {
			return nil, err
		}
	}
	return &Request{Method: strings.ToUpper(c.Method), Path: p, Body: body}, nil
}

// normalize converts YAML maps to map[string]any.
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = decodeJSON(b, &out)
	return out
}

// decodeJSON unmarshals keeping numbers exact (json.Number): API ids such
// as 6274080425 must not round-trip through float64 into paths.
func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// Adapter implements adapter.Adapter for HTTP APIs.
type Adapter struct {
	adapter.Unimplemented
	Client *http.Client
}

// New returns an HTTP API adapter.
func New() *Adapter { return &Adapter{Client: &http.Client{Timeout: 30 * time.Second}} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "httpapi", Version: "0.1.0", Kinds: []string{"http"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo}}, nil
}

func parseRequest(op adapter.Operation) (Request, error) {
	var r Request
	if err := json.Unmarshal([]byte(op.Payload), &r); err != nil {
		return r, fmt.Errorf("http operation payload must be JSON {method, path, query, body}: %w", err)
	}
	r.Method = strings.ToUpper(r.Method)
	if r.Method == "" || !strings.HasPrefix(r.Path, "/") {
		return r, errors.New("http operation needs method and an absolute path")
	}
	return r, nil
}

func access(method string) adapter.Access {
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return adapter.Read
	case "DELETE":
		return adapter.Destructive
	}
	return adapter.Write
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, _ adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	r, err := parseRequest(op)
	if err != nil {
		return adapter.Classification{}, err
	}
	acc := access(r.Method)
	obj := r.Method + " " + r.Path
	return adapter.Classification{Access: acc, Objects: []string{r.Path}, Statements: []adapter.StatementClass{{Kind: r.Method, Access: acc, Objects: []string{obj}}}}, nil
}

type response struct {
	Status int `json:"status"`
	Body   any `json:"body,omitempty"`
}

func (a *Adapter) do(ctx context.Context, t adapter.Target, r *Request) (*response, error) {
	base := strings.TrimRight(t.Config["base_url"], "/")
	if base == "" {
		return nil, errors.New("httpapi: target config base_url is required")
	}
	u := base + r.Path
	if len(r.Query) > 0 {
		q := url.Values{}
		for k, v := range r.Query {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	var body io.Reader
	if r.Body != nil {
		b, _ := json.Marshal(r.Body)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, err
	}
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	keys := make([]string, 0, len(t.Config))
	for k := range t.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "header.") {
			req.Header.Set(strings.TrimPrefix(k, "header."), os.ExpandEnv(t.Config[k]))
		}
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := &response{Status: resp.StatusCode}
	if len(bytes.TrimSpace(rb)) > 0 {
		var v any
		if decodeJSON(rb, &v) == nil {
			out.Body = v
		} else {
			out.Body = string(rb)
		}
	}
	return out, nil
}

func (a *Adapter) manifest(t adapter.Target) (*Manifest, error) {
	if p := t.Config["manifest"]; p != "" {
		return LoadManifest(p)
	}
	return &Manifest{}, nil
}

func hashOf(v any) string {
	b, _ := json.Marshal(v) // encoding/json sorts map keys
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// guardFields returns the fields compared for drift, or nil for the whole resource.
func guardFields(rule *Rule, req Request) []string {
	if rule == nil {
		return nil
	}
	if len(rule.Guard) > 0 {
		return rule.Guard
	}
	seen := map[string]bool{}
	if m, ok := req.Body.(map[string]any); ok {
		for k := range m {
			seen[k] = true
		}
	}
	if rule.Inverse != nil {
		b, _ := json.Marshal(normalize(rule.Inverse.Body))
		for _, m := range beforeFieldRe.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guardHash hashes the guarded fields of a resource (all of it if fields is
// nil or the resource is not an object).
func guardHash(v any, fields []string) string {
	m, ok := v.(map[string]any)
	if !ok || len(fields) == 0 {
		return hashOf(v)
	}
	sub := map[string]any{}
	for _, f := range fields {
		sub[f] = m[f]
	}
	return hashOf(sub)
}

type planData struct {
	Request    Request           `json:"request"`
	Rule       *Rule             `json:"rule,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	BeforeReq  *Request          `json:"before_req,omitempty"`
	Before     any               `json:"before,omitempty"`
	BeforeHash string            `json:"before_hash,omitempty"`
	Guard      []string          `json:"guard,omitempty"`
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	req, err := parseRequest(op)
	if err != nil {
		return nil, err
	}
	if access(req.Method) == adapter.Read {
		return nil, errors.New("read-only request; call the API directly")
	}
	m, err := a.manifest(t)
	if err != nil {
		return nil, err
	}
	cls, _ := a.Classify(ctx, t, op)
	pd := planData{Request: req}
	rev := adapter.Irreversible
	var warnings []string
	var diff strings.Builder
	fmt.Fprintf(&diff, "%s %s%s\n", req.Method, t.Config["base_url"], req.Path)
	if req.Body != nil {
		b, _ := json.MarshalIndent(req.Body, "   ", "  ")
		fmt.Fprintf(&diff, "   body: %s\n", b)
	}
	rule, params := m.find(req)
	if rule == nil {
		warnings = append(warnings, "no compensation manifest entry matches this call; it is treated as irreversible")
	} else {
		pd.Rule, pd.Params = rule, params
		rev = rule.Reversibility
		if rule.Description != "" {
			fmt.Fprintf(&diff, "   %s\n", rule.Description)
		}
		vs := vars{params: params, body: req.Body}
		if rule.Before != nil {
			br, err := vs.resolve(rule.Before)
			if err != nil {
				return nil, err
			}
			resp, err := a.do(ctx, t, br)
			if err != nil {
				return nil, err
			}
			if resp.Status >= 400 {
				return nil, fmt.Errorf("reading the resource before the change (%s %s) returned HTTP %d", br.Method, br.Path, resp.Status)
			}
			pd.Guard = guardFields(rule, req)
			pd.BeforeReq, pd.Before, pd.BeforeHash = br, resp.Body, guardHash(resp.Body, pd.Guard)
			if req.Body != nil {
				if bm, ok := resp.Body.(map[string]any); ok {
					if nm, ok := req.Body.(map[string]any); ok {
						keys := make([]string, 0, len(nm))
						for k := range nm {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						for _, k := range keys {
							if fmt.Sprint(bm[k]) != fmt.Sprint(nm[k]) {
								fmt.Fprintf(&diff, "   ~ %s: %v → %v\n", k, bm[k], nm[k])
							}
						}
					}
				}
			}
		}
		if rule.Inverse != nil {
			fmt.Fprintf(&diff, "   undo: %s %s\n", rule.Inverse.Method, rule.Inverse.Path)
		}
	}
	p := &adapter.Plan{ID: adapter.NewID("p"), Adapter: "httpapi", Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Reversibility: rev, Destructive: req.Method == "DELETE", BlastRows: 1, Warnings: warnings, RehearsalTier: "static",
		Effects:   []adapter.Effect{{Kind: "http-" + strings.ToLower(req.Method), Object: req.Path, Description: req.Method + " " + req.Path, Rows: 1}},
		Facts:     map[string]string{"api": m.API},
		Diff:      fmt.Sprintf("plan · %s (%s) · %s\n%s", t.Name, t.Env, rev, diff.String()),
		CreatedAt: time.Now().UTC().Format(time.RFC3339), PreconditionHash: pd.BeforeHash}
	p.ID = adapter.NewID("p")
	p.AdapterData, _ = json.Marshal(pd)
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

type undoHandle struct {
	Inverse   *Request `json:"inverse,omitempty"`
	GuardReq  *Request `json:"guard_req,omitempty"`
	AfterHash string   `json:"after_hash,omitempty"`
	Guard     []string `json:"guard,omitempty"`
	Response  any      `json:"response,omitempty"`
}

// Apply implements adapter.Adapter.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var pd planData
	if err := decodeJSON(p.AdapterData, &pd); err != nil {
		return nil, err
	}
	if pd.BeforeReq != nil {
		resp, err := a.do(ctx, t, pd.BeforeReq)
		if err != nil {
			return nil, err
		}
		if guardHash(resp.Body, pd.Guard) != pd.BeforeHash {
			return nil, &adapter.ConflictError{Reason: "the resource changed since plan; re-plan required", Conflicts: []string{pd.BeforeReq.Path}}
		}
	}
	resp, err := a.do(ctx, t, &pd.Request)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		return nil, fmt.Errorf("%s %s returned HTTP %d: %v", pd.Request.Method, pd.Request.Path, resp.Status, resp.Body)
	}
	uh := undoHandle{Response: resp.Body}
	rev := p.Reversibility
	if pd.Rule != nil && pd.Rule.Inverse != nil {
		vs := vars{params: pd.Params, body: pd.Request.Body, before: pd.Before, response: resp.Body}
		inv, err := vs.resolve(pd.Rule.Inverse)
		if err != nil {
			rev = adapter.Irreversible // e.g. the response lacked the id the inverse needs
		} else {
			uh.Inverse = inv
		}
		// Guard: the resource as it is right after the change.
		if pd.BeforeReq != nil && pd.Request.Method != "DELETE" {
			if after, err := a.do(ctx, t, pd.BeforeReq); err == nil && after.Status < 400 {
				uh.GuardReq, uh.Guard, uh.AfterHash = pd.BeforeReq, pd.Guard, guardHash(after.Body, pd.Guard)
			}
		}
	}
	raw, _ := json.Marshal(uh)
	out, _ := json.Marshal(resp.Body)
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: fmt.Sprintf("HTTP %d %s", resp.Status, truncate(string(out), 300)), RowsAffected: 1, UndoHandle: raw, Reversibility: rev}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Undo implements adapter.Adapter.
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	var uh undoHandle
	if err := decodeJSON(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	if uh.Inverse == nil || r.Reversibility == adapter.Irreversible {
		return nil, errors.New("no compensating call is known for this request")
	}
	if uh.GuardReq != nil {
		cur, err := a.do(ctx, t, uh.GuardReq)
		if err != nil {
			return nil, err
		}
		if guardHash(cur.Body, uh.Guard) != uh.AfterHash {
			return nil, &adapter.ConflictError{Reason: "the resource changed since apply; undo refused", Conflicts: []string{uh.GuardReq.Path}}
		}
	}
	resp, err := a.do(ctx, t, uh.Inverse)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		return nil, fmt.Errorf("compensating %s %s returned HTTP %d", uh.Inverse.Method, uh.Inverse.Path, resp.Status)
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: fmt.Sprintf("%s %s → HTTP %d", uh.Inverse.Method, uh.Inverse.Path, resp.Status), Reversibility: r.Reversibility}, nil
}

// Verify implements adapter.Adapter (the call already returned 2xx).
func (a *Adapter) Verify(context.Context, adapter.Target, *adapter.Plan, *adapter.Receipt) (*adapter.Verification, error) {
	return &adapter.Verification{Passed: true, Checks: []adapter.Check{{Name: "2xx response", Passed: true}}}, nil
}
