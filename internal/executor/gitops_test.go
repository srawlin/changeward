package executor

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/srawlin/changeward/adapters/kubernetes"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/policy"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
	"github.com/srawlin/changeward/pkg/adapter"
)

func TestGitOpsGateOverExecutor(t *testing.T) {
	if exec.Command("kubectl", "--context=kind-changeward", "version", "--request-timeout=3s").Run() != nil {
		t.Skip("no kind-changeward cluster")
	}
	ns := fmt.Sprintf("cw-gate-%d", time.Now().UnixNano())
	_ = exec.Command("kubectl", "--context=kind-changeward", "create", "ns", ns).Run()
	defer func() {
		_ = exec.Command("kubectl", "--context=kind-changeward", "delete", "ns", ns, "--wait=false").Run()
	}()
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := store.OpenSQLite(filepath.Join(dir, "cw.db"))
	defer func() { _ = st.Close() }()
	pf, _ := policy.Parse([]byte("rules:\n  - {name: review, when: 'change.lane == \"ci\"', decide: require, channel: out-of-band}\n"))
	pol, _ := policy.New(pf)
	eng := &engine.Engine{Store: st, Adapters: engine.MapRegistry{"kubernetes": kubernetes.New()}, Policy: pol}
	tg := adapter.Target{Name: "cluster", Adapter: "kubernetes", Env: "prod", Config: map[string]string{"context": "kind-changeward", "namespace": ns}}
	svc := &session.Service{Engine: eng, Store: st, StateDir: dir, Resolve: func(string) (adapter.Target, error) { return tg, nil }}
	ciTok, ci := NewToken("argocd", []string{RolePropose})
	sreTok, sre := NewToken("sre", []string{RoleApprove})
	srv := httptest.NewServer((&Server{Engine: eng, Sessions: svc, Tokens: []Token{ci, sre}}).Handler())
	defer srv.Close()
	manifests := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: web}\ndata: {v: \"2\"}\n"
	c, err := NewClient(srv.URL, ciTok).GitOpsGate(ctx, "cluster", manifests, "abc123")
	if err != nil || c.State != model.AwaitingApproval || c.Provenance.Commit != "abc123" {
		t.Fatalf("first gate: %v %+v", err, c)
	}
	if _, err := NewClient(srv.URL, sreTok).Approve(ctx, c.ID, c.Plan.Hash, true, "reviewed"); err != nil {
		t.Fatal(err)
	}
	c2, err := NewClient(srv.URL, ciTok).GitOpsGate(ctx, "cluster", manifests, "abc123")
	if err != nil || c2.ID != c.ID || c2.State != model.Approved {
		t.Fatalf("retried gate: %v %s %s", err, c2.ID, c2.State)
	}
}
