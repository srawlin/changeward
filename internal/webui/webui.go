// Package webui is the local, single-user web view of sessions and
// changes: the timeline with per-step diffs, decisions and audit events,
// plus approve/reject/undo actions. It is meant for localhost; actions
// require a per-process CSRF token embedded in the page.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/session"
	"github.com/srawlin/changegate/internal/store"
)

//go:embed page.html
var pageHTML string

// UI serves the web view.
type UI struct {
	Engine   *engine.Engine
	Sessions *session.Service
	Approver string // identity used for approvals from the UI
	token    string
	tmpl     *template.Template
}

// New builds the UI.
func New(eng *engine.Engine, svc *session.Service, approver string) *UI {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	funcs := template.FuncMap{
		"short": func(s string) string {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) > 90 {
				return s[:87] + "..."
			}
			return s
		},
		"applied": session.Applied,
		"pending": func(c *model.Change) bool { return c.State == model.AwaitingApproval },
		"stateClass": func(s model.State) string {
			switch s {
			case model.Verified, model.Applied:
				return "ok"
			case model.Rejected, model.Failed:
				return "bad"
			case model.AwaitingApproval:
				return "wait"
			}
			return "muted"
		},
		"add1": func(i int) int { return i + 1 },
	}
	return &UI{Engine: eng, Sessions: svc, Approver: approver, token: hex.EncodeToString(b), tmpl: template.Must(template.New("page").Funcs(funcs).Parse(pageHTML))}
}

type sessionView struct {
	Session *model.Session
	Changes []*model.Change
}

type pageData struct {
	Token    string
	Sessions []*model.Session
	Current  *sessionView
	Change   *model.Change
	Events   []*model.Event
	Flash    string
}

// Handler returns the HTTP handler.
func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.index)
	mux.HandleFunc("GET /sessions/{id}", u.session)
	mux.HandleFunc("GET /changes/{id}/view", u.change)
	mux.HandleFunc("POST /changes/{id}/{action}", u.action)
	return mux
}

func (u *UI) render(w http.ResponseWriter, d pageData) {
	d.Token = u.token
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := u.tmpl.Execute(w, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *UI) index(w http.ResponseWriter, r *http.Request) {
	ss, err := u.Sessions.Store.ListSessions(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := pageData{Sessions: ss, Flash: r.URL.Query().Get("flash")}
	if len(ss) > 0 {
		cs, _ := u.Sessions.Changes(r.Context(), ss[0].ID)
		d.Current = &sessionView{ss[0], cs}
	}
	u.render(w, d)
}

func (u *UI) session(w http.ResponseWriter, r *http.Request) {
	s, err := u.Sessions.Store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ss, _ := u.Sessions.Store.ListSessions(r.Context(), 50)
	cs, _ := u.Sessions.Changes(r.Context(), s.ID)
	u.render(w, pageData{Sessions: ss, Current: &sessionView{s, cs}, Flash: r.URL.Query().Get("flash")})
}

func (u *UI) change(w http.ResponseWriter, r *http.Request) {
	c, err := u.Engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	evs, _ := u.Engine.Store.ListEvents(r.Context(), store.EventFilter{ChangeID: c.ID})
	ss, _ := u.Sessions.Store.ListSessions(r.Context(), 50)
	u.render(w, pageData{Sessions: ss, Change: c, Events: evs, Flash: r.URL.Query().Get("flash")})
}

func (u *UI) action(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(u.token)) != 1 {
		http.Error(w, "invalid or missing token", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	c, err := u.Engine.Get(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var msg string
	switch r.PathValue("action") {
	case "approve", "reject":
		_, err = u.Engine.Approve(ctx, c.ID, engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: u.Approver, Channel: "web",
			Approved: r.PathValue("action") == "approve", Reason: "via the local web view"})
		msg = r.PathValue("action") + "d"
	case "undo":
		res := u.Sessions.UndoChain(ctx, c, r.FormValue("chain") == "1", u.Approver)
		err = res.Err
		msg = "undone"
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if err != nil {
		msg = "error: " + err.Error()
	}
	http.Redirect(w, r, "/changes/"+c.ID+"/view?flash="+urlQuery(msg), http.StatusSeeOther)
}

func urlQuery(s string) string {
	return strings.NewReplacer(" ", "+", "&", "%26", "#", "%23").Replace(s)
}
