// Package executor is the shared executor: an HTTP API next to the target
// systems that holds their credentials, runs the engine and enforces policy.
// Agents' MCP servers, CLIs and SDKs call it with a bearer token; the token's
// identity is the delegating human, so an agent can never approve its own
// change (§4.1, §6.3).
package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/srawlin/changegate/internal/backend"
	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/gitops"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/session"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

// Roles.
const (
	RolePropose = "propose" // start sessions, propose, apply, undo own changes, inline confirm
	RoleApprove = "approve" // out-of-band approval of others' changes
	RoleAdmin   = "admin"   // act on any session
)

// Token is a configured API token (only its SHA-256 is stored).
type Token struct {
	Name   string   `yaml:"name" json:"name"`
	SHA256 string   `yaml:"sha256" json:"sha256"`
	Roles  []string `yaml:"roles" json:"roles"`
}

func (t Token) has(role string) bool {
	for _, r := range t.Roles {
		if r == role || r == RoleAdmin {
			return true
		}
	}
	return false
}

// NewToken returns a random token and its config entry.
func NewToken(name string, roles []string) (string, Token) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := "cgx_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(tok))
	return tok, Token{Name: name, SHA256: hex.EncodeToString(sum[:]), Roles: roles}
}

// Server is the executor HTTP API.
type Server struct {
	Engine   *engine.Engine
	Sessions *session.Service
	Tokens   []Token
}

type ctxKey struct{}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(h, "Bearer ")
		if x := r.Header.Get("X-Changegate-Token"); x != "" {
			// Gateways (Envoy ext_authz, agentgateway) forward the client's own
			// Authorization header; they add Changegate's token here instead.
			tok, h = x, ""
		}
		if tok == "" || tok == h {
			writeErr(w, http.StatusUnauthorized, errors.New("missing bearer token"))
			return
		}
		sum := sha256.Sum256([]byte(tok))
		got := hex.EncodeToString(sum[:])
		for _, t := range s.Tokens {
			if subtle.ConstantTimeCompare([]byte(got), []byte(t.SHA256)) == 1 {
				next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, t)))
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
	}
}

func caller(r *http.Request) Token { t, _ := r.Context().Value(ctxKey{}).(Token); return t }

func identity(t Token) string { return "user:" + t.Name }

func (s *Server) local(r *http.Request) *backend.Local {
	return &backend.Local{Engine: s.Engine, Sessions: s.Sessions, Human: identity(caller(r))}
}

// Handler returns the API handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	h := func(pattern, role string, fn func(http.ResponseWriter, *http.Request) (any, error)) {
		mux.HandleFunc(pattern, s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !caller(r).has(role) {
				writeErr(w, http.StatusForbidden, errors.New("token lacks the "+role+" role"))
				return
			}
			v, err := fn(w, r)
			if err != nil {
				writeErr(w, statusOf(err), err)
				return
			}
			writeJSON(w, http.StatusOK, v)
		}))
	}
	h("POST /v1/sessions", RolePropose, s.startSession)
	h("GET /v1/sessions/{id}", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) { return s.ownSession(r, r.PathValue("id")) })
	h("POST /v1/sessions/{id}/end", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		if _, err := s.ownSession(r, r.PathValue("id")); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, s.local(r).EndSession(r.Context(), r.PathValue("id"))
	})
	h("GET /v1/sessions/{id}/target", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		if _, err := s.ownSession(r, r.PathValue("id")); err != nil {
			return nil, err
		}
		a, err := s.local(r).TargetAdapter(r.Context(), r.PathValue("id"))
		return map[string]string{"adapter": a}, err
	})
	h("GET /v1/sessions/{id}/changes", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		if _, err := s.ownSession(r, r.PathValue("id")); err != nil {
			return nil, err
		}
		return s.local(r).Changes(r.Context(), r.PathValue("id"))
	})
	h("POST /v1/sessions/{id}/checkpoint", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{ Label string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, err := s.ownSession(r, r.PathValue("id")); err != nil {
			return nil, err
		}
		id, err := s.local(r).Checkpoint(r.Context(), r.PathValue("id"), in.Label)
		return map[string]string{"checkpoint_id": id}, err
	})
	h("POST /v1/sessions/{id}/query", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Query   string `json:"query"`
			MaxRows int    `json:"max_rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, badRequest(err)
		}
		if _, err := s.ownSession(r, r.PathValue("id")); err != nil {
			return nil, err
		}
		return s.local(r).Query(r.Context(), r.PathValue("id"), in.Query, in.MaxRows)
	})
	h("POST /v1/changes", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in backend.ProposeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, badRequest(err)
		}
		if _, err := s.ownSession(r, in.SessionID); err != nil {
			return nil, err
		}
		return s.local(r).Propose(r.Context(), in)
	})
	h("GET /v1/changes/{ref}", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) { return s.ownChange(r) })
	h("POST /v1/changes/{ref}/apply", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		c, err := s.ownChange(r)
		if err != nil {
			return nil, err
		}
		return s.local(r).Apply(r.Context(), c.ID)
	})
	h("POST /v1/changes/{ref}/undo", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			WithDependents bool `json:"with_dependents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c, err := s.ownChange(r)
		if err != nil {
			return nil, err
		}
		res, err := s.local(r).Undo(r.Context(), c.ID, in.WithDependents)
		if err != nil && res != nil {
			return res, err
		}
		return res, err
	})
	h("POST /v1/changes/{ref}/approve-inline", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			PlanHash string `json:"plan_hash"`
			Approved bool   `json:"approved"`
			Channel  string `json:"channel"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, badRequest(err)
		}
		c, err := s.ownChange(r)
		if err != nil {
			return nil, err
		}
		return s.local(r).ApproveInline(r.Context(), c.ID, in.PlanHash, in.Approved, in.Channel)
	})
	// Gateway authorization (agentgateway extAuthz, Envoy ext_authz, ContextForge plugin).
	// Any method: Envoy ext_authz forwards the client's own method, and MCP
	// clients also send GET (SSE stream) and DELETE (end session).
	mux.HandleFunc("/v1/authz/mcp", s.auth(s.authzMCP))
	mux.HandleFunc("/v1/authz/mcp/", s.auth(s.authzMCP)) // Envoy appends the original path
	mux.HandleFunc("POST /v1/authz/tool", s.auth(s.authzTool))
	// GitOps gate (Argo CD PreSync hooks, CI on GitOps repositories).
	h("POST /v1/gitops/gate", RolePropose, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Target   string `json:"target"`
			Payload  string `json:"payload"`
			Revision string `json:"revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, badRequest(err)
		}
		t, err := s.Sessions.Resolve(in.Target)
		if err != nil {
			return nil, err
		}
		return gitops.Gate(r.Context(), s.Engine, t, in.Payload, model.Principal{Kind: "human", Human: identity(caller(r))}, model.Provenance{Commit: in.Revision})
	})
	// Out-of-band approval by someone else (separation of duties is enforced by the engine).
	h("POST /v1/changes/{ref}/approve", RoleApprove, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			PlanHash string `json:"plan_hash"`
			Approved bool   `json:"approved"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, badRequest(err)
		}
		return s.Engine.Approve(r.Context(), r.PathValue("ref"), engine.ApproveRequest{PlanHash: in.PlanHash, Approver: identity(caller(r)),
			Channel: "executor-api", Approved: in.Approved, Reason: in.Reason})
	})
	return mux
}

func (s *Server) startSession(_ http.ResponseWriter, r *http.Request) (any, error) {
	var in backend.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, badRequest(err)
	}
	if in.Agent != "" && !strings.HasPrefix(in.Agent, "agent:") {
		in.Agent = "agent:" + in.Agent
	}
	return s.local(r).StartSession(r.Context(), in)
}

var errForbidden = errors.New("not your session")

func (s *Server) ownSession(r *http.Request, id string) (*model.Session, error) {
	sess, err := s.Sessions.Store.GetSession(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if sess.Principal.Human != identity(caller(r)) && !caller(r).has(RoleAdmin) {
		return nil, errForbidden
	}
	return sess, nil
}

func (s *Server) ownChange(r *http.Request) (*model.Change, error) {
	c, err := s.Engine.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, err
	}
	if c.Principal.Human != identity(caller(r)) && !caller(r).has(RoleAdmin) && !caller(r).has(RoleApprove) {
		return nil, errForbidden
	}
	return c, nil
}

type badReq struct{ error }

func badRequest(err error) error { return badReq{err} }

func statusOf(err error) int {
	var ce *adapter.ConflictError
	var br badReq
	switch {
	case errors.As(err, &ce):
		return http.StatusConflict
	case errors.As(err, &br):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, errForbidden):
		return http.StatusForbidden
	}
	return http.StatusUnprocessableEntity
}

// ErrorBody is the JSON error shape.
type ErrorBody struct {
	Error     string   `json:"error"`
	Conflicts []string `json:"conflicts,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, err error) {
	b := ErrorBody{Error: err.Error()}
	var ce *adapter.ConflictError
	if errors.As(err, &ce) {
		b.Conflicts, b.Reason = ce.Conflicts, ce.Reason
	}
	writeJSON(w, code, b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
