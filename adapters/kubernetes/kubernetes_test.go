package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srawlin/changegate/pkg/adapter"
)

// Tests need a cluster: $CG_TEST_KUBE_CONTEXT (default kind-changegate,
// created with `kind create cluster --name changegate`).
func cluster(t *testing.T) adapter.Target {
	t.Helper()
	kctx := os.Getenv("CG_TEST_KUBE_CONTEXT")
	if kctx == "" {
		kctx = "kind-changegate"
	}
	base := adapter.Target{Name: "k8s", Adapter: "kubernetes", Env: "personal", Config: map[string]string{"context": kctx}}
	ctx := context.Background()
	if _, err := kubectl(ctx, base, nil, "version", "--request-timeout=3s"); err != nil {
		t.Skip("no Kubernetes test cluster: " + err.Error())
	}
	ns := fmt.Sprintf("cg-test-%d", time.Now().UnixNano())
	if _, err := kubectl(ctx, base, nil, "create", "namespace", ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = kubectl(context.Background(), base, nil, "delete", "namespace", ns, "--wait=false") })
	base.Config["namespace"] = ns
	base.Config["rollout_timeout"] = "90s"
	return base
}

const app = `apiVersion: v1
kind: ConfigMap
metadata: {name: web-config}
data: {greeting: hello}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, labels: {app: web}}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
          imagePullPolicy: IfNotPresent
`

func plan(t *testing.T, a *Adapter, tg adapter.Target, payload string) *adapter.Plan {
	t.Helper()
	p, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "k8s", Payload: payload})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Hash != adapter.HashPlan(p) {
		t.Fatal("hash")
	}
	return p
}

func live(t *testing.T, tg adapter.Target, kind, name string) Obj {
	o, err := getLive(context.Background(), tg, Ref{Kind: kind, Name: name, Namespace: tg.Config["namespace"]})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestKubernetesLifecycle(t *testing.T) {
	ctx := context.Background()
	tg := cluster(t)
	a := New()
	p := plan(t, a, tg, app)
	if p.BlastRows != 2 || !strings.Contains(p.Diff, "+ "+tg.Config["namespace"]+"/Deployment/web (create)") || p.Reversibility != adapter.Reversible {
		t.Fatalf("create plan:\n%s", p.Diff)
	}
	if live(t, tg, "ConfigMap", "web-config") != nil {
		t.Fatal("dry run created the object")
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := a.Verify(ctx, tg, p, rc); err != nil || !v.Passed {
		t.Fatalf("verify: %v %+v", err, v)
	}

	// Update: change data and replicas.
	before := strip(live(t, tg, "ConfigMap", "web-config"))
	upd := strings.Replace(strings.Replace(app, "greeting: hello", "greeting: bonjour", 1), "replicas: 1", "replicas: 2", 1)
	p2 := plan(t, a, tg, upd)
	if !strings.Contains(p2.Diff, `data.greeting: "hello" → "bonjour"`) || !strings.Contains(p2.Diff, "spec.replicas: 1 → 2") {
		t.Fatalf("update diff:\n%s", p2.Diff)
	}
	rc2, err := a.Apply(ctx, tg, p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Undo(ctx, tg, p2, rc2); err != nil {
		t.Fatal(err)
	}
	if after := strip(live(t, tg, "ConfigMap", "web-config")); !reflect.DeepEqual(before, after) {
		t.Fatalf("undo did not restore the ConfigMap:\n%v\n%v", before, after)
	}

	// Drift between plan and apply is refused.
	p3 := plan(t, a, tg, upd)
	if _, err := kubectl(ctx, tg, nil, "patch", "configmap", "web-config", "-n", tg.Config["namespace"], "-p", `{"data":{"x":"y"}}`); err != nil {
		t.Fatal(err)
	}
	var ce *adapter.ConflictError
	if _, err := a.Apply(ctx, tg, p3); !errors.As(err, &ce) {
		t.Fatalf("drift: %v", err)
	}
	// Changes after apply make undo refuse.
	p4 := plan(t, a, tg, upd)
	rc4, err := a.Apply(ctx, tg, p4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kubectl(ctx, tg, nil, "patch", "configmap", "web-config", "-n", tg.Config["namespace"], "-p", `{"data":{"z":"1"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Undo(ctx, tg, p4, rc4); !errors.As(err, &ce) {
		t.Fatalf("stale undo: %v", err)
	}

	// Delete and scale commands.
	pd := plan(t, a, tg, "delete deploy/web cm/web-config")
	if !pd.Destructive || pd.Reversibility != adapter.Reversible {
		t.Fatalf("delete plan: %+v", pd)
	}
	rcd, err := a.Apply(ctx, tg, pd)
	if err != nil {
		t.Fatal(err)
	}
	if live(t, tg, "Deployment", "web") != nil {
		t.Fatal("not deleted")
	}
	if _, err := a.Undo(ctx, tg, pd, rcd); err != nil {
		t.Fatal(err)
	}
	if live(t, tg, "Deployment", "web") == nil || live(t, tg, "ConfigMap", "web-config") == nil {
		t.Fatal("undo of delete did not re-create the objects")
	}
	ps := plan(t, a, tg, "scale deployment/web --replicas=3")
	if !strings.Contains(ps.Diff, "spec.replicas: 2 → 3") {
		t.Fatalf("scale:\n%s", ps.Diff)
	}
}

func TestKubernetesRisk(t *testing.T) {
	tg := cluster(t)
	a := New()
	ctx := context.Background()
	pvc := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Mi}}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: owned
  annotations: {argocd.argoproj.io/tracking-id: "shop:/ConfigMap:shop/owned"}
data: {a: "1"}
`
	if _, err := a.Apply(ctx, tg, plan(t, a, tg, pvc)); err != nil {
		t.Fatal(err)
	}
	if p := plan(t, a, tg, "delete pvc/data"); p.Reversibility != adapter.Irreversible || len(p.Warnings) == 0 {
		t.Fatalf("pvc delete: %s %v", p.Reversibility, p.Warnings)
	}
	p := plan(t, a, tg, strings.Replace(pvc, `a: "1"`, `a: "2"`, 1))
	if p.Facts["gitops_owner"] != "argocd" || !strings.Contains(p.Diff, "managed by argocd") {
		t.Fatalf("gitops: %v\n%s", p.Facts, p.Diff)
	}
}
