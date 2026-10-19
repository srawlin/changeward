package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"db/migrations/000001_init.up.sql":                    "CREATE TABLE a (id int);",
		"db/migrations/000001_init.down.sql":                  "DROP TABLE a;",
		"db/migrations/000002_add_b.up.sql":                   "ALTER TABLE a ADD COLUMN b int;",
		"flyway/sql/V1__init.sql":                             "CREATE TABLE f (id int);",
		"flyway/sql/V1_1__more.sql":                           "ALTER TABLE f ADD COLUMN x int;",
		"flyway/sql/V2__two.sql":                              "SELECT 1;",
		"flyway/sql/U1_1__more.sql":                           "ALTER TABLE f DROP COLUMN x;",
		"goose/20240101000000_init.sql":                       "-- +goose Up\nCREATE TABLE g (id int);\n-- +goose Down\nDROP TABLE g;\n",
		"atlas/migrations/20240101000000_init.sql":            "CREATE TABLE at (id int);",
		"atlas/migrations/atlas.sum":                          "h1:abc",
		"prisma/migrations/migration_lock.toml":               "provider = \"postgresql\"",
		"prisma/migrations/20240101000000_init/migration.sql": "CREATE TABLE p (id int);",
		"lq/changelog.sql":                                    "--liquibase formatted sql\n--changeset alice:1\nCREATE TABLE l (id int);\n--rollback DROP TABLE l;\n--changeset bob:2 runInTransaction:false\nCREATE INDEX CONCURRENTLY li ON l(id);\n",
		"app/db/migrate/20240101000000_create_users.rb":       "class CreateUsers < ActiveRecord::Migration[7.1]\n  def change\n  end\nend\n",
		"shop/migrations/__init__.py":                         "",
		"shop/migrations/0001_initial.py":                     "from django.db import migrations\nclass Migration(migrations.Migration):\n    pass\n",
		"alembic/versions/a1_init.py":                         "revision = 'a1'\ndown_revision = None\ndef upgrade():\n    pass\ndef downgrade():\n    op.drop_table('x')\n",
		"alembic/versions/b2_next.py":                         "revision = 'b2'\ndown_revision = 'a1'\ndef upgrade():\n    pass\ndef downgrade():\n    pass\n",
		"sq/sqitch.plan":                                      "%project=x\nusers 2024-01-01T00:00:00Z a <a@x> # users\n",
		"sq/deploy/users.sql":                                 "CREATE TABLE su (id int);",
		"sq/revert/users.sql":                                 "DROP TABLE su;",
	})
	sets, err := DetectAll(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Set{}
	for _, s := range sets {
		got[s.Framework] = s
	}
	for _, fw := range Frameworks {
		if _, ok := got[fw]; !ok {
			t.Errorf("framework %s not detected", fw)
		}
	}
	gm := got[GolangMigrate].Migrations
	if len(gm) != 2 || !gm[0].HasDown || gm[1].HasDown || !strings.Contains(gm[0].DownSQL, "DROP TABLE a") {
		t.Errorf("golang-migrate: %+v", gm)
	}
	fw := got[Flyway].Migrations
	if len(fw) != 3 || fw[1].Version != "1_1" || !fw[1].HasDown || fw[0].HasDown {
		t.Errorf("flyway order/undo: %+v", fw)
	}
	if g := got[Goose].Migrations[0]; !strings.Contains(g.UpSQL, "CREATE TABLE g") || !strings.Contains(g.DownSQL, "DROP TABLE g") {
		t.Errorf("goose: %+v", g)
	}
	lq := got[Liquibase].Migrations
	if len(lq) != 2 || !lq[0].HasDown || !lq[1].NoTransaction {
		t.Errorf("liquibase: %+v", lq)
	}
	al := got[Alembic].Migrations
	if len(al) != 2 || !strings.HasSuffix(al[0].Version, "a1") || !al[0].HasDown || al[1].HasDown {
		t.Errorf("alembic: %+v", al)
	}
	if got[Rails].Migrations[0].SQL || got[Prisma].Migrations[0].HasDown {
		t.Error("rails is code; prisma has no down")
	}
}
