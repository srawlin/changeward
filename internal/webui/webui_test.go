package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

func TestWebUI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pol, _ := policy.New(nil)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"toykv": toykv.New()}, Policy: pol}
	tg := adapter.Target{Name: "kv", Adapter: "toykv", Env: "personal", Config: map[string]string{"path": filepath.Join(dir, "kv.json")}}
	svc := &session.Service{Engine: eng, Store: st, Resolve: func(string) (adapter.Target, error) { return tg, nil }, StateDir: dir}
	s, _ := svc.Start(ctx, session.StartOptions{Target: "kv", Principal: model.Principal{Kind: "agent", Agent: "agent:test", Human: "user:dev"}})
	c, _ := eng.Propose(ctx, engine.ProposeRequest{Target: tg, Operation: adapter.Operation{Kind: "kv", Payload: "set <b>x</b> 1"}, Principal: s.Principal, SessionID: s.ID})
	c, _ = eng.Apply(ctx, c.ID, tg, "agent:test")

	srv := httptest.NewServer(New(eng, svc, "user:dev").Handler())
	defer srv.Close()
	get := func(p string) string {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		return string(b)
	}
	home := get("/")
	if !strings.Contains(home, c.ID) || !strings.Contains(home, "verified") || strings.Contains(home, "<b>x</b>") {
		t.Fatalf("index (must list the change and escape HTML):\n%s", home)
	}
	page := get("/changes/" + c.ID + "/view")
	m := regexp.MustCompile(`name="token" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no undo form:\n%s", page)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.PostForm(srv.URL+"/changes/"+c.ID+"/undo", url.Values{"token": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF protection: %d", r.StatusCode)
	}
	r, err = client.PostForm(srv.URL+"/changes/"+c.ID+"/undo", url.Values{"token": {m[1]}, "chain": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("undo: %d", r.StatusCode)
	}
	c, _ = eng.Get(ctx, c.ID)
	if c.State != model.Undone {
		t.Fatalf("state %s", c.State)
	}
}
