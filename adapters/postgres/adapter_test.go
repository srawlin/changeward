package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/srawlin/changegate/pkg/adapter"
)

func newA(t *testing.T) *Adapter {
	a := New(Options{StateDir: t.TempDir()})
	t.Cleanup(a.Close)
	return a
}

func TestDMLRoundtrips(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		a := newA(t)
		tg, _ := freshDB(t, admin, "personal", seedSQL)
		pg18 := strings.Contains(admin, "55418") || strings.Contains(admin, "18")
		for _, sql := range []string{
			"UPDATE orders SET status = 'cancelled', tags = tags || 'c'::text, meta = meta || '{\"x\":1}' WHERE id % 4 = 0",
			"UPDATE orders SET total = total * 1.1 WHERE customer_id = 3",
			"DELETE FROM orders_archive WHERE created_at < DATE '2020-01-15'",
			"DELETE FROM orders WHERE status = 'shipped' AND id > 40",
			"INSERT INTO customers(email, name) VALUES ('new@example.com', 'New'), ('n2@example.com', NULL)",
			"INSERT INTO orders(customer_id, total) SELECT id, 9.99 FROM customers WHERE id < 4",
		} {
			roundtrip(t, a, tg, sql, adapter.Reversible)
		}
		if p := mustPlan(t, a, tg, "UPDATE orders SET status = 'x' WHERE false"); p.BlastRows != 0 || p.Reversibility != adapter.Reversible {
			t.Fatalf("no-op plan: %+v", p)
		}
		// Multi-statement change, including a table created in the same change.
		roundtrip(t, a, tg, `CREATE TABLE regions (code text PRIMARY KEY, name text);
			INSERT INTO regions VALUES ('eu', 'Europe'), ('us', 'United States');
			ALTER TABLE orders ADD COLUMN region text REFERENCES regions(code);
			UPDATE orders SET region = CASE WHEN id % 2 = 0 THEN 'eu' ELSE 'us' END;
			CREATE INDEX orders_region_idx ON orders(region);`, adapter.Reversible)
		if pg18 {
			roundtrip(t, a, tg, "INSERT INTO orders_archive VALUES (1, 0, NULL) ON CONFLICT (id) DO UPDATE SET total = -1", adapter.Reversible)
			roundtrip(t, a, tg, "UPDATE orders o SET status = c.name FROM customers c WHERE c.id = o.customer_id AND c.id = 2", adapter.Reversible)
			roundtrip(t, a, tg, "DELETE FROM nopk WHERE a = 2", adapter.Reversible)
		}
	})
}

func TestDMLPlanDetails(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		a := newA(t)
		tg, conn := freshDB(t, admin, "personal", seedSQL)
		tg.Config["sensitive.0"] = "customers.email"
		p := mustPlan(t, a, tg, "DELETE FROM customers WHERE id > 15 AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = customers.id)")
		if p.BlastRows != 0 && !strings.Contains(p.Diff, "•••") {
			t.Fatalf("sensitive column not redacted:\n%s", p.Diff)
		}
		p = mustPlan(t, a, tg, "UPDATE orders SET status = 'held' WHERE id <= 5")
		if p.BlastRows != 5 || p.RehearsalTier != "live-dry-run" || !strings.Contains(p.Diff, `status: "new" → "held"`) {
			t.Fatalf("plan:\n%s", p.Diff)
		}
		if n := scalar[int64](t, conn, "SELECT count(*) FROM orders WHERE status = 'held'"); n != 0 {
			t.Fatal("dry run leaked")
		}
		// Shared environments show keys, never values.
		tg.Env = "prod"
		p = mustPlan(t, a, tg, "UPDATE orders SET status = 'held' WHERE id <= 5")
		if strings.Contains(p.Diff, "held") && strings.Contains(p.Diff, `"new"`) {
			t.Fatalf("prod plan shows values:\n%s", p.Diff)
		}
		tg.Env = "personal"
		// Reads are not changes.
		if _, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "sql", Payload: "SELECT 1"}); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("read plan: %v", err)
		}
		if _, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "sql", Payload: "BEGIN; DELETE FROM orders; COMMIT"}); err == nil {
			t.Fatal("transaction control should be rejected")
		}
		// A failing statement reports a dry-run error and leaves no trace.
		if _, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "sql", Payload: "ALTER TABLE orders ALTER COLUMN status SET NOT NULL; UPDATE orders SET status = NULL WHERE id = 1"}); err == nil ||
			!strings.Contains(err.Error(), "failed") {
			t.Fatalf("expected dry-run failure, got %v", err)
		}
	})
}

func TestSideEffectsNeverDryRun(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		ctx := context.Background()
		a := newA(t)
		tg, conn := freshDB(t, admin, "personal", seedSQL)
		// A second session that pg_terminate_backend would kill if executed.
		victim, err := pgx.Connect(ctx, tg.Config["dsn"])
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = victim.Close(ctx) }()
		var pid int
		if err := victim.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		for _, sql := range []string{
			fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid),
			fmt.Sprintf("UPDATE orders SET status = 'x' WHERE pg_terminate_backend(%d)", pid),
			"DO $$ BEGIN DELETE FROM orders; END $$",
		} {
			p := mustPlan(t, a, tg, sql)
			if p.Reversibility != adapter.Irreversible || !p.Destructive {
				t.Fatalf("%s: %s", sql, p.Reversibility)
			}
		}
		if err := victim.Ping(ctx); err != nil {
			t.Fatalf("dry run executed a side-effect function: %v", err)
		}
		if n := scalar[int64](t, conn, "SELECT count(*) FROM orders"); n != 50 {
			t.Fatalf("DO block ran during planning: %d rows", n)
		}
	})
}

func TestDriftAndStaleUndo(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		ctx := context.Background()
		a := newA(t)
		tg, conn := freshDB(t, admin, "personal", seedSQL)
		p := mustPlan(t, a, tg, "UPDATE orders SET status = 'held' WHERE status = 'new' AND id < 10")
		if _, err := conn.Exec(ctx, "UPDATE orders SET total = 1 WHERE id = 1"); err != nil {
			t.Fatal(err)
		}
		_, err := a.Apply(ctx, tg, p)
		var ce *adapter.ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("expected drift conflict, got %v", err)
		}
		if n := scalar[int64](t, conn, "SELECT count(*) FROM orders WHERE status = 'held'"); n != 0 {
			t.Fatal("drifted apply changed rows")
		}
		p = mustPlan(t, a, tg, "UPDATE orders SET status = 'held' WHERE status = 'new' AND id < 10")
		rc, err := a.Apply(ctx, tg, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "UPDATE orders SET status = 'shipped' WHERE id = 2"); err != nil {
			t.Fatal(err)
		}
		_, err = a.Undo(ctx, tg, p, rc)
		if !errors.As(err, &ce) {
			t.Fatalf("expected stale-undo conflict, got %v", err)
		}
		if n := scalar[int64](t, conn, "SELECT count(*) FROM orders WHERE status = 'held'"); n == 0 {
			t.Fatal("refused undo changed rows")
		}
	})
}

func TestSoftDropAndDDL(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		ctx := context.Background()
		a := newA(t)
		tg, conn := freshDB(t, admin, "personal", seedSQL)

		p, _ := roundtrip(t, a, tg, "DROP TABLE orders_archive", adapter.Reversible)
		if !strings.Contains(p.Diff, "soft-drop") || p.BlastRows != 30 {
			t.Fatalf("drop table plan:\n%s", p.Diff)
		}
		// Applied soft-drop hides the table from the app.
		p = mustPlan(t, a, tg, "DROP TABLE orders_archive")
		rc, err := a.Apply(ctx, tg, p)
		if err != nil {
			t.Fatal(err)
		}
		if scalar[bool](t, conn, "SELECT to_regclass('public.orders_archive') IS NOT NULL") {
			t.Fatal("table still visible after soft-drop")
		}
		if _, err := a.Undo(ctx, tg, p, rc); err != nil {
			t.Fatal(err)
		}
		if n := scalar[int64](t, conn, "SELECT count(*) FROM orders_archive"); n != 30 {
			t.Fatalf("restored rows = %d", n)
		}

		// Soft-dropped NOT NULL column: inserts that omit it keep working.
		p = mustPlan(t, a, tg, "ALTER TABLE orders DROP COLUMN legacy_id")
		rc, err = a.Apply(ctx, tg, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO orders(customer_id, total) VALUES (1, 1)"); err != nil {
			t.Fatalf("insert after soft drop column: %v", err)
		}
		if _, err := conn.Exec(ctx, "DELETE FROM orders WHERE total = 1 AND customer_id = 1 AND id > 50"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Undo(ctx, tg, p, rc); err != nil {
			t.Fatal(err)
		}

		for _, c := range []struct {
			sql string
			rev adapter.Reversibility
		}{
			{"ALTER TABLE orders ADD COLUMN note text DEFAULT 'n/a'", adapter.Reversible},
			{"ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'pending'", adapter.Reversible},
			{"ALTER TABLE orders ALTER COLUMN status DROP DEFAULT", adapter.Reversible},
			{"ALTER TABLE customers ALTER COLUMN name SET NOT NULL", adapter.Reversible},
			{"ALTER TABLE orders RENAME COLUMN status TO state", adapter.Reversible},
			{"ALTER TABLE orders_archive RENAME TO archive", adapter.Reversible},
			{"CREATE INDEX orders_status_idx ON orders(status)", adapter.Reversible},
			{"CREATE UNIQUE INDEX ON customers(email)", adapter.Reversible},
			{"ALTER TABLE orders ADD CONSTRAINT total_positive CHECK (total > 0)", adapter.Reversible},
			{"ALTER TABLE orders DROP CONSTRAINT orders_customer_id_fkey", adapter.Reversible},
			{"CREATE VIEW big_orders AS SELECT * FROM orders WHERE total > 30", adapter.Reversible},
			{"CREATE SCHEMA reporting; CREATE TABLE reporting.daily (d date PRIMARY KEY, n int)", adapter.Reversible},
			{"CREATE TYPE mood AS ENUM ('ok', 'sad')", adapter.Reversible},
			{"TRUNCATE orders_archive", adapter.Reversible},
			{"CREATE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 1'", adapter.Reversible},
			{"ALTER TABLE orders ALTER COLUMN legacy_id TYPE bigint", adapter.Compensatable},
		} {
			roundtrip(t, a, tg, c.sql, c.rev)
		}
		// Drop index: undo re-creates it.
		if _, err := conn.Exec(ctx, "CREATE INDEX cust_name_idx ON customers(name)"); err != nil {
			t.Fatal(err)
		}
		roundtrip(t, a, tg, "DROP INDEX cust_name_idx", adapter.Reversible)

		// CASCADE cannot be soft-dropped.
		p = mustPlan(t, a, tg, "DROP TABLE customers CASCADE")
		if p.Reversibility != adapter.CheckpointOnly {
			t.Fatalf("cascade reversibility %s", p.Reversibility)
		}
		// A drop that would fail (FK dependents) fails in the dry run.
		if _, err := a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: "DROP TABLE customers"}); err == nil {
			t.Fatal("expected dependent-object error from the dry run")
		}
	})
}

func TestLocksAndRewrites(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		a := newA(t)
		tg, _ := freshDB(t, admin, "personal", seedSQL)
		p := mustPlan(t, a, tg, "ALTER TABLE orders ALTER COLUMN legacy_id TYPE bigint")
		if p.Facts["max_lock"] != "AccessExclusiveLock" || p.Facts["rewrite"] != "true" {
			t.Fatalf("facts %v\n%s", p.Facts, p.Diff)
		}
		p = mustPlan(t, a, tg, "CREATE INDEX ON orders(status)")
		if !strings.Contains(p.Facts["locks"], "public.orders=ShareLock") {
			t.Fatalf("facts %v", p.Facts)
		}
		p = mustPlan(t, a, tg, "CREATE INDEX CONCURRENTLY orders_c_idx ON orders(customer_id)")
		if p.RehearsalTier != "static" {
			t.Fatalf("concurrent tier %s", p.RehearsalTier)
		}
		roundtrip(t, a, tg, "CREATE INDEX CONCURRENTLY orders_c_idx ON orders(customer_id)", adapter.Reversible)
	})
}

func TestCascadesAndTriggers(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		a := newA(t)
		tg, _ := freshDB(t, admin, "personal", `
			CREATE TABLE parent (id int PRIMARY KEY);
			CREATE TABLE child (id int PRIMARY KEY, parent_id int REFERENCES parent(id) ON DELETE CASCADE);
			CREATE TABLE audit (msg text);
			CREATE FUNCTION log_it() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO audit VALUES (TG_OP); RETURN NEW; END $$;
			CREATE TABLE watched (id int PRIMARY KEY, v int);
			CREATE TRIGGER watched_log AFTER UPDATE ON watched FOR EACH ROW EXECUTE FUNCTION log_it();
			INSERT INTO parent VALUES (1), (2); INSERT INTO child VALUES (10, 1), (11, 1), (20, 2);
			INSERT INTO watched VALUES (1, 1);`)
		p := mustPlan(t, a, tg, "DELETE FROM parent WHERE id = 1")
		if p.Reversibility != adapter.CheckpointOnly || !strings.Contains(strings.Join(p.Warnings, " "), "cascades") {
			t.Fatalf("cascade: %s %v", p.Reversibility, p.Warnings)
		}
		p = mustPlan(t, a, tg, "UPDATE watched SET v = 2")
		if p.Reversibility != adapter.Compensatable {
			t.Fatalf("trigger: %s", p.Reversibility)
		}
	})
}

func TestQuery(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		a := newA(t)
		tg, _ := freshDB(t, admin, "personal", seedSQL)
		tg.Config["sensitive.0"] = "*.email"
		r, err := a.Query(context.Background(), tg, "SELECT id, email, name FROM customers ORDER BY id", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Rows) != 5 || !r.Truncated || r.Rows[0][1] != "•••" || r.Rows[0][2] != "Customer 1" {
			t.Fatalf("query: %+v", r)
		}
		if _, err := a.Query(context.Background(), tg, "DELETE FROM customers", 5); err == nil {
			t.Fatal("query must refuse writes")
		}
		// Functions cannot write through the read path.
		_, err = a.Query(context.Background(), tg, "SELECT setval('customers_id_seq', 100)", 5)
		if err == nil {
			t.Fatal("expected read-only refusal")
		}
	})
}

func TestCheckpointAndCopy(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		ctx := context.Background()
		a := newA(t)
		tg, conn := freshDB(t, admin, "personal", seedSQL)
		_ = conn.Close(ctx) // template operations need exclusive access
		s0 := fp(t, a, tg)
		cp, err := a.Checkpoint(ctx, tg, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.DeleteCheckpoint(ctx, tg, cp) }()
		p := mustPlan(t, a, tg, "DELETE FROM orders; DROP TABLE orders_archive CASCADE")
		if _, err := a.Apply(ctx, tg, p); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Restore(ctx, tg, cp); err != nil {
			t.Fatal(err)
		}
		if fp(t, a, tg) != s0 {
			t.Fatal("restore did not return to the checkpoint")
		}
		cpy, err := a.ProvisionCopy(ctx, tg, "")
		if err != nil {
			t.Fatal(err)
		}
		if fp(t, a, cpy.Target) != s0 || cpy.Target.Env != "personal" {
			t.Fatal("copy differs from source")
		}
		if err := a.ReleaseCopy(ctx, cpy); err != nil {
			t.Fatal(err)
		}
	})
}
