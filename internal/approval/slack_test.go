package approval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSlackFlow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pol, _ := policy.New(nil)
	var posted []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		if r.Header.Get("Authorization") != "Bearer xoxb-test" && r.URL.Path == "/chat.postMessage" {
			t.Errorf("missing bot token")
		}
		posted = append(posted, m)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"toykv": toykv.New()}, Policy: pol}
	sl := &Slack{BotToken: "xoxb-test", Channel: "C1", SigningSecret: "s3cret", Engine: eng, API: api.URL,
		Users: map[string]string{"U_STEVE": "steve", "U_ALICE": "alice"}}
	eng.Notifier = sl
	_ = st.CreateSession(ctx, &model.Session{ID: "s_inc", Target: "kv", Env: "prod", IncidentRef: "INC-7", StartedAt: time.Now().UTC().Format(time.RFC3339)}, nil)
	tgt := adapter.Target{Name: "kv", Adapter: "toykv", Env: "prod", Config: map[string]string{"path": filepath.Join(dir, "kv.json")}}
	c, err := eng.Propose(ctx, engine.ProposeRequest{Target: tgt, Operation: adapter.Operation{Kind: "kv", Payload: "set a 1"},
		Principal: model.Principal{Kind: "agent", Agent: "agent:ops", Human: "user:steve"}, SessionID: "s_inc"})
	if err != nil || c.State != model.AwaitingApproval {
		t.Fatalf("propose: %v %s", err, c.State)
	}
	if len(posted) != 1 || posted[0]["channel"] != "C1" || !strings.Contains(mustJSON(posted[0]), `"action_id":"approve"`) {
		t.Fatalf("posted: %v", posted)
	}
	click := func(user, action, sig string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{"type": "block_actions", "user": map[string]any{"id": user, "username": user},
			"actions": []any{map[string]any{"action_id": action, "value": c.Plan.ID + "|" + c.Plan.Hash}}})
		body := []byte("payload=" + url.QueryEscape(string(payload)))
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		if sig == "" {
			sig = sign("s3cret", ts, body)
		}
		req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(string(body)))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", sig)
		rec := httptest.NewRecorder()
		sl.ServeHTTP(rec, req)
		return rec
	}
	if rec := click("U_ALICE", "approve", "v0=bad"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature accepted: %d", rec.Code)
	}
	if rec := click("U_STEVE", "approve", ""); !strings.Contains(rec.Body.String(), "separation of duties") {
		t.Fatalf("self-approval: %s", rec.Body.String())
	}
	if rec := click("U_ALICE", "approve", ""); !strings.Contains(rec.Body.String(), "approved by user:alice") {
		t.Fatalf("approve: %s", rec.Body.String())
	}
	c, _ = eng.Get(ctx, c.ID)
	if c.State != model.Approved || c.Approvals[0].Channel != "slack" {
		t.Fatalf("state %s %+v", c.State, c.Approvals)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
