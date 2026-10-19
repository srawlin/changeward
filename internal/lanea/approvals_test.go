package lanea

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSatisfied(t *testing.T) {
	cases := []struct {
		approvers []string
		quorum    int
		by        []string
		ok        bool
	}{
		{[]string{"owner:codeowners"}, 1, []string{"bob"}, true},
		{[]string{"owner:codeowners"}, 1, []string{"alice"}, false}, // author's own approval
		{[]string{"owner:codeowners"}, 1, nil, false},
		{[]string{"user:carol"}, 1, []string{"bob"}, false},
		{[]string{"user:Carol"}, 1, []string{"carol"}, true},
		{[]string{"group:dba", "group:sre"}, 2, []string{"bob"}, false},
		{[]string{"group:dba", "group:sre"}, 2, []string{"bob", "dave", "alice"}, true},
	}
	for i, c := range cases {
		ok, _, why := Satisfied(c.approvers, c.quorum, c.by, "alice")
		if ok != c.ok {
			t.Errorf("case %d: %v (%s)", i, ok, why)
		}
	}
	_, platform, _ := Satisfied([]string{"owner:codeowners", "user:bob"}, 1, []string{"bob"}, "alice")
	if !reflect.DeepEqual(platform, []string{"owner:codeowners"}) {
		t.Fatalf("platform-enforced approvers: %v", platform)
	}
}

func TestGitHubApprovals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"login": "alice"}, "head": map[string]any{"sha": "head"}})
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			_, _ = w.Write([]byte(`[
			  {"user":{"login":"bob"},"state":"APPROVED","commit_id":"old"},
			  {"user":{"login":"carol"},"state":"APPROVED","commit_id":"head"},
			  {"user":{"login":"dave"},"state":"APPROVED","commit_id":"head"},
			  {"user":{"login":"dave"},"state":"CHANGES_REQUESTED","commit_id":"head"},
			  {"user":{"login":"erin"},"state":"COMMENTED","commit_id":"head"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	g := &GitHub{API: srv.URL, Repo: "o/r", PR: 7, Token: "t"}
	by, author, err := g.Approvals()
	if err != nil || author != "alice" || !reflect.DeepEqual(by, []string{"carol"}) {
		t.Fatalf("approvals %v author %q err %v (stale and superseded approvals must not count)", by, author, err)
	}
}
