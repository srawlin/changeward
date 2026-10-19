package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srawlin/changegate/pkg/adapter"
)

// fakeTracker is an in-memory issues API.
type fakeTracker struct {
	mu     sync.Mutex
	next   int
	issues map[string]map[string]any
	emails int
}

func (f *fakeTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	write := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "POST" && r.URL.Path == "/issues":
		f.next++
		id := fmt.Sprint(f.next)
		body["id"] = id
		f.issues[id] = body
		write(201, body)
	case r.Method == "POST" && r.URL.Path == "/emails":
		f.emails++
		write(202, map[string]any{"queued": true})
	case len(parts) == 2 && parts[0] == "issues":
		is, ok := f.issues[parts[1]]
		if !ok {
			write(404, map[string]any{"error": "not found"})
			return
		}
		switch r.Method {
		case "GET":
			write(200, is)
		case "PUT":
			body["id"] = parts[1]
			f.issues[parts[1]] = body
			write(200, body)
		case "DELETE":
			delete(f.issues, parts[1])
			w.WriteHeader(204)
		}
	default:
		write(404, nil)
	}
}

const manifest = `api: tracker
operations:
  - match: {method: POST, path: /issues}
    inverse: {method: DELETE, path: "/issues/{response.id}"}
  - match: {method: PUT, path: "/issues/{id}"}
    before: {method: GET, path: "/issues/{id}"}
    inverse: {method: PUT, path: "/issues/{id}", body: "{before}"}
  - match: {method: DELETE, path: "/issues/{id}"}
    before: {method: GET, path: "/issues/{id}"}
    inverse: {method: POST, path: /issues, body: "{before}"}
    reversibility: compensatable
  - match: {method: POST, path: /emails}
    reversibility: irreversible
`

func setup(t *testing.T) (*fakeTracker, adapter.Target) {
	f := &fakeTracker{issues: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	mp := filepath.Join(t.TempDir(), "tracker.yaml")
	_ = os.WriteFile(mp, []byte(manifest), 0o644)
	return f, adapter.Target{Name: "tracker", Adapter: "httpapi", Env: "shared-dev", Config: map[string]string{"base_url": srv.URL, "manifest": mp, "header.Authorization": "Bearer t"}}
}

func op(method, path string, body any) adapter.Operation {
	b, _ := json.Marshal(Request{Method: method, Path: path, Body: body})
	return adapter.Operation{Kind: "http", Payload: string(b)}
}

func TestHTTPCompensation(t *testing.T) {
	ctx := context.Background()
	f, tg := setup(t)
	a := New()
	// Create → undo deletes it.
	p, err := a.Plan(ctx, tg, op("POST", "/issues", map[string]any{"title": "bug", "status": "open"}))
	if err != nil || p.Reversibility != adapter.Reversible {
		t.Fatalf("plan: %v %+v", err, p)
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 {
		t.Fatal("not created")
	}
	if _, err := a.Undo(ctx, tg, p, rc); err != nil || len(f.issues) != 0 {
		t.Fatalf("undo create: %v %d", err, len(f.issues))
	}
	// Update → diff shown, undo restores, drift and stale undo refused.
	p, _ = a.Plan(ctx, tg, op("POST", "/issues", map[string]any{"title": "bug", "status": "open"}))
	_, _ = a.Apply(ctx, tg, p)
	p, err = a.Plan(ctx, tg, op("PUT", "/issues/2", map[string]any{"title": "bug", "status": "closed"}))
	if err != nil || !strings.Contains(p.Diff, "~ status: open → closed") {
		t.Fatalf("update plan: %v\n%s", err, p.Diff)
	}
	rc, err = a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Undo(ctx, tg, p, rc); err != nil || f.issues["2"]["status"] != "open" {
		t.Fatalf("undo update: %v %v", err, f.issues["2"])
	}
	p, _ = a.Plan(ctx, tg, op("PUT", "/issues/2", map[string]any{"title": "bug", "status": "closed"}))
	f.issues["2"]["title"] = "edited by someone"
	var ce *adapter.ConflictError
	if _, err := a.Apply(ctx, tg, p); !errors.As(err, &ce) {
		t.Fatalf("drift: %v", err)
	}
	p, _ = a.Plan(ctx, tg, op("PUT", "/issues/2", map[string]any{"title": "x", "status": "closed"}))
	rc, _ = a.Apply(ctx, tg, p)
	f.issues["2"]["status"] = "reopened"
	if _, err := a.Undo(ctx, tg, p, rc); !errors.As(err, &ce) {
		t.Fatalf("stale undo: %v", err)
	}
	// Delete is compensatable (re-created with a new id).
	p, _ = a.Plan(ctx, tg, op("DELETE", "/issues/2", nil))
	if p.Reversibility != adapter.Compensatable || !p.Destructive {
		t.Fatalf("delete: %s", p.Reversibility)
	}
	rc, _ = a.Apply(ctx, tg, p)
	if _, err := a.Undo(ctx, tg, p, rc); err != nil || len(f.issues) != 1 {
		t.Fatalf("undo delete: %v %d", err, len(f.issues))
	}
	// Irreversible and unknown calls.
	if p, _ := a.Plan(ctx, tg, op("POST", "/emails", map[string]any{"to": "x"})); p.Reversibility != adapter.Irreversible {
		t.Fatal("emails must be irreversible")
	}
	if p, _ := a.Plan(ctx, tg, op("PATCH", "/billing/charge", nil)); p.Reversibility != adapter.Irreversible || len(p.Warnings) == 0 {
		t.Fatal("unmatched call must be irreversible")
	}
	if f.emails != 0 {
		t.Fatal("planning must not send requests that change state")
	}
}
