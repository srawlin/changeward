package telemetry_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/internal/telemetry"
	"github.com/srawlin/changeward/pkg/adapter"
)

func TestOTLPExport(t *testing.T) {
	var mu sync.Mutex
	var names []string
	traces := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("authorization") != "Bearer k" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		var p struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID string `json:"traceId"`
						Name    string `json:"name"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		for _, rs := range p.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					names = append(names, s.Name)
					traces[s.TraceID] = true
				}
			}
		}
		mu.Unlock()
	}))
	defer srv.Close()
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pol, _ := policy.New(nil)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"toykv": toykv.New()}, Policy: pol,
		Observer: telemetry.NewExporter(srv.URL, map[string]string{"authorization": "Bearer k"})}
	tg := adapter.Target{Name: "kv", Adapter: "toykv", Env: "personal", Config: map[string]string{"path": filepath.Join(dir, "kv.json")}}
	c, err := eng.Propose(context.Background(), engine.ProposeRequest{Target: tg, Operation: adapter.Operation{Kind: "kv", Payload: "set a 1"}, Principal: model.Principal{Kind: "agent", Agent: "agent:x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Apply(context.Background(), c.ID, tg, "t"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(names)
		mu.Unlock()
		if n >= 8 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(names, ",")
	if len(traces) != 1 || !strings.Contains(all, "changeward.verified") || !strings.Contains(all, "changeward.change kv") {
		t.Fatalf("spans: %v traces=%d", names, len(traces))
	}
	if telemetry.NewExporter("", nil) != nil {
		t.Fatal("no endpoint must mean no exporter")
	}
}
