package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteEveryFramework(t *testing.T) {
	m := NewMigration{Name: "Add region to orders", Header: []string{"from session s_1"},
		Up:   []string{"ALTER TABLE orders ADD COLUMN region text", "CREATE INDEX ON orders(region)"},
		Down: []string{"DROP INDEX IF EXISTS orders_region_idx", "ALTER TABLE orders DROP COLUMN IF EXISTS region"}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, fw := range Writable {
		t.Run(fw, func(t *testing.T) {
			root := t.TempDir()
			dir := map[string]string{Rails: "db/migrate", Django: "shop/migrations", Alembic: "alembic/versions", Prisma: "prisma/migrations", Atlas: "migrations"}[fw]
			if dir == "" {
				dir = "db/migrations"
			}
			// Seed what each framework needs to be detected.
			switch fw {
			case Django:
				write(t, root, map[string]string{dir + "/__init__.py": "", dir + "/0001_initial.py": "from django.db import migrations\nclass Migration(migrations.Migration):\n    pass\n"})
			case Alembic:
				write(t, root, map[string]string{dir + "/a1_init.py": "revision = 'a1'\ndown_revision = None\n"})
			case Prisma:
				write(t, root, map[string]string{dir + "/migration_lock.toml": "provider = \"postgresql\""})
			case Atlas:
				write(t, root, map[string]string{dir + "/atlas.sum": "h1:x"})
			case Sqitch:
				write(t, root, map[string]string{dir + "/sqitch.plan": "%project=x\n"})
			}
			sets, _ := DetectAll(root)
			var set *Set
			for i := range sets {
				if sets[i].Framework == fw {
					set = &sets[i]
				}
			}
			files, err := Write(root, fw, dir, set, m, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Fatal("no files written")
			}
			body, _ := os.ReadFile(filepath.Join(root, files[0]))
			if !strings.Contains(string(body), "ADD COLUMN region") {
				t.Fatalf("%s does not contain the up SQL:\n%s", files[0], body)
			}
			// Re-detect: the new migration must be found, with its down where the framework has one.
			sets, _ = DetectAll(root)
			found := false
			for _, s := range sets {
				for _, x := range s.Migrations {
					if s.Framework == fw && strings.Contains(x.Name+x.Path, "add_region_to_orders") {
						found = true
						hasDownFw := fw != Atlas && fw != Prisma
						if hasDownFw && !x.HasDown {
							t.Errorf("down not detected for %s", fw)
						}
						if x.SQL && !strings.Contains(x.UpSQL, "ADD COLUMN region") {
							t.Errorf("up SQL not read back for %s: %q", fw, x.UpSQL)
						}
					}
				}
			}
			if !found {
				t.Fatalf("written %s migration not re-detected (files %v)", fw, files)
			}
		})
	}
}
