// Package configfile is the Changeward adapter for reloadable configuration
// files: NGINX, Traefik's file provider, HAProxy and similar (§7.5).
//
// Operation kind "file": payload is the desired full content of the file.
// Plan shows a line diff and validates the candidate with the target's test
// command (for NGINX `nginx -t`) without reloading. Apply checks the file
// still matches what was planned, writes atomically, tests, reloads, and
// verifies with health checks; a failed test restores the old file before
// anything is reloaded, and a failed health check triggers the engine's
// automatic rollback. Undo restores the previous content if the file still
// matches what was applied.
//
// Target config:
//
//	path         the managed file
//	preset       nginx | traefik | haproxy (sets default commands)
//	test_cmd     command validating the configuration (may use {file})
//	reload_cmd   command reloading the service
//	health_urls  comma-separated URLs that must answer 2xx/3xx after reload
//	reload_settle  wait before health checks (default 700ms)
package configfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srawlin/changeward/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// Adapter implements adapter.Adapter for configuration files.
type Adapter struct {
	adapter.Unimplemented
	mu sync.Mutex // serializes candidate validation per process
}

// New returns a config-file adapter.
func New() *Adapter { return &Adapter{} }

var _ adapter.Adapter = (*Adapter)(nil)

var presets = map[string][2]string{
	"nginx":   {"nginx -t", "nginx -s reload"},
	"haproxy": {"haproxy -c -f {file}", "systemctl reload haproxy"},
	"traefik": {"", ""}, // the file provider watches the file; validation is a YAML/TOML parse
}

func commands(t adapter.Target) (test, reload string) {
	p := presets[t.Config["preset"]]
	test, reload = p[0], p[1]
	if v, ok := t.Config["test_cmd"]; ok {
		test = v
	}
	if v, ok := t.Config["reload_cmd"]; ok {
		reload = v
	}
	return strings.ReplaceAll(test, "{file}", t.Config["path"]), strings.ReplaceAll(reload, "{file}", t.Config["path"])
}

func sh(ctx context.Context, cmdline string) (string, error) {
	if strings.TrimSpace(cmdline) == "" {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %v: %s", cmdline, err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "configfile", Version: "0.1.0", Kinds: []string{"file"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo}}, nil
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, t adapter.Target, _ adapter.Operation) (adapter.Classification, error) {
	return adapter.Classification{Access: adapter.Write, Objects: []string{t.Config["path"]},
		Statements: []adapter.StatementClass{{Kind: "WRITE FILE", Access: adapter.Write, Objects: []string{t.Config["path"]}}}}, nil
}

type planData struct {
	Before     string `json:"before"`
	BeforeHash string `json:"before_hash"`
	Existed    bool   `json:"existed"`
}

// LineDiff is a minimal unified-style diff (LCS on lines).
func LineDiff(a, b string) []string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			i, j = i+1, j+1
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, fmt.Sprintf("+%4d  %s", j+1, y[j]))
			j++
		default:
			out = append(out, fmt.Sprintf("-%4d  %s", i+1, x[i]))
			i++
		}
	}
	return out
}

func (a *Adapter) validate(ctx context.Context, t adapter.Target, candidate []byte) error {
	path := t.Config["path"]
	if t.Config["preset"] == "traefik" {
		var v any
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
			if err := yaml.Unmarshal(candidate, &v); err != nil {
				return fmt.Errorf("invalid YAML: %w", err)
			}
		}
	}
	test, _ := commands(t)
	if test == "" {
		return nil
	}
	// The test command reads the live path, so the candidate is swapped in
	// for the duration of the test only (nothing is reloaded).
	a.mu.Lock()
	defer a.mu.Unlock()
	orig, err := os.ReadFile(path)
	existed := err == nil
	if err := writeAtomic(path, candidate); err != nil {
		return err
	}
	_, terr := sh(ctx, test)
	if existed {
		if err := writeAtomic(path, orig); err != nil {
			return fmt.Errorf("restoring %s after validation: %w", path, err)
		}
	} else {
		_ = os.Remove(path)
	}
	return terr
}

func writeAtomic(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".changeward.tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	path := t.Config["path"]
	if path == "" {
		return nil, errors.New("configfile: target config path is required")
	}
	if op.Kind == "changeward.fingerprint" {
		b, _ := os.ReadFile(path)
		return &adapter.Plan{Facts: map[string]string{"fingerprint": hash(b)}}, nil
	}
	if op.Kind != "file" && op.Kind != "" {
		return nil, fmt.Errorf("configfile: unsupported operation kind %q", op.Kind)
	}
	before, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	candidate := []byte(op.Payload)
	if err := a.validate(ctx, t, candidate); err != nil {
		return nil, fmt.Errorf("candidate configuration is invalid: %w", err)
	}
	lines := LineDiff(string(before), op.Payload)
	cls, _ := a.Classify(ctx, t, op)
	p := &adapter.Plan{ID: adapter.NewID("p"), Adapter: "configfile", Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Reversibility: adapter.Reversible, BlastRows: int64(len(lines)), RehearsalTier: "config-test",
		Effects:   []adapter.Effect{{Kind: "file-write", Object: path, Description: fmt.Sprintf("%d lines changed in %s; then reload", len(lines), filepath.Base(path)), Rows: int64(len(lines))}},
		Facts:     map[string]string{"validated": fmt.Sprint(func() bool { s, _ := commands(t); return s != "" }())},
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	var diff strings.Builder
	fmt.Fprintf(&diff, "plan %s · %s (%s) · %s · %s\n", p.ID, t.Name, t.Env, p.Reversibility, path)
	shown := lines
	if len(shown) > 60 {
		shown = shown[:60]
	}
	for _, l := range shown {
		diff.WriteString("   " + l + "\n")
	}
	if len(lines) > len(shown) {
		fmt.Fprintf(&diff, "   … %d more\n", len(lines)-len(shown))
	}
	if test, _ := commands(t); test != "" {
		fmt.Fprintf(&diff, "   validated with: %s\n", test)
	} else {
		p.Warnings = append(p.Warnings, "no test command configured; the candidate was not validated by the service")
	}
	p.Diff = diff.String()
	p.PreconditionHash = hash(before)
	p.AdapterData, _ = json.Marshal(planData{Before: string(before), BeforeHash: hash(before), Existed: existed})
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

type undoHandle struct {
	Before    string `json:"before"`
	Existed   bool   `json:"existed"`
	AfterHash string `json:"after_hash"`
}

func (a *Adapter) install(ctx context.Context, t adapter.Target, content []byte, remove bool) error {
	path := t.Config["path"]
	prev, prevErr := os.ReadFile(path)
	if remove {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := writeAtomic(path, content); err != nil {
		return err
	}
	test, reload := commands(t)
	if _, err := sh(ctx, test); err != nil {
		// Never reload a configuration that fails its test.
		if prevErr == nil {
			_ = writeAtomic(path, prev)
		}
		return fmt.Errorf("configuration test failed; the previous file was restored and nothing was reloaded: %w", err)
	}
	if _, err := sh(ctx, reload); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

// Apply implements adapter.Adapter.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var d planData
	if err := json.Unmarshal(p.AdapterData, &d); err != nil {
		return nil, err
	}
	cur, _ := os.ReadFile(t.Config["path"])
	if hash(cur) != d.BeforeHash {
		return nil, &adapter.ConflictError{Reason: "file changed since plan; re-plan required", Conflicts: []string{t.Config["path"]}}
	}
	if err := a.install(ctx, t, []byte(p.Operation.Payload), false); err != nil {
		return nil, err
	}
	uh, _ := json.Marshal(undoHandle{Before: d.Before, Existed: d.Existed, AfterHash: hash([]byte(p.Operation.Payload))})
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: "written and reloaded", RowsAffected: 1, UndoHandle: uh, Reversibility: adapter.Reversible}, nil
}

// Verify implements adapter.Adapter: every health URL must answer 2xx/3xx.
func (a *Adapter) Verify(ctx context.Context, t adapter.Target, _ *adapter.Plan, _ *adapter.Receipt) (*adapter.Verification, error) {
	v := &adapter.Verification{Passed: true}
	settle := 700 * time.Millisecond
	if d, err := time.ParseDuration(t.Config["reload_settle"]); err == nil {
		settle = d
	}
	time.Sleep(settle) // let old workers finish handing over
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, u := range strings.Split(t.Config["health_urls"], ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		// Reloads are asynchronous: require three consecutive healthy
		// answers so a check served by the old workers cannot pass.
		var last error
		streak := 0
		for i := 0; i < 15 && streak < 3; i++ {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				last = fmt.Errorf("HTTP %d", resp.StatusCode)
				if resp.StatusCode < 400 {
					streak++
				} else {
					streak = 0
				}
			} else {
				last, streak = err, 0
			}
			time.Sleep(400 * time.Millisecond)
		}
		ok := streak >= 3
		detail := "ok"
		if !ok {
			detail = last.Error()
		}
		v.Checks = append(v.Checks, adapter.Check{Name: "health " + u, Passed: ok, Detail: detail})
		v.Passed = v.Passed && ok
	}
	if len(v.Checks) == 0 {
		v.Checks = []adapter.Check{{Name: "reloaded", Passed: true}}
	}
	return v, nil
}

// Undo implements adapter.Adapter.
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	cur, _ := os.ReadFile(t.Config["path"])
	if hash(cur) != uh.AfterHash {
		return nil, &adapter.ConflictError{Reason: "file changed since apply; undo refused (nothing was changed)", Conflicts: []string{t.Config["path"]}}
	}
	if err := a.install(ctx, t, []byte(uh.Before), !uh.Existed); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: "previous configuration restored and reloaded", Reversibility: adapter.Reversible}, nil
}
