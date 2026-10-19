package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/srawlin/changegate/internal/backend"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/pkg/adapter"
)

// Client implements backend.Backend against a shared executor.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

var _ backend.Backend = (*Client)(nil)

// NewClient returns an executor client.
func NewClient(u, token string) *Client {
	return &Client{URL: strings.TrimRight(u, "/"), Token: token, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// APIError is an error returned by the executor.
type APIError struct {
	Status int
	Body   ErrorBody
}

func (e *APIError) Error() string { return fmt.Sprintf("executor: %s (HTTP %d)", e.Body.Error, e.Status) }

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		var eb ErrorBody
		_ = json.Unmarshal(b, &eb)
		if resp.StatusCode == http.StatusConflict {
			return &adapter.ConflictError{Reason: eb.Reason, Conflicts: eb.Conflicts}
		}
		if out != nil && resp.StatusCode == http.StatusUnprocessableEntity {
			_ = json.Unmarshal(b, out) // e.g. a partial undo result
		}
		return &APIError{Status: resp.StatusCode, Body: eb}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

// StartSession implements backend.Backend.
func (c *Client) StartSession(ctx context.Context, r backend.StartRequest) (*model.Session, error) {
	var s model.Session
	return &s, c.call(ctx, "POST", "/v1/sessions", r, &s)
}

// GetSession implements backend.Backend.
func (c *Client) GetSession(ctx context.Context, id string) (*model.Session, error) {
	var s model.Session
	return &s, c.call(ctx, "GET", "/v1/sessions/"+esc(id), nil, &s)
}

// EndSession implements backend.Backend.
func (c *Client) EndSession(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/v1/sessions/"+esc(id)+"/end", nil, nil)
}

// TargetAdapter implements backend.Backend.
func (c *Client) TargetAdapter(ctx context.Context, id string) (string, error) {
	var out struct{ Adapter string }
	err := c.call(ctx, "GET", "/v1/sessions/"+esc(id)+"/target", nil, &out)
	return out.Adapter, err
}

// Propose implements backend.Backend.
func (c *Client) Propose(ctx context.Context, r backend.ProposeRequest) (*model.Change, error) {
	var ch model.Change
	return &ch, c.call(ctx, "POST", "/v1/changes", r, &ch)
}

// Get implements backend.Backend.
func (c *Client) Get(ctx context.Context, ref string) (*model.Change, error) {
	var ch model.Change
	return &ch, c.call(ctx, "GET", "/v1/changes/"+esc(ref), nil, &ch)
}

// ApproveInline implements backend.Backend.
func (c *Client) ApproveInline(ctx context.Context, ref, planHash string, approved bool, channel string) (*model.Change, error) {
	var ch model.Change
	return &ch, c.call(ctx, "POST", "/v1/changes/"+esc(ref)+"/approve-inline", map[string]any{"plan_hash": planHash, "approved": approved, "channel": channel}, &ch)
}

// Approve records an out-of-band approval (token needs the approve role).
func (c *Client) Approve(ctx context.Context, ref, planHash string, approved bool, reason string) (*model.Change, error) {
	var ch model.Change
	return &ch, c.call(ctx, "POST", "/v1/changes/"+esc(ref)+"/approve", map[string]any{"plan_hash": planHash, "approved": approved, "reason": reason}, &ch)
}

// Apply implements backend.Backend.
func (c *Client) Apply(ctx context.Context, ref string) (*model.Change, error) {
	var ch model.Change
	err := c.call(ctx, "POST", "/v1/changes/"+esc(ref)+"/apply", nil, &ch)
	return &ch, err
}

// Undo implements backend.Backend.
func (c *Client) Undo(ctx context.Context, ref string, withDependents bool) (*backend.UndoResult, error) {
	var res backend.UndoResult
	err := c.call(ctx, "POST", "/v1/changes/"+esc(ref)+"/undo", map[string]any{"with_dependents": withDependents}, &res)
	if err != nil && res.Error != "" {
		return &res, errors.New(res.Error)
	}
	return &res, err
}

// Changes implements backend.Backend.
func (c *Client) Changes(ctx context.Context, id string) ([]*model.Change, error) {
	var cs []*model.Change
	return cs, c.call(ctx, "GET", "/v1/sessions/"+esc(id)+"/changes", nil, &cs)
}

// Checkpoint implements backend.Backend.
func (c *Client) Checkpoint(ctx context.Context, id, label string) (string, error) {
	var out struct {
		CheckpointID string `json:"checkpoint_id"`
	}
	err := c.call(ctx, "POST", "/v1/sessions/"+esc(id)+"/checkpoint", map[string]string{"label": label}, &out)
	return out.CheckpointID, err
}

// Query implements backend.Backend.
func (c *Client) Query(ctx context.Context, id, q string, maxRows int) (*adapter.QueryResult, error) {
	var out adapter.QueryResult
	return &out, c.call(ctx, "POST", "/v1/sessions/"+esc(id)+"/query", map[string]any{"query": q, "max_rows": maxRows}, &out)
}
