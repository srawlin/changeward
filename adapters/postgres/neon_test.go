package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/srawlin/changegate/pkg/adapter"
)

// TestNeonLive exercises Neon checkpoints, restore and branch copies against
// a real Neon project. It needs NEON_API_KEY, NEON_PROJECT_ID, NEON_BRANCH_ID
// and NEON_DSN (the branch's connection string) and is skipped otherwise.
// It overwrites data on that branch: use a dedicated test project.
func TestNeonLive(t *testing.T) {
	dsn, project, branch := os.Getenv("NEON_DSN"), os.Getenv("NEON_PROJECT_ID"), os.Getenv("NEON_BRANCH_ID")
	if dsn == "" || project == "" || branch == "" || os.Getenv("NEON_API_KEY") == "" || testing.Short() {
		t.Skip("Neon test project not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Exec(ctx, `DROP TABLE IF EXISTS cg_neon; CREATE TABLE cg_neon (id int PRIMARY KEY, v text);
		INSERT INTO cg_neon SELECT g, 'v' || g FROM generate_series(1, 100) g;`)
	_ = c.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{StateDir: t.TempDir()})
	defer a.Close()
	tg := adapter.Target{Name: "neon", Adapter: "postgres", Env: "personal",
		Config: map[string]string{"dsn": dsn, "neon_project_id": project, "neon_branch_id": branch}}
	s0 := fp(t, a, tg)

	cp, err := a.Checkpoint(ctx, tg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.DeleteCheckpoint(context.Background(), tg, cp) }()
	p := mustPlan(t, a, tg, "DELETE FROM cg_neon; DROP TABLE cg_neon CASCADE")
	if _, err := a.Apply(ctx, tg, p); err != nil {
		t.Fatal(err)
	}
	if fp(t, a, tg) == s0 {
		t.Fatal("apply changed nothing")
	}
	rc, err := a.Restore(ctx, tg, cp)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rc.Output)
	var got string
	for i := 0; i < 30; i++ { // the endpoint restarts after a restore
		if got = fpRetry(ctx, a, tg); got != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if got != s0 {
		t.Fatal("Neon restore did not return to the checkpoint")
	}

	cpy, err := a.ProvisionCopy(ctx, tg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.ReleaseCopy(context.Background(), cpy); err != nil {
			t.Errorf("release copy: %v", err)
		}
	}()
	var cfp string
	for i := 0; i < 30; i++ { // a new branch endpoint may take a moment to accept connections
		if cfp = fpRetry(ctx, a, cpy.Target); cfp != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if cfp != s0 || cpy.Target.Env != "personal" {
		t.Fatalf("branch copy differs from source")
	}
}

func fpRetry(ctx context.Context, a *Adapter, t adapter.Target) string {
	a.dropPool(t.Config["dsn"])
	p, err := a.Plan(ctx, t, adapter.Operation{Kind: "changegate.fingerprint"})
	if err != nil {
		return ""
	}
	return p.Facts["fingerprint"]
}
