package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/srawlin/changeward/pkg/adapter"
)

// Integration tests need Postgres. They use $CW_TEST_PG18 and $CW_TEST_PG16
// (admin DSNs), defaulting to the docker containers in scripts/test-pg.sh.
var servers = map[string]string{
	"pg18": envOr("CW_TEST_PG18", "postgres://postgres:pg@localhost:55418/postgres?sslmode=disable"),
	"pg16": envOr("CW_TEST_PG16", "postgres://postgres:pg@localhost:55416/postgres?sslmode=disable"),
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// eachServer runs fn against every reachable server.
func eachServer(t *testing.T, fn func(t *testing.T, admin string)) {
	ran := false
	for name, dsn := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := pgx.Connect(ctx, dsn)
		cancel()
		if err != nil {
			continue
		}
		_ = c.Close(context.Background())
		ran = true
		t.Run(name, func(t *testing.T) { fn(t, dsn) })
	}
	if !ran {
		t.Skip("no Postgres test server reachable (see scripts/test-pg.sh)")
	}
}

// freshDB creates an empty database and returns a target for it.
func freshDB(t *testing.T, admin string, env string, seed string) (adapter.Target, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cwtest_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(ctx)
	dsn, _ := WithDatabase(admin, name)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if seed != "" {
		if _, err := conn.Exec(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = conn.Close(ctx)
		if c, err := pgx.Connect(ctx, admin); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	return adapter.Target{Name: "app", Adapter: "postgres", Env: env, Config: map[string]string{"dsn": dsn}}, conn
}

const seedSQL = `
CREATE TABLE customers (id serial PRIMARY KEY, email text NOT NULL, name text);
CREATE TABLE orders (
  id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id int REFERENCES customers(id),
  total numeric(12,2) NOT NULL,
  status text NOT NULL DEFAULT 'new',
  tags text[],
  meta jsonb,
  blob bytea,
  placed_at timestamptz NOT NULL DEFAULT '2026-01-01T00:00:00Z',
  total_cents bigint GENERATED ALWAYS AS ((total * 100)::bigint) STORED,
  legacy_id int NOT NULL DEFAULT 0
);
CREATE TABLE orders_archive (id int PRIMARY KEY, total numeric, created_at date);
CREATE TABLE nopk (a int, b text);
INSERT INTO customers(email, name) SELECT 'c' || g || '@example.com', 'Customer ' || g FROM generate_series(1, 20) g;
INSERT INTO orders(customer_id, total, status, tags, meta, blob, legacy_id)
  SELECT (g % 20) + 1, g * 1.25, CASE WHEN g % 3 = 0 THEN 'shipped' ELSE 'new' END,
         ARRAY['t' || g, 'x'], jsonb_build_object('n', g, 'f', g / 3.0), decode('deadbeef', 'hex'), g
  FROM generate_series(1, 50) g;
INSERT INTO orders_archive SELECT g, g * 2, DATE '2020-01-01' + g FROM generate_series(1, 30) g;
INSERT INTO nopk VALUES (1, 'a'), (1, 'a'), (2, 'b');
`

func fp(t *testing.T, a *Adapter, tg adapter.Target) string {
	t.Helper()
	p, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "changeward.fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	return p.Facts["fingerprint"]
}

func mustPlan(t *testing.T, a *Adapter, tg adapter.Target, sql string) *adapter.Plan {
	t.Helper()
	p, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "sql", Payload: sql})
	if err != nil {
		t.Fatalf("plan %q: %v", sql, err)
	}
	if p.Hash != adapter.HashPlan(p) {
		t.Fatal("plan hash mismatch")
	}
	return p
}

func scalar[T any](t *testing.T, c *pgx.Conn, sql string) T {
	t.Helper()
	var v T
	if err := c.QueryRow(context.Background(), sql).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func snap(t *testing.T, a *Adapter, tg adapter.Target) *Snapshot {
	t.Helper()
	ctx := context.Background()
	pool, err := a.pool(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshotPool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// roundtrip plans, applies and undoes sql and checks the database returns
// to exactly its starting state.
func roundtrip(t *testing.T, a *Adapter, tg adapter.Target, sql string, wantRev adapter.Reversibility) (*adapter.Plan, *adapter.Receipt) {
	t.Helper()
	ctx := context.Background()
	s0 := snap(t, a, tg)
	p := mustPlan(t, a, tg, sql)
	if d := DiffSnapshots(s0, snap(t, a, tg)); len(d) > 0 {
		t.Fatalf("plan of %q changed the database: %v", sql, d)
	}
	if wantRev != "" && p.Reversibility != wantRev {
		t.Fatalf("%q: reversibility %s, want %s\n%s", sql, p.Reversibility, wantRev, p.Diff)
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatalf("apply %q: %v", sql, err)
	}
	if len(DiffSnapshots(s0, snap(t, a, tg))) == 0 {
		t.Fatalf("apply of %q did not change anything", sql)
	}
	v, err := a.Verify(ctx, tg, p, rc)
	if err != nil || !v.Passed {
		t.Fatalf("verify %q: %v %+v", sql, err, v)
	}
	if p.Reversibility == adapter.Reversible || p.Reversibility == adapter.Compensatable {
		if _, err := a.Undo(ctx, tg, p, rc); err != nil {
			t.Fatalf("undo %q: %v", sql, err)
		}
		if p.Reversibility == adapter.Reversible {
			if d := DiffSnapshots(s0, snap(t, a, tg)); len(d) > 0 {
				t.Fatalf("undo of %q did not restore the starting state: %v", sql, d)
			}
		}
	}
	return p, rc
}
