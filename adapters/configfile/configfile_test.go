package configfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srawlin/changegate/pkg/adapter"
)

func conf(body string) string {
	return fmt.Sprintf("server {\n  listen 80;\n  location /healthz { return %s; }\n  location / { return 200 \"%s\"; }\n}\n", "200 \"ok\"", body)
}

func nginxTarget(t *testing.T) adapter.Target {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	dir, _ := os.MkdirTemp("", "cg-nginx-")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "default.conf")
	_ = os.WriteFile(path, []byte(conf("v1")), 0o644)
	name := fmt.Sprintf("cg-nginx-%d", time.Now().UnixNano())
	port := "58" + fmt.Sprint(100+time.Now().UnixNano()%800)
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-p", port+":80", "-v", dir+":/etc/nginx/conf.d", "nginx:alpine").CombinedOutput(); err != nil {
		t.Skipf("cannot start nginx: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	base := "http://127.0.0.1:" + port
	for i := 0; i < 50; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			_ = r.Body.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return adapter.Target{Name: "edge", Adapter: "configfile", Env: "staging", Config: map[string]string{
		"path": path, "preset": "nginx", "test_cmd": "docker exec " + name + " nginx -t", "reload_cmd": "docker exec " + name + " nginx -s reload",
		"health_urls": base + "/healthz", "base": base}}
}

func get(t *testing.T, u string) string {
	t.Helper()
	var body string
	for i := 0; i < 20; i++ {
		r, err := http.Get(u)
		if err == nil {
			b, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			body = string(b)
		}
		if err == nil {
			return body
		}
		time.Sleep(200 * time.Millisecond)
	}
	return body
}

func waitFor(t *testing.T, u, want string) {
	t.Helper()
	for i := 0; i < 25; i++ {
		if get(t, u) == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never returned %q (got %q)", u, want, get(t, u))
}

func TestNginx(t *testing.T) {
	ctx := context.Background()
	tg := nginxTarget(t)
	a := New()
	base := tg.Config["base"]
	waitFor(t, base+"/", "v1")

	p, err := a.Plan(ctx, tg, adapter.Operation{Kind: "file", Payload: conf("v2")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Diff, `-   4    location / { return 200 "v1"; }`) || get(t, base+"/") != "v1" {
		t.Fatalf("plan:\n%s", p.Diff)
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, base+"/", "v2")
	if v, _ := a.Verify(ctx, tg, p, rc); !v.Passed {
		t.Fatalf("verify: %+v", v)
	}
	if _, err := a.Undo(ctx, tg, p, rc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, base+"/", "v1")

	// An invalid configuration never gets past the plan, and the live file is untouched.
	if _, err := a.Plan(ctx, tg, adapter.Operation{Kind: "file", Payload: "server { listen 80; bogus_directive on; }"}); err == nil {
		t.Fatal("invalid config accepted")
	}
	if b, _ := os.ReadFile(tg.Config["path"]); string(b) != conf("v1") {
		t.Fatal("validation left the candidate in place")
	}
	// Drift between plan and apply.
	p, _ = a.Plan(ctx, tg, adapter.Operation{Kind: "file", Payload: conf("v3")})
	_ = os.WriteFile(tg.Config["path"], []byte(conf("hand-edited")), 0o644)
	var ce *adapter.ConflictError
	if _, err := a.Apply(ctx, tg, p); !errors.As(err, &ce) {
		t.Fatalf("drift: %v", err)
	}
	// A reload that breaks health fails verification.
	_ = os.WriteFile(tg.Config["path"], []byte(conf("v1")), 0o644)
	broken := strings.Replace(conf("v4"), `return 200 "ok"`, "return 503", 1)
	p, _ = a.Plan(ctx, tg, adapter.Operation{Kind: "file", Payload: broken})
	rc, err = a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Verify(ctx, tg, p, rc); v.Passed {
		t.Fatal("health check should fail")
	}
	if _, err := a.Undo(ctx, tg, p, rc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, base+"/healthz", "ok")
}
