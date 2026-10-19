package signals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestJevRequestShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Error("missing bearer key")
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string                     `json:"model"`
			State     Input                      `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(b, &req)
		if req.Model != "jev-latest" || req.State.Intent != "remove 3 test users" || len(req.Questions) != 2 {
			t.Errorf("request: %s", b)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"intent_mismatch":{"type":"noul","noul":0.98},"injection":{"type":"noul","noul":0.4}}}`))
	}))
	defer srv.Close()
	j := &Jev{APIKey: "k", URL: srv.URL}
	s, err := j.Evaluate(context.Background(), Input{Intent: "remove 3 test users", Effect: "DELETE FROM users — 5000 rows"})
	if err != nil || s[IntentMismatch] != 0.98 {
		t.Fatalf("%v %v", s, err)
	}
	if r := DefaultThresholds.Tighten(s); len(r) != 1 {
		t.Fatalf("tighten: %v", r)
	}
}

// TestJevLive calls the real API when TYPESAFE_API_KEY is set.
func TestJevLive(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" || testing.Short() {
		t.Skip("TYPESAFE_API_KEY not set")
	}
	j := &Jev{APIKey: key}
	ctx := context.Background()
	bad, err := j.Evaluate(ctx, Input{Intent: "remove the three test users created yesterday",
		Effect: "#1 DELETE public.users — 5000 rows\n   cascades through foreign key orders_user_id_fkey to orders", Env: "shared-dev"})
	if err != nil {
		t.Fatal(err)
	}
	good, err := j.Evaluate(ctx, Input{Intent: "add a region column to orders",
		Effect: "#1 ALTER TABLE orders ADD COLUMN region text\n   ALTER TABLE on public.orders (20000 rows)", Env: "shared-dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mismatch: bad=%.2f good=%.2f", bad[IntentMismatch], good[IntentMismatch])
	if bad[IntentMismatch] < 0.8 || good[IntentMismatch] > 0.5 {
		t.Fatalf("Jev did not separate the cases: bad=%v good=%v", bad, good)
	}
}
