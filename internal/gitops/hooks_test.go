package gitops

import (
	"strings"
	"testing"
)

func TestStripHooks(t *testing.T) {
	in := `apiVersion: v1
kind: ConfigMap
metadata: {name: shop, namespace: shop}
data: {a: "1"}
---
apiVersion: batch/v1
kind: Job
metadata:
  generateName: changeward-gate-
  annotations: {argocd.argoproj.io/hook: PreSync}
---
---
apiVersion: v1
kind: Pod
metadata:
  name: migrate
  annotations: {helm.sh/hook: pre-upgrade}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
`
	out, n, err := StripHooks(in)
	if err != nil || n != 2 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if !strings.Contains(out, "kind: ConfigMap") || !strings.Contains(out, "kind: Deployment") || strings.Contains(out, "Job") || strings.Contains(out, "migrate") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
