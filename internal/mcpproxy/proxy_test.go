package mcpproxy

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
	"github.com/srawlin/changeward/internal/mcpserver"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

// TestMain doubles as a fake upstream MCP server when CW_FAKE_UPSTREAM is set.
func TestMain(m *testing.M) {
	if log := os.Getenv("CW_FAKE_UPSTREAM"); log != "" {
		fakeUpstream(log)
		return
	}
	os.Exit(m.Run())
}

type sqlIn struct {
	SQL string `json:"sql"`
}
type emailIn struct {
	To string `json:"to"`
}

func fakeUpstream(logPath string) {
	record := func(s string) {
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		_, _ = fmt.Fprintln(f, s)
		_ = f.Close()
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-db"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "execute_sql", Description: "Run SQL"}, func(_ context.Context, _ *mcp.CallToolRequest, in sqlIn) (*mcp.CallToolResult, any, error) {
		record("execute_sql " + in.SQL)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "upstream ran: " + in.SQL}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "send_email", Description: "Send an email"}, func(_ context.Context, _ *mcp.CallToolRequest, in emailIn) (*mcp.CallToolResult, any, error) {
		record("send_email " + in.To)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent to " + in.To}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "get_weather", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		record("get_weather")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sunny"}}}, nil, nil
	})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

func TestProxy(t *testing.T) {
	admin := os.Getenv("CW_TEST_PG18")
	if admin == "" {
		admin = "postgres://postgres:pg@localhost:55418/postgres?sslmode=disable"
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skip("no Postgres 18 test server")
	}
	name := fmt.Sprintf("cwproxy_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); _ = c.Close(ctx) }()
	dsn, _ := postgres.WithDatabase(admin, name)
	tc, _ := pgx.Connect(ctx, dsn)
	if _, err := tc.Exec(ctx, "CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatal(err)
	}
	_ = tc.Close(ctx)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "upstream.log")
	t.Setenv("CW_FAKE_UPSTREAM", logPath)
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pg := postgres.New(postgres.Options{StateDir: dir})
	defer pg.Close()
	pol, _ := policy.New(nil)
	reg := engine.MapRegistry{"postgres": pg}
	eng := &engine.Engine{Store: st, Adapters: reg, Policy: pol}
	resolve := func(string) (adapter.Target, error) {
		return adapter.Target{Name: "app", Adapter: "postgres", Env: "personal", Config: map[string]string{"dsn": dsn}}, nil
	}
	inner := &mcpserver.Server{Engine: eng, Sessions: &session.Service{Engine: eng, Store: st, Resolve: resolve, StateDir: dir}, Querier: pg, Target: "app", Human: "user:dev"}
	exe, _ := os.Executable()
	p := &Proxy{Inner: inner, Command: []string{exe}, Name: "fake-db"}
	reg[p.AdapterName()] = p.Adapter()
	srv, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.upstream.Close() }()

	elicited := 0
	client := mcp.NewClient(&mcp.Implementation{Name: "agent"}, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicited++
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		}})
	ct, stt := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	call := func(name string, args any) string {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	upstreamLog := func() string { b, _ := os.ReadFile(logPath); return string(b) }

	if got := call("execute_sql", map[string]any{"sql": "SELECT * FROM t"}); !strings.Contains(got, "upstream ran: SELECT") {
		t.Fatalf("read pass-through: %s", got)
	}
	got := call("execute_sql", map[string]any{"sql": "UPDATE t SET v = 'z' WHERE id = 1"})
	if !strings.Contains(got, "applied") || strings.Contains(upstreamLog(), "UPDATE") {
		t.Fatalf("write must be applied by Changeward, not upstream: %s\nupstream: %s", got, upstreamLog())
	}
	if got := call("changeward_undo_change", map[string]any{"change": "1"}); !strings.Contains(got, "undone") {
		t.Fatalf("undo through proxy: %s", got)
	}
	if got := call("get_weather", map[string]any{}); got != "sunny" {
		t.Fatalf("read-only tool: %s", got)
	}
	got = call("send_email", map[string]any{"to": "ceo@example.com"})
	if got != "sent to ceo@example.com" || elicited != 1 {
		t.Fatalf("gated tool: %q elicited=%d", got, elicited)
	}
	cs2, _ := st.ListChanges(ctx, store.ChangeFilter{})
	var kinds []string
	for _, c := range cs2 {
		kinds = append(kinds, c.Operation.Kind+":"+string(c.State))
	}
	if strings.Join(kinds, ",") != "sql:undone,mcp-tool:verified" {
		t.Fatalf("records: %v", kinds)
	}
	if cs2[1].Plan.Reversibility != adapter.Irreversible || cs2[1].Principal.Kind != "agent" {
		t.Fatalf("tool change record: %+v", cs2[1].Plan)
	}
	_ = model.Verified
}
