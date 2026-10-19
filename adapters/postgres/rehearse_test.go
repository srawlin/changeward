package postgres

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/changegate/changegate/pkg/adapter"
)

func TestRehearseMigrations(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		ctx := context.Background()
		base := []MigrationInput{{ID: "1", Name: "init", Path: "001_init.up.sql", HasDown: true,
			Up: `CREATE TYPE order_status AS ENUM ('new', 'paid');
			     CREATE TABLE customers (id serial PRIMARY KEY, email varchar(80) NOT NULL UNIQUE);
			     CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id int NOT NULL REFERENCES customers(id), total numeric(10,2) NOT NULL,
			       status order_status NOT NULL DEFAULT 'new', legacy_id int NOT NULL DEFAULT 0, tags text[], placed_at timestamptz NOT NULL DEFAULT now());`}}
		news := []MigrationInput{
			{ID: "2", Name: "add_region", Path: "002_add_region.up.sql", HasDown: true,
				Up:   "ALTER TABLE orders ADD COLUMN region text; CREATE INDEX orders_region_idx ON orders(region);",
				Down: "DROP INDEX orders_region_idx; ALTER TABLE orders DROP COLUMN region;"},
			{ID: "3", Name: "drop_legacy", Path: "003_drop_legacy.up.sql", HasDown: true,
				Up:   "ALTER TABLE orders DROP COLUMN legacy_id;",
				Down: "ALTER TABLE orders ADD COLUMN legacy_id int NOT NULL DEFAULT 0;"},
		}
		// T0: the catalog is restored, so the rollback looks fine; the report says T0.
		rep, err := RehearseMigrations(ctx, RehearsalOptions{AdminDSN: admin}, base, news)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Tier != TierT0 || len(rep.Results) != 2 || rep.Results[0].Reversibility != adapter.Reversible {
			t.Fatalf("T0 report: %+v", rep)
		}
		if rep.Results[0].MaxLock != "ShareLock" && rep.Results[0].MaxLock != "AccessExclusiveLock" {
			t.Fatalf("locks not observed: %+v", rep.Results[0])
		}
		if !strings.Contains(strings.Join(rep.Results[0].Effects, "\n"), "+ column public.orders.region") {
			t.Fatalf("effects: %v", rep.Results[0].Effects)
		}
		// T1: synthetic data shows the down migration loses legacy_id values.
		rep, err = RehearseMigrations(ctx, RehearsalOptions{AdminDSN: admin, SyntheticRows: 200}, base, news)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Tier != TierT1 || rep.Results[1].Reversibility != adapter.Irreversible || !strings.Contains(strings.Join(rep.Results[1].RollbackDiff, " "), "public.orders") {
			t.Fatalf("T1 drop_legacy: %+v", rep.Results[1])
		}
		if rep.Results[0].Reversibility != adapter.CheckpointOnly || rep.Results[0].TableRows["public.orders"] != 200 || len(rep.Results[0].RowsTouched) != 0 {
			t.Fatalf("T1 add_region: %+v", rep.Results[0])
		}
		// add_region alone at T1 is cleanly reversible.
		rep, err = RehearseMigrations(ctx, RehearsalOptions{AdminDSN: admin, SyntheticRows: 200}, base, news[:1])
		if err != nil {
			t.Fatal(err)
		}
		if rep.Results[0].Reversibility != adapter.Reversible || rep.Results[0].Reapplied == nil || !*rep.Results[0].Reapplied {
			t.Fatalf("T1 add_region alone: %+v", rep.Results[0])
		}
		// Missing down and destructive: irreversible; earlier rollbacks are blocked.
		news[1].HasDown, news[1].Down = false, ""
		rep, err = RehearseMigrations(ctx, RehearsalOptions{AdminDSN: admin}, base, news)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Results[1].Reversibility != adapter.Irreversible || rep.Results[0].Reversibility != adapter.CheckpointOnly {
			t.Fatalf("blocked rollback: %+v", rep.Results)
		}
		// A failing forward migration fails the rehearsal.
		rep, err = RehearseMigrations(ctx, RehearsalOptions{AdminDSN: admin, SyntheticRows: 10}, base, []MigrationInput{
			{ID: "9", Name: "not_null", Up: "ALTER TABLE orders ADD COLUMN must int NOT NULL;"}})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Passed || !strings.Contains(rep.Results[0].Error, "forward migration failed") {
			t.Fatalf("expected failure: %+v", rep.Results[0])
		}
	})
}

// Command mode (Rails/Django/Alembic-style): psql stands in for the framework CLI.
func TestRehearseCommands(t *testing.T) {
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not installed")
	}
	eachServer(t, func(t *testing.T, admin string) {
		up := `psql "$DATABASE_URL" -q -v ON_ERROR_STOP=1 -c "CREATE TABLE IF NOT EXISTS widgets (id int PRIMARY KEY)" -c "ALTER TABLE widgets ADD COLUMN IF NOT EXISTS color text"`
		down := `test "$CHANGEGATE_NEW_MIGRATIONS" = "{n}" && psql "$DATABASE_URL" -q -v ON_ERROR_STOP=1 -c "ALTER TABLE widgets DROP COLUMN color"`
		rep, err := RehearseMigrations(context.Background(), RehearsalOptions{AdminDSN: admin, Commands: &Commands{Up: up, Down: down}}, nil,
			[]MigrationInput{{ID: "20240101", Framework: "rails", Name: "add_color", Path: "db/migrate/20240101_add_color.rb", HasDown: true}})
		if err != nil {
			t.Fatal(err)
		}
		r := rep.Results[0]
		if !r.Passed || r.Reversibility != adapter.Reversible || !strings.Contains(strings.Join(r.Effects, " "), "+ column public.widgets.color") {
			t.Fatalf("command rehearsal: %+v", r)
		}
		// A down command that does not undo the change is caught.
		rep, err = RehearseMigrations(context.Background(), RehearsalOptions{AdminDSN: admin, Commands: &Commands{Up: up, Down: "true"}}, nil,
			[]MigrationInput{{ID: "20240101", Framework: "rails", Name: "add_color", HasDown: true}})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Results[0].Reversibility == adapter.Reversible {
			t.Fatalf("no-op down accepted: %+v", rep.Results[0])
		}
	})
}
