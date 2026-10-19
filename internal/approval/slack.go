// Package approval implements out-of-band approval channels. Slack is the
// first: pending changes are posted with Approve/Reject buttons; clicks are
// verified (Slack request signing) and recorded as approvals bound to the
// exact plan hash.
package approval

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
)

// Slack posts approval requests and handles interactions.
type Slack struct {
	WebhookURL    string
	BotToken      string
	Channel       string
	SigningSecret string
	Users         map[string]string
	Engine        *engine.Engine
	// API is the Slack Web API base (overridable in tests).
	API    string
	Client *http.Client
	Now    func() time.Time
}

func (s *Slack) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *Slack) api() string {
	if s.API != "" {
		return s.API
	}
	return "https://slack.com/api"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Message builds the Block Kit message for a pending change.
func Message(c *model.Change, interactive bool) map[string]any {
	d := c.Decision
	reasons := ""
	if d != nil {
		reasons = strings.Join(d.Reasons, "; ")
	}
	who := c.Principal.String()
	blocks := []any{
		map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": "Changegate: approval needed (" + c.Target.Env + ")"}},
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("*%s* on `%s` by %s\n%s", c.Plan.Reversibility, c.Target.Name, who, reasons)}},
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + truncate(c.Plan.Diff, 2800) + "```"}},
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("plan `%s` · change `%s` · expires %s", c.Plan.ID, c.ID, c.Plan.ExpiresAt)}}},
	}
	if interactive {
		val := c.Plan.ID + "|" + c.Plan.Hash
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "changegate", "elements": []any{
			map[string]any{"type": "button", "action_id": "approve", "style": "primary", "text": map[string]any{"type": "plain_text", "text": "Approve"}, "value": val},
			map[string]any{"type": "button", "action_id": "reject", "style": "danger", "text": map[string]any{"type": "plain_text", "text": "Reject"}, "value": val},
		}})
	} else {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": "Approve with `changegate approve " + c.Plan.ID + "`"}}})
	}
	return map[string]any{"text": "Changegate approval needed: " + c.Plan.ID, "blocks": blocks}
}

// NotifyPending implements engine.Notifier.
func (s *Slack) NotifyPending(ctx context.Context, c *model.Change) error {
	if c.Decision == nil || c.Decision.Channel != "out-of-band" {
		return nil // inline approvals happen in the terminal or agent client
	}
	switch {
	case s.BotToken != "" && s.Channel != "":
		msg := Message(c, true)
		msg["channel"] = s.Channel
		var out struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := s.post(ctx, s.api()+"/chat.postMessage", msg, "Bearer "+s.BotToken, &out); err != nil {
			return err
		}
		if !out.OK {
			return fmt.Errorf("slack: %s", out.Error)
		}
		return nil
	case s.WebhookURL != "":
		return s.post(ctx, s.WebhookURL, Message(c, false), "", nil)
	}
	return nil
}

func (s *Slack) post(ctx context.Context, u string, body any, auth string, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack: %s: %s", resp.Status, bytes.TrimSpace(rb))
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}

// Verify checks Slack's request signature (v0, HMAC-SHA256) and freshness.
func (s *Slack) Verify(h http.Header, body []byte) error {
	if s.SigningSecret == "" {
		return errors.New("slack signing secret not configured")
	}
	ts := h.Get("X-Slack-Request-Timestamp")
	sig := h.Get("X-Slack-Signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("missing timestamp")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if d := now().Unix() - sec; d > 300 || d < -300 {
		return errors.New("stale request")
	}
	mac := hmac.New(sha256.New, []byte(s.SigningSecret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("bad signature")
	}
	return nil
}

type interaction struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	ResponseURL string `json:"response_url"`
}

// Identity maps a Slack user to a Changegate identity.
func (s *Slack) Identity(userID string) string {
	if u, ok := s.Users[userID]; ok {
		return "user:" + strings.TrimPrefix(u, "user:")
	}
	return "slack:" + userID
}

// ServeHTTP handles POST /slack/interactions.
func (s *Slack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Verify(r.Header, body); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var in interaction
	if err := json.Unmarshal([]byte(form.Get("payload")), &in); err != nil || in.Type != "block_actions" || len(in.Actions) == 0 {
		http.Error(w, "unsupported payload", http.StatusBadRequest)
		return
	}
	act := in.Actions[0]
	planID, hash, ok := strings.Cut(act.Value, "|")
	if !ok || (act.ActionID != "approve" && act.ActionID != "reject") {
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	who := s.Identity(in.User.ID)
	c, err := s.Engine.Approve(r.Context(), planID, engine.ApproveRequest{PlanHash: hash, Approver: who, Channel: "slack", Approved: act.ActionID == "approve",
		Reason: "via Slack by " + in.User.Username})
	text := ""
	switch {
	case err != nil:
		text = fmt.Sprintf(":warning: %s could not %s plan `%s`: %v", who, act.ActionID, planID, err)
	case c.State == model.Approved:
		text = fmt.Sprintf(":white_check_mark: approved by %s. Apply with `changegate apply %s` (the agent can also call apply_change).", who, planID)
	case c.State == model.Rejected:
		text = fmt.Sprintf(":no_entry: rejected by %s.", who)
	default:
		text = fmt.Sprintf(":ballot_box_with_check: %s approved; more approvals needed (%s).", who, c.State)
	}
	if in.ResponseURL != "" {
		_ = s.post(r.Context(), in.ResponseURL, map[string]any{"text": text, "replace_original": false}, "", nil)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
}
