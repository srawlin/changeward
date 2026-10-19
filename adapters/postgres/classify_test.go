package postgres

import (
	"strings"
	"testing"

	"github.com/srawlin/changegate/pkg/adapter"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		sql     string
		kind    string
		access  adapter.Access
		flags   []string
		objects []string
	}{
		{"SELECT * FROM orders o JOIN public.users u ON u.id = o.user_id", "SELECT", adapter.Read, nil, []string{"orders", "public.users"}},
		{"WITH x AS (SELECT 1) SELECT * FROM x", "SELECT", adapter.Read, nil, nil},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", "SELECT", adapter.Destructive, []string{FlagWithClause}, []string{"t"}},
		{"SELECT pg_terminate_backend(123)", "SELECT", adapter.Write, []string{FlagSideEffectFunction}, nil},
		{"DELETE FROM executives", "DELETE", adapter.Destructive, []string{FlagDeleteWithoutWhere}, []string{"executives"}},
		{"UPDATE orders SET region = 'eu' WHERE id > 10", "UPDATE", adapter.Write, nil, []string{"orders"}},
		{"UPDATE orders SET region = 'eu'", "UPDATE", adapter.Write, []string{FlagUpdateWithoutWhere}, nil},
		{"INSERT INTO t(a) VALUES (1) ON CONFLICT (a) DO UPDATE SET a = 2", "INSERT", adapter.Write, []string{FlagUpsert}, nil},
		{"DROP TABLE IF EXISTS a.b CASCADE", "DROP TABLE", adapter.Destructive, []string{FlagCascade, FlagDropTable}, []string{"a.b"}},
		{"TRUNCATE orders", "TRUNCATE", adapter.Destructive, []string{FlagTruncate}, []string{"orders"}},
		{"CREATE INDEX orders_region_idx ON orders(region)", "CREATE INDEX", adapter.Write, []string{FlagNonConcurrentIndex}, []string{"orders", "orders_region_idx"}},
		{"CREATE INDEX CONCURRENTLY i ON orders(region)", "CREATE INDEX", adapter.Write, []string{FlagConcurrent}, nil},
		{"ALTER TABLE orders ADD COLUMN region text", "ALTER TABLE", adapter.Write, nil, []string{"orders"}},
		{"ALTER TABLE orders ADD COLUMN id2 uuid NOT NULL DEFAULT gen_random_uuid()", "ALTER TABLE", adapter.Write, []string{FlagVolatileDefault}, nil},
		{"ALTER TABLE orders ADD COLUMN x int NOT NULL", "ALTER TABLE", adapter.Write, []string{FlagAddColumnNotNull}, nil},
		{"ALTER TABLE orders DROP COLUMN legacy_id", "ALTER TABLE", adapter.Destructive, []string{FlagDropColumn}, nil},
		{"ALTER TABLE orders ALTER COLUMN total TYPE numeric(12,2)", "ALTER TABLE", adapter.Write, []string{FlagAlterColumnType, FlagHeavyLock}, nil},
		{"ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id)", "ALTER TABLE", adapter.Write, []string{FlagFKWithoutNotValid}, nil},
		{"ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id) NOT VALID", "ALTER TABLE", adapter.Write, nil, nil},
		{"ALTER TABLE orders RENAME COLUMN a TO b", "RENAME COLUMN", adapter.Write, []string{FlagRenameColumn}, []string{"orders"}},
		{"GRANT SELECT ON orders TO bob", "GRANT", adapter.Write, []string{FlagPrivilege}, []string{"orders"}},
		{"BEGIN", "TRANSACTION", adapter.Write, []string{FlagTxControl}, nil},
		{"DO $$ BEGIN PERFORM 1; END $$", "DO", adapter.Destructive, []string{FlagCodeBlock}, nil},
		{"SET session_replication_role = replica", "SET", adapter.Write, []string{FlagDisablesTriggers, FlagSessionSetting}, nil},
		{"EXPLAIN ANALYZE DELETE FROM t", "EXPLAIN", adapter.Destructive, []string{FlagDeleteWithoutWhere}, nil},
		{"EXPLAIN DELETE FROM t", "EXPLAIN", adapter.Read, nil, nil},
		{"COPY t TO STDOUT", "COPY", adapter.Read, nil, nil},
	}
	for _, c := range cases {
		stmts, err := Parse(c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		s := stmts[0]
		if s.Kind != c.kind || s.Access != c.access {
			t.Errorf("%s: got %s/%s, want %s/%s", c.sql, s.Kind, s.Access, c.kind, c.access)
		}
		if c.flags != nil && strings.Join(s.Flags, ",") != strings.Join(c.flags, ",") {
			t.Errorf("%s: flags %v, want %v", c.sql, s.Flags, c.flags)
		}
		if c.flags == nil && len(s.Flags) != 0 && c.kind != "SELECT" && c.kind != "EXPLAIN" {
			t.Errorf("%s: unexpected flags %v", c.sql, s.Flags)
		}
		if c.objects != nil && strings.Join(s.Objects, ",") != strings.Join(c.objects, ",") {
			t.Errorf("%s: objects %v, want %v", c.sql, s.Objects, c.objects)
		}
	}
}

func TestClassifyScript(t *testing.T) {
	cls, stmts, err := Classify(`
CREATE TABLE regions (id serial PRIMARY KEY, name text);
CREATE INDEX regions_name_idx ON regions(name);
UPDATE orders SET region = 'eu' WHERE id < 100;
DELETE FROM orders_archive WHERE created_at < now() - interval '1 year';`)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 4 || cls.Access != adapter.Destructive {
		t.Fatalf("%d stmts, access %s", len(stmts), cls.Access)
	}
	if stmts[1].HasFlag(FlagNonConcurrentIndex) {
		t.Fatal("index on a table created in the same script should not be flagged")
	}
	if !strings.HasPrefix(stmts[2].Text, "UPDATE orders") {
		t.Fatalf("statement text: %q", stmts[2].Text)
	}
	if _, _, err := Classify("SELEC 1"); err == nil {
		t.Fatal("expected parse error")
	}
}
