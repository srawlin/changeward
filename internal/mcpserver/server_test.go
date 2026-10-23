package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srawlin/changeward/adapters/postgres"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

func testDB(t *testing.T) string {
	admin := os.Getenv("CW_TEST_PG18")
	if admin == "" {
		admin = "postgres://postgres:pg@localhost:55418/postgres?sslmode=disable"
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skip("no Postgres 18 test server")
	}
	name := fmt.Sprintf("cwmcp_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(ctx)
	})
	dsn, _ := postgres.WithDatabase(admin, name)
	tc, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close(ctx) }()
	if _, err := tc.Exec(ctx, `CREATE TABLE users (id serial PRIMARY KEY, email text, active bool DEFAULT true);
		INSERT INTO users(email) SELECT 'u' || g || '@x.io' FROM generate_series(1, 2000) g;`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

type harness struct {
	cs     *mcp.ClientSession
	answer bool
	asked  int
}

func setup(t *testing.T, env string) *harness {
	t.Helper()
	dsn := testDB(t)
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	if err != nil {
		t.Fatal(err)
	}
	pg := postgres.New(postgres.Options{StateDir: dir})
	t.Cleanup(func() { pg.Close(); _ = st.Close() })
	pol, _ := policy.New(nil)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"postgres": pg}, Policy: pol}
	resolve := func(string) (adapter.Target, error) {
		return adapter.Target{Name: "app", Adapter: "postgres", Env: env, Config: map[string]string{"dsn": dsn, "sensitive.0": "users.email"}}, nil
	}
	s := &Server{Engine: eng, Sessions: &session.Service{Engine: eng, Store: st, Resolve: resolve, StateDir: dir}, Querier: pg,
		Target: "app", Human: "user:dev", Version: "test"}
	h := &harness{answer: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1.0"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			h.asked++
			if !strings.Contains(r.Params.Message, "UPDATE public.users") {
				t.Errorf("elicitation message lacks the diff: %s", r.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": h.answer}}, nil
		},
	})
	ct, stt := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := s.New().Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	if h.cs, err = client.Connect(ctx, ct, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.cs.Close() })
	return h
}

func (h *harness) call(t *testing.T, name string, args map[string]any) (string, map[string]any) {
	t.Helper()
	r, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	sc, _ := r.StructuredContent.(map[string]any)
	if r.IsError {
		return "ERROR " + b.String(), sc
	}
	return b.String(), sc
}

func TestMCPPersonalFlow(t *testing.T) {
	h := setup(t, "personal")
	tools, err := h.cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 8 {
		t.Fatalf("tools: %v %d", err, len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if tl.Name == "apply_change" && (tl.Annotations == nil || tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint) {
			t.Fatal("apply_change must be annotated destructive")
		}
		if tl.Name == "query" && !tl.Annotations.ReadOnlyHint {
			t.Fatal("query must be read-only")
		}
	}
	txt, _ := h.call(t, "query", map[string]any{"sql": "SELECT id, email FROM users ORDER BY id LIMIT 3"})
	if !strings.Contains(txt, "•••") || strings.Contains(txt, "@x.io") {
		t.Fatalf("query not redacted: %s", txt)
	}
	if txt, _ := h.call(t, "query", map[string]any{"sql": "DELETE FROM users"}); !strings.HasPrefix(txt, "ERROR") {
		t.Fatalf("query must refuse writes: %s", txt)
	}
	txt, sc := h.call(t, "propose_change", map[string]any{"sql": "UPDATE users SET active = false WHERE id <= 10", "intent": "deactivate test users"})
	if sc["decision"] != "approve" || !strings.Contains(txt, "10 rows") {
		t.Fatalf("propose: %s %v", txt, sc)
	}
	txt, sc = h.call(t, "apply_change", map[string]any{"plan_id": sc["plan_id"]})
	if sc["state"] != "verified" {
		t.Fatalf("apply: %s", txt)
	}
	changeID := sc["change_id"]
	txt, _ = h.call(t, "list_session_changes", nil)
	if !strings.Contains(txt, "verified") {
		t.Fatalf("list: %s", txt)
	}
	txt, sc = h.call(t, "undo_change", map[string]any{"change": "1"})
	if sc["state"] != "undone" || sc["change_id"] != changeID {
		t.Fatalf("undo: %s", txt)
	}
	txt, _ = h.call(t, "query", map[string]any{"sql": "SELECT count(*) FROM users WHERE NOT active"})
	if !strings.Contains(txt, "\n0\n") {
		t.Fatalf("after undo: %s", txt)
	}
}

func TestMCPElicitation(t *testing.T) {
	// shared-dev: >1000 rows requires inline confirmation, asked via MCP elicitation.
	h := setup(t, "shared-dev")
	_, sc := h.call(t, "propose_change", map[string]any{"sql": "UPDATE users SET active = false WHERE id <= 1500"})
	if sc["decision"] != "require" {
		t.Fatalf("expected approval requirement: %v", sc)
	}
	h.answer = false
	_, sc2 := h.call(t, "apply_change", map[string]any{"plan_id": sc["plan_id"]})
	if sc2["state"] != "rejected" || h.asked != 1 {
		t.Fatalf("declined: %v asked=%d", sc2, h.asked)
	}
	_, sc = h.call(t, "propose_change", map[string]any{"sql": "UPDATE users SET active = false WHERE id <= 1500"})
	h.answer = true
	var txt string
	txt, sc = h.call(t, "apply_change", map[string]any{"plan_id": sc["plan_id"]})
	if sc["state"] != "verified" || h.asked != 2 {
		t.Fatalf("approved: %s %v", txt, sc)
	}
}

func TestMCPProdIsOutOfBand(t *testing.T) {
	h := setup(t, "prod")
	_, sc := h.call(t, "propose_change", map[string]any{"sql": "DELETE FROM users WHERE id = 1"})
	if sc["state"] != "rejected" { // no incident reference
		t.Fatalf("prod without incident: %v", sc)
	}
	if h.asked != 0 {
		t.Fatal("prod must never use in-band approval")
	}
}
