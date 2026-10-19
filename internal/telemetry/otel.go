// Package telemetry exports change lifecycles to an OpenTelemetry collector
// (OTLP/HTTP, JSON encoding). This is the customer's own observability
// pipeline, opt-in: nothing is sent unless an endpoint is configured.
//
// Each change is one trace (trace ID derived from the change ID): a root
// span from proposal to the terminal state, with a child span per
// transition. Attributes carry target, environment, adapter, decision,
// reversibility and rows; never plan contents or row values.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/srawlin/changegate/internal/model"
)

// Exporter sends spans to an OTLP/HTTP endpoint.
type Exporter struct {
	Endpoint string            // e.g. http://localhost:4318 (the /v1/traces path is appended)
	Headers  map[string]string // e.g. authorization for a hosted collector
	Service  string
	Client   *http.Client

	mu    sync.Mutex
	start map[string]time.Time // change ID → first seen
	last  map[string]time.Time // change ID → previous transition
}

// NewExporter returns an exporter, or nil when endpoint is empty.
func NewExporter(endpoint string, headers map[string]string) *Exporter {
	if endpoint == "" {
		return nil
	}
	return &Exporter{Endpoint: strings.TrimRight(endpoint, "/"), Headers: headers, Service: "changegate",
		Client: &http.Client{Timeout: 5 * time.Second}, start: map[string]time.Time{}, last: map[string]time.Time{}}
}

func traceID(changeID string) string {
	s := sha256.Sum256([]byte("trace:" + changeID))
	return hex.EncodeToString(s[:16])
}

func rootSpanID(changeID string) string {
	s := sha256.Sum256([]byte("root:" + changeID))
	return hex.EncodeToString(s[:8])
}

func randomSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func attr(k string, v any) kv {
	switch x := v.(type) {
	case int64:
		return kv{k, map[string]any{"intValue": fmt.Sprint(x)}}
	case bool:
		return kv{k, map[string]any{"boolValue": x}}
	}
	return kv{k, map[string]any{"stringValue": fmt.Sprint(v)}}
}

type span struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId,omitempty"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Start        string `json:"startTimeUnixNano"`
	End          string `json:"endTimeUnixNano"`
	Attributes   []kv   `json:"attributes"`
	Status       struct {
		Code int `json:"code"`
	} `json:"status"`
}

func attrs(c *model.Change) []kv {
	out := []kv{attr("changegate.change_id", c.ID), attr("changegate.state", string(c.State)), attr("changegate.target", c.Target.Name),
		attr("changegate.env", c.Target.Env), attr("changegate.adapter", c.Target.Adapter), attr("changegate.lane", c.Provenance.Lane),
		attr("changegate.principal.kind", c.Principal.Kind)}
	if c.SessionID != "" {
		out = append(out, attr("changegate.session_id", c.SessionID))
	}
	if c.Principal.Agent != "" {
		out = append(out, attr("changegate.principal.agent", c.Principal.Agent))
	}
	if c.Plan != nil {
		out = append(out, attr("changegate.plan_id", c.Plan.ID), attr("changegate.reversibility", string(c.Plan.Reversibility)),
			attr("changegate.destructive", c.Plan.Destructive), attr("changegate.blast_rows", c.Plan.BlastRows))
	}
	if c.Decision != nil {
		out = append(out, attr("changegate.decision", c.Decision.Decision))
	}
	if c.Receipt != nil {
		out = append(out, attr("changegate.rows_affected", c.Receipt.RowsAffected))
	}
	return out
}

// Observe records one transition. It implements engine.Observer.
func (x *Exporter) Observe(ctx context.Context, c *model.Change, from, to model.State) {
	now := time.Now()
	x.mu.Lock()
	first, ok := x.start[c.ID]
	if !ok {
		first = now
		if t, err := time.Parse(time.RFC3339Nano, c.CreatedAt); err == nil {
			first = t
		}
		x.start[c.ID] = first
	}
	prev, ok := x.last[c.ID]
	if !ok {
		prev = first
	}
	x.last[c.ID] = now
	terminal := to.Terminal() || to == model.Verified || to == model.Rejected || to == model.Undone || to == model.AwaitingApproval
	if to.Terminal() || to == model.Undone || to == model.Rejected {
		delete(x.start, c.ID)
		delete(x.last, c.ID)
	}
	x.mu.Unlock()

	tid := traceID(c.ID)
	s := span{TraceID: tid, SpanID: randomSpanID(), ParentSpanID: rootSpanID(c.ID), Name: "changegate." + string(to), Kind: 1,
		Start: fmt.Sprint(prev.UnixNano()), End: fmt.Sprint(now.UnixNano()), Attributes: append(attrs(c), attr("changegate.from_state", string(from)))}
	if to == model.Failed {
		s.Status.Code = 2
	}
	spans := []span{s}
	if terminal {
		// (Re)emit the root span covering the whole lifecycle so far; backends
		// keep the latest version by span ID.
		root := span{TraceID: tid, SpanID: rootSpanID(c.ID), Name: "changegate.change " + c.Target.Name, Kind: 1,
			Start: fmt.Sprint(first.UnixNano()), End: fmt.Sprint(now.UnixNano()), Attributes: attrs(c)}
		if to == model.Failed || to == model.Rejected {
			root.Status.Code = 2
		}
		spans = append(spans, root)
	}
	// Synchronous with a short cap: CLI processes exit right after a change,
	// and a lost export must never block or fail the change itself.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = x.send(sctx, spans)
}

func (x *Exporter) send(ctx context.Context, spans []span) error {
	payload := map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []kv{attr("service.name", x.Service)}},
		"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "changegate"}, "spans": spans}},
	}}}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.Endpoint+"/v1/traces", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range x.Headers {
		req.Header.Set(k, v)
	}
	resp, err := x.Client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
