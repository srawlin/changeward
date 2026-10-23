package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srawlin/changeward/conformance"
	"github.com/srawlin/changeward/pkg/adapter"
)

// Tests need MySQL 8: $CW_TEST_MYSQL (root DSN), default the docker container
// started by scripts/test-pg.sh (cw-mysql on :53306).
func adminDSN() string {
	if d := os.Getenv("CW_TEST_MYSQL"); d != "" {
		return d
	}
	return "root:pg@tcp(127.0.0.1:53306)/"
}

const fixture = `CREATE TABLE customers (id INT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(80) NOT NULL, name VARCHAR(80), score DOUBLE, born DATE);
CREATE TABLE orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, customer_id INT NOT NULL, total DECIMAL(10,2) NOT NULL, status VARCHAR(20) NOT NULL DEFAULT 'new',
  meta JSON, blob_col VARBINARY(32), placed_at DATETIME(3) NOT NULL DEFAULT '2026-01-01 00:00:00.000', cents BIGINT AS (total * 100) STORED,
  legacy_id INT NOT NULL DEFAULT 0, CONSTRAINT fk_c FOREIGN KEY (customer_id) REFERENCES customers(id));
CREATE TABLE archive (id INT PRIMARY KEY, note TEXT);`

func freshDB(t *testing.T) adapter.Target {
	t.Helper()
	ctx := context.Background()
	root, err := sql.Open("mysql", adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.PingContext(ctx); err != nil {
		t.Skip("no MySQL test server: " + err.Error())
	}
	name := fmt.Sprintf("cwtest_%d", time.Now().UnixNano())
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)
		_ = root.Close()
	})
	dsn := adminDSN() + name
	db, _ := sql.Open("mysql", dsn)
	defer func() { _ = db.Close() }()
	for _, stmt := range strings.Split(fixture, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	for i := 1; i <= 30; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO customers(email, name, score, born) VALUES (?, ?, ?, ?)", fmt.Sprintf("c%d@x.io", i), nullIf(i%5 == 0, fmt.Sprintf("C %d", i)), float64(i)/7, "1990-01-02"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO orders(customer_id, total, status, meta, blob_col, legacy_id) VALUES (?, ?, ?, JSON_OBJECT('n', ?, 'f', ?), UNHEX(MD5(?)), ?)`,
			i, float64(i)*1.25, []string{"new", "paid"}[i%2], i, float64(i)/3, i, i); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO archive VALUES (?, ?)", i, fmt.Sprintf("note %d 'quoted' é", i)); err != nil {
			t.Fatal(err)
		}
	}
	return adapter.Target{Name: "app", Adapter: "mysql", Env: "personal", Config: map[string]string{"dsn": dsn}}
}

func nullIf(b bool, s string) any {
	if b {
		return nil
	}
	return s
}

func fp(t *testing.T, a *Adapter, tg adapter.Target) string {
	p, err := a.Plan(context.Background(), tg, adapter.Operation{Kind: "changeward.fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	return p.Facts["fingerprint"]
}

func roundtrip(t *testing.T, a *Adapter, tg adapter.Target, sqlText string, want adapter.Reversibility) {
	t.Helper()
	ctx := context.Background()
	s0 := fp(t, a, tg)
	p, err := a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: sqlText})
	if err != nil {
		t.Fatalf("plan %q: %v", sqlText, err)
	}
	if fp(t, a, tg) != s0 {
		t.Fatalf("plan of %q changed the database", sqlText)
	}
	if p.Reversibility != want {
		t.Fatalf("%q: %s, want %s\n%s", sqlText, p.Reversibility, want, p.Diff)
	}
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatalf("apply %q: %v", sqlText, err)
	}
	if want == adapter.Reversible {
		if _, err := a.Undo(ctx, tg, p, rc); err != nil {
			t.Fatalf("undo %q: %v", sqlText, err)
		}
		if fp(t, a, tg) != s0 {
			t.Fatalf("undo of %q did not restore the state", sqlText)
		}
	}
}

func TestMySQLRoundtrips(t *testing.T) {
	tg := freshDB(t)
	a := New()
	defer a.Close()
	for _, s := range []string{
		"UPDATE orders SET status = 'cancelled', meta = JSON_SET(meta, '$.x', 1) WHERE id % 3 = 0",
		"UPDATE customers SET score = score * 2, name = NULL WHERE id < 5",
		"DELETE FROM archive WHERE id > 20",
		"DELETE FROM orders WHERE status = 'paid' ORDER BY id DESC LIMIT 3",
		"INSERT INTO customers(email, name) VALUES ('n1@x.io', 'N1'), ('n2@x.io', NULL)",
		"INSERT INTO archive(id, note) VALUES (100, 'a'), (101, 'b')",
		"UPDATE orders o SET o.total = o.total + 1 WHERE o.customer_id = 2",
		"DROP TABLE archive",
		"TRUNCATE archive",
		"ALTER TABLE orders DROP COLUMN legacy_id",
		"ALTER TABLE orders ADD COLUMN region VARCHAR(8) DEFAULT 'eu'",
		"CREATE INDEX orders_status ON orders(status)",
		"CREATE TABLE regions (code CHAR(2) PRIMARY KEY)",
		"RENAME TABLE archive TO archive2",
	} {
		roundtrip(t, a, tg, s, adapter.Reversible)
	}
	roundtrip(t, a, tg, "INSERT INTO archive SELECT id + 1000, note FROM archive", adapter.CheckpointOnly)
	roundtrip(t, a, tg, "DELETE FROM customers WHERE id NOT IN (SELECT customer_id FROM orders) AND id > 100", adapter.Reversible)
}

func TestMySQLGuards(t *testing.T) {
	ctx := context.Background()
	tg := freshDB(t)
	a := New()
	defer a.Close()
	db, _ := sql.Open("mysql", tg.Config["dsn"])
	defer func() { _ = db.Close() }()
	p, err := a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: "UPDATE orders SET status = 'held' WHERE id <= 5"})
	if err != nil || p.BlastRows != 5 || !strings.Contains(p.Diff, `status: "new" → "held"`) {
		t.Fatalf("plan: %v\n%s", err, p.Diff)
	}
	_, _ = db.ExecContext(ctx, "UPDATE orders SET total = 1 WHERE id = 2")
	var ce *adapter.ConflictError
	if _, err := a.Apply(ctx, tg, p); !errors.As(err, &ce) {
		t.Fatalf("drift: %v", err)
	}
	p, _ = a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: "UPDATE orders SET status = 'held' WHERE id <= 5"})
	rc, err := a.Apply(ctx, tg, p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.ExecContext(ctx, "UPDATE orders SET status = 'x' WHERE id = 3")
	if _, err := a.Undo(ctx, tg, p, rc); !errors.As(err, &ce) {
		t.Fatalf("stale undo: %v", err)
	}
	var held int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE status = 'held'").Scan(&held)
	if held != 4 {
		t.Fatalf("refused undo changed rows: %d", held)
	}
	if _, err := a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: "DROP TABLE customers"}); err == nil {
		t.Fatal("drop of a referenced table should fail at plan time")
	}
	if p, _ := a.Plan(ctx, tg, adapter.Operation{Kind: "sql", Payload: "DELETE FROM customers WHERE id = 1"}); p == nil {
		t.Log("FK restrict surfaces as a dry-run error")
	}
}

func TestMySQLConformance(t *testing.T) {
	a := New()
	defer a.Close()
	rep := conformance.Run(context.Background(), conformance.Driver{Adapter: a, Seed: 7, N: 25,
		NewTarget: func(context.Context) (adapter.Target, func(), error) { return freshDB(t), func() {}, nil },
		Operations: func(_ context.Context, _ adapter.Target, seed int64, n int) ([]adapter.Operation, error) {
			rng := rand.New(rand.NewSource(seed))
			var ops []adapter.Operation
			for i := 0; len(ops) < n; i++ {
				var q string
				switch rng.Intn(6) {
				case 0:
					q = fmt.Sprintf("UPDATE orders SET status = 's%d', total = total + %d WHERE id %% %d = %d", i, rng.Intn(9)+1, rng.Intn(4)+2, rng.Intn(2))
				case 1:
					q = fmt.Sprintf("DELETE FROM archive WHERE id %% %d = %d", rng.Intn(5)+3, rng.Intn(3))
				case 2:
					q = fmt.Sprintf("INSERT INTO archive(id, note) VALUES (%d, 'gen %d')", 1000+i, i)
				case 3:
					q = fmt.Sprintf("UPDATE customers SET name = 'n%d', score = NULL WHERE id = %d", i, rng.Intn(30)+1)
				case 4:
					q = fmt.Sprintf("INSERT INTO customers(email) VALUES ('g%d@x.io')", i)
				case 5:
					q = fmt.Sprintf("UPDATE orders SET meta = JSON_SET(meta, '$.k%d', %d) WHERE id < %d", i, i, rng.Intn(20)+1)
				}
				ops = append(ops, adapter.Operation{Kind: "sql", Payload: q})
			}
			return ops, nil
		}})
	t.Log(rep.String())
	if !rep.Passed {
		t.Fatal(rep.String())
	}
}
