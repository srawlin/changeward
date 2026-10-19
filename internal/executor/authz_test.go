package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/internal/session"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

func TestAuthz(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cg.db"))
	defer func() { _ = st.Close() }()
	pol, _ := policy.New(nil)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{}, Policy: pol}
	tok, gw := NewToken("gateway", []string{RolePropose})
	srv := httptest.NewServer((&Server{Engine: eng, Sessions: &session.Service{Engine: eng, Store: st, StateDir: dir,
		Resolve: func(string) (adapter.Target, error) { return adapter.Target{}, nil }}, Tokens: []Token{gw}}).Handler())
	defer srv.Close()
	call := func(env, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/authz/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("x-changegate-env", env)
		req.Header.Set("x-changegate-server", "postgres-mcp")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		env, body string
		want      int
	}{
		{"prod", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 200},
		{"prod", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_sql","arguments":{"sql":"SELECT * FROM users"}}}`, 200},
		{"prod", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_sql","arguments":{"sql":"DELETE FROM users"}}}`, 403},
		{"personal", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"postgres.query","arguments":{"query":"UPDATE t SET a = 1"}}}`, 403},
		{"prod", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_email","arguments":{"to":"x"}}}`, 403},
	}
	for i, c := range cases {
		if got := call(c.env, c.body); got != c.want {
			t.Errorf("case %d: %d, want %d", i, got, c.want)
		}
	}
	// Envoy ext_authz style: original path appended, token in x-changegate-token, client's own Authorization kept.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/authz/mcp/mcp/messages", strings.NewReader(`{"method":"tools/call","params":{"name":"execute_sql","arguments":{"sql":"TRUNCATE t"}}}`))
	req.Header.Set("Authorization", "Bearer client-token-not-for-changegate")
	req.Header.Set("X-Changegate-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("x-changegate-decision") != "deny" {
		t.Fatalf("envoy-style request: %d %s", resp.StatusCode, resp.Header.Get("x-changegate-decision"))
	}
	// Tools the gateway declares read-only pass; denials are JSON-RPC errors.
	for _, c := range []struct {
		readOnly string
		code     int
	}{{"echo,get-*", 200}, {"get-*", 403}, {"", 403}} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/authz/mcp/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
		req.Header.Set("X-Changegate-Token", tok)
		req.Header.Set("X-Changegate-Env", "personal")
		req.Header.Set("X-Changegate-Read-Only-Tools", c.readOnly)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			ID    int `json:"id"`
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Fatalf("read-only %q: %d", c.readOnly, resp.StatusCode)
		}
		if c.code == 403 && (out.ID != 9 || out.Error.Code != -32001 || !strings.Contains(out.Error.Message, "Changegate")) {
			t.Fatalf("denial must be a JSON-RPC error for the call: %+v", out)
		}
	}
	// MCP's GET (SSE stream) and DELETE (end session) pass; tokens are still required.
	for _, m := range []string{"GET", "DELETE"} {
		req, _ := http.NewRequest(m, srv.URL+"/v1/authz/mcp/mcp", nil)
		req.Header.Set("X-Changegate-Token", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s through the gateway: %d", m, resp.StatusCode)
		}
		req, _ = http.NewRequest(m, srv.URL+"/v1/authz/mcp/mcp", nil)
		if resp, err = http.DefaultClient.Do(req); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s without a token: %d", m, resp.StatusCode)
		}
	}
	evs, _ := st.ListEvents(t.Context(), store.EventFilter{})
	if len(evs) == 0 || !strings.HasPrefix(evs[len(evs)-1].Type, "gateway.") {
		t.Fatalf("gateway decisions must be audited: %+v", evs)
	}
}
