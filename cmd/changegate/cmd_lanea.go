package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/internal/lanea"
	"github.com/srawlin/changegate/internal/migrations"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/pkg/adapter"
	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// Framework bookkeeping tables, excluded from data comparisons.
var bookkeeping = []string{"public.schema_migrations", "public.flyway_schema_history", "public.goose_db_version", "public._prisma_migrations",
	"public.atlas_schema_revisions", "atlas_schema_revisions.atlas_schema_revisions", "public.databasechangelog", "public.databasechangeloglock",
	"public.alembic_version", "public.django_migrations", "public.ar_internal_metadata"}

func migrationsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "migrations",
		Short: "Detect migration frameworks and list migrations",
		RunE: func(*cobra.Command, []string) error {
			if dir == "" {
				wd, _ := os.Getwd()
				dir = repoRoot(wd)
			}
			sets, err := migrations.DetectAll(dir)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(sets)
			}
			if len(sets) == 0 {
				fmt.Println("no migrations detected")
			}
			for _, s := range sets {
				fmt.Printf("%s  %s  (%d migrations)\n", s.Framework, s.Dir, len(s.Migrations))
				for _, m := range s.Migrations {
					down := "down"
					if !m.HasDown {
						down = "no down"
					}
					kind := "sql"
					if !m.SQL {
						kind = "code"
					}
					fmt.Printf("  %-16s %-40s %-8s %s\n", m.Version, m.Name, down, kind)
				}
				for _, n := range s.Notes {
					fmt.Println("  note:", n)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "repository root (default: git top level)")
	return cmd
}

// changedFiles lists files added or modified in base...HEAD.
func changedFiles(root, base string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", root, "diff", "--name-only", "--diff-filter=AMR", base+"...HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...HEAD: %w (fetch the base branch, e.g. actions/checkout with fetch-depth: 0)", base, err)
	}
	m := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			m[filepath.Clean(l)] = true
		}
	}
	return m, nil
}

type rehearseFlags struct {
	dir, framework, migDir, baseRef, dsn, fromDB, tier, schema, seed, scaleFrom, env, out, upCmd, downCmd string
	newPaths                                                                                              []string
	all, keep, neon                                                                                       bool
	synthetic, maxRows                                                                                    int64
}

func rehearseCmd() *cobra.Command {
	f := &rehearseFlags{}
	cmd := &cobra.Command{
		Use:   "rehearse",
		Short: "Rehearse new migrations on a copy: forward → verify → rollback → compare",
		Long: `Rehearse the migrations a change adds (Lane A). Changegate builds a throwaway
copy, replays base migrations (T0) or clones a database (--from-database,
--neon), optionally generates synthetic data (T1), then applies each new
migration, observing locks, table rewrites and rows touched; runs the down
migrations in reverse; and compares catalog and data checksums with the state
before each migration. Policy is applied to the evidence for --env.

  changegate rehearse --base-ref origin/main --dsn postgres://postgres@localhost/postgres --out evidence.json

Exit codes: 0 rehearsal ran (see the report and gate), 1 a migration failed.`,
		RunE: func(*cobra.Command, []string) error { return runRehearse(f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.dir, "dir", "", "repository root (default: git top level)")
	fl.StringVar(&f.framework, "framework", "", "migration framework (default: detected)")
	fl.StringVar(&f.migDir, "migrations-dir", "", "migrations directory relative to the root (default: detected)")
	fl.StringVar(&f.baseRef, "base-ref", "", "git ref of the base branch; migrations changed since it are new")
	fl.StringSliceVar(&f.newPaths, "new", nil, "treat these migration paths as new (instead of --base-ref)")
	fl.BoolVar(&f.all, "all", false, "treat every migration as new (rehearse the whole history)")
	fl.StringVar(&f.dsn, "dsn", "", "admin DSN of a scratch server (default: $CHANGEGATE_REHEARSAL_DSN)")
	fl.StringVar(&f.fromDB, "from-database", "", "clone this database (golden image or clone) instead of replaying base migrations")
	fl.BoolVar(&f.neon, "neon", false, "rehearse on a Neon branch ($NEON_API_KEY, $NEON_PROJECT_ID, $NEON_BRANCH_ID, --dsn = branch DSN)")
	fl.StringVar(&f.tier, "tier", "", "fidelity tier of the copy (T0-T3); inferred when not set")
	fl.StringVar(&f.schema, "schema", "", "load this schema dump instead of replaying base migrations")
	fl.StringVar(&f.seed, "seed", "", "SQL file to load after the base state")
	fl.Int64Var(&f.synthetic, "synthetic", 0, "generate N synthetic rows per table (T1)")
	fl.StringVar(&f.scaleFrom, "scale-from", "", "read per-table row counts (reltuples only, no values) from this DSN for synthetic data")
	fl.Int64Var(&f.maxRows, "max-rows", 100000, "cap synthetic rows per table")
	fl.StringVar(&f.upCmd, "up-cmd", "", "command that migrates to head (code-based frameworks; DATABASE_URL is set)")
	fl.StringVar(&f.downCmd, "down-cmd", "", "command that rolls back {n} migrations")
	fl.StringVar(&f.env, "env", "prod", "environment class the change will deploy to (for policy)")
	fl.StringVar(&f.out, "out", "", "write evidence JSON here (input to `report` and `gate`)")
	fl.BoolVar(&f.keep, "keep", false, "keep the rehearsal database")
	return cmd
}

func runRehearse(f *rehearseFlags) error {
	ctx := bg()
	if f.dir == "" {
		wd, _ := os.Getwd()
		f.dir = repoRoot(wd)
	}
	set, err := chooseSet(f.dir, f.framework, f.migDir)
	if err != nil {
		return err
	}
	ev := &lanea.Evidence{Env: f.env, Framework: set.Framework, Dir: set.Dir, BaseRef: f.baseRef}
	ev.Provenance = lanea.DetectProvenance(f.dir, f.baseRef, "")

	isNew := func(m migrations.Migration) bool { return f.all }
	switch {
	case len(f.newPaths) > 0:
		want := map[string]bool{}
		for _, p := range f.newPaths {
			want[filepath.Clean(p)] = true
		}
		isNew = func(m migrations.Migration) bool { return want[filepath.Clean(m.Path)] || want[filepath.Clean(m.DownPath)] }
	case f.baseRef != "":
		changed, err := changedFiles(f.dir, f.baseRef)
		if err != nil {
			return err
		}
		isNew = func(m migrations.Migration) bool { return changed[filepath.Clean(m.Path)] || (m.DownPath != "" && changed[filepath.Clean(m.DownPath)]) }
	case !f.all:
		return errors.New("say which migrations are new: --base-ref REF, --new PATH, or --all")
	}
	var base, news []postgres.MigrationInput
	seenNew := false
	for _, m := range set.Migrations {
		in := postgres.MigrationInput{ID: m.Version, Framework: m.Framework, Name: m.Name, Path: m.Path, Up: m.UpSQL, Down: m.DownSQL, HasDown: m.HasDown, NoTransaction: m.NoTransaction}
		if isNew(m) {
			seenNew = true
			news = append(news, in)
		} else {
			if seenNew {
				ev.Skipped = append(ev.Skipped, fmt.Sprintf("%s is older than a new migration but unchanged; it is replayed as base", m.Path))
			}
			base = append(base, in)
		}
	}
	if len(news) == 0 {
		ev.Rehearsal = nil
		ev.Decision = &model.Decision{Decision: policy.Approve, Reasons: []string{"no new migrations"}}
		return writeEvidence(f.out, ev)
	}
	opts := postgres.RehearsalOptions{AdminDSN: f.dsn, FromDatabase: f.fromDB, Tier: f.tier, Keep: f.keep, BookkeepingTables: bookkeeping}
	if opts.AdminDSN == "" {
		opts.AdminDSN = os.Getenv("CHANGEGATE_REHEARSAL_DSN")
	}
	codeBased := false
	for _, m := range set.Migrations {
		if !m.SQL {
			codeBased = true
		}
	}
	if codeBased {
		if f.upCmd == "" {
			return fmt.Errorf("%s migrations are code; pass --up-cmd and --down-cmd (e.g. --up-cmd 'bin/rails db:migrate' --down-cmd 'bin/rails db:rollback STEP={n}')", set.Framework)
		}
		opts.Commands = &postgres.Commands{Up: f.upCmd, Down: f.downCmd}
	}
	if f.neon {
		pg := postgres.New(postgres.Options{})
		defer pg.Close()
		src := adapter.Target{Name: "neon", Adapter: "postgres", Env: adapter.EnvStaging, Config: map[string]string{
			"dsn": opts.AdminDSN, "neon_project_id": os.Getenv("NEON_PROJECT_ID"), "neon_branch_id": os.Getenv("NEON_BRANCH_ID")}}
		cp, err := pg.ProvisionCopy(ctx, src, "changegate-rehearse-"+time.Now().UTC().Format("20060102-150405"))
		if err != nil {
			return err
		}
		defer func() { _ = pg.ReleaseCopy(ctx, cp) }()
		opts.Copy = &cp.Target
		if opts.Tier == "" {
			opts.Tier = postgres.TierT3
		}
	}
	if opts.AdminDSN == "" && opts.Copy == nil {
		return errors.New("--dsn (an admin connection to a scratch Postgres server) is required")
	}
	if f.schema != "" {
		b, err := os.ReadFile(f.schema)
		if err != nil {
			return err
		}
		opts.SchemaSQL = string(b)
	}
	if f.seed != "" {
		b, err := os.ReadFile(f.seed)
		if err != nil {
			return err
		}
		opts.SeedSQL = string(b)
	}
	opts.SyntheticRows = f.synthetic
	if f.scaleFrom != "" {
		counts, err := rowCounts(f.scaleFrom, f.maxRows)
		if err != nil {
			return err
		}
		opts.RowCounts = counts
		if opts.SyntheticRows == 0 {
			opts.SyntheticRows = 1
		}
	}
	if opts.SyntheticRows > f.maxRows {
		opts.SyntheticRows = f.maxRows
	}
	rep, err := postgres.RehearseMigrations(ctx, opts, base, news)
	if err != nil {
		return err
	}
	ev.Rehearsal = rep
	a, err := newApp()
	if err != nil {
		return err
	}
	defer a.close()
	pol, err := a.policy()
	if err != nil {
		return err
	}
	d := lanea.Decide(pol, ev)
	ev.Decision = &d
	if err := writeEvidence(f.out, ev); err != nil {
		return err
	}
	if !rep.Passed {
		return exitError{code: 1}
	}
	return nil
}

func rowCounts(dsn string, maxRows int64) (map[string]int64, error) {
	ctx := bg()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `SELECT n.nspname || '.' || c.relname, greatest(c.reltuples, 0)::bigint FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if v > maxRows {
			v = maxRows
		}
		out[k] = v
	}
	return out, rows.Err()
}

func writeEvidence(path string, ev *lanea.Evidence) error {
	if path != "" {
		b, _ := json.MarshalIndent(ev, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}
	if jsonOut {
		return printJSON(ev)
	}
	fmt.Print(lanea.Markdown(ev))
	return nil
}

func readEvidence(path string) (*lanea.Evidence, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ev lanea.Evidence
	return &ev, json.Unmarshal(b, &ev)
}

func reportCmd() *cobra.Command {
	var github, gitlab, status bool
	cmd := &cobra.Command{
		Use:   "report EVIDENCE.json",
		Short: "Render rehearsal evidence as a PR/MR comment (and post it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ev, err := readEvidence(args[0])
			if err != nil {
				return err
			}
			md := lanea.Markdown(ev)
			state, desc := statusOf(ev)
			switch {
			case github:
				g, err := lanea.GitHubFromEnv()
				if err != nil {
					return err
				}
				u, err := g.Comment(md)
				if err != nil {
					return err
				}
				fmt.Println("posted", u)
				if status {
					if err := g.Status(state, desc, u); err != nil {
						return err
					}
				}
				if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
					_ = appendFile(p, md)
				}
			case gitlab:
				g, err := lanea.GitLabFromEnv()
				if err != nil {
					return err
				}
				if err := g.Comment(md); err != nil {
					return err
				}
				fmt.Println("posted merge request note")
				if status {
					gs := map[string]string{"success": "success", "failure": "failed", "pending": "pending"}[state]
					if err := g.Status(gs, desc); err != nil {
						return err
					}
				}
			default:
				fmt.Print(md)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&github, "github", false, "post to the GitHub pull request (GitHub Actions env)")
	cmd.Flags().BoolVar(&gitlab, "gitlab", false, "post to the GitLab merge request (GitLab CI env)")
	cmd.Flags().BoolVar(&status, "status", false, "also set a commit status")
	return cmd
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(s)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func statusOf(ev *lanea.Evidence) (state, desc string) {
	if ev.Rehearsal != nil && !ev.Rehearsal.Passed {
		return "failure", "rehearsal failed"
	}
	if ev.Decision == nil {
		return "success", "no new migrations"
	}
	switch ev.Decision.Decision {
	case policy.Approve:
		return "success", "rehearsed; auto-approved"
	case policy.Deny:
		return "failure", "blocked by policy: " + strings.Join(ev.Decision.Reasons, "; ")
	}
	return "pending", "review required: " + strings.Join(ev.Decision.Approvers, ", ")
}

func gateCmd() *cobra.Command {
	var env string
	var reportOnly bool
	cmd := &cobra.Command{
		Use:   "gate EVIDENCE.json",
		Short: "Decide from rehearsal evidence whether a change may deploy",
		Long: `Apply policy to rehearsal evidence. Exit codes: 0 approved (standard change),
2 review required (approvers printed), 3 denied, 1 rehearsal failed.
With --report-only the decision is printed but the exit code is always 0
(the recommended way to start: report, don't block).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ev, err := readEvidence(args[0])
			if err != nil {
				return err
			}
			if env != "" {
				ev.Env = env
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			pol, err := a.policy()
			if err != nil {
				return err
			}
			d := lanea.Decide(pol, ev)
			if jsonOut {
				_ = printJSON(d)
			} else {
				fmt.Printf("decision: %s\n", d.Decision)
				if len(d.Approvers) > 0 {
					fmt.Printf("approvers: %s\n", strings.Join(d.Approvers, ", "))
				}
				for _, r := range d.Reasons {
					fmt.Printf("reason: %s\n", r)
				}
			}
			if p := os.Getenv("GITHUB_OUTPUT"); p != "" {
				_ = appendFile(p, fmt.Sprintf("decision=%s\napprovers=%s\n", d.Decision, strings.Join(d.Approvers, ",")))
			}
			if reportOnly {
				return nil
			}
			if ev.Rehearsal != nil && !ev.Rehearsal.Passed {
				return exitError{code: 1, msg: "rehearsal failed"}
			}
			switch d.Decision {
			case policy.Deny:
				return exitError{code: 3}
			case policy.Require:
				return exitError{code: 2}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "override the environment class (e.g. the deployment environment)")
	cmd.Flags().BoolVar(&reportOnly, "report-only", false, "never fail; just print the decision")
	return cmd
}

func detectCmd() *cobra.Command {
	var base string
	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Detect agent provenance of the commits in a change (bots, trailers, branches)",
		RunE: func(*cobra.Command, []string) error {
			wd, _ := os.Getwd()
			p := lanea.DetectProvenance(repoRoot(wd), base, "")
			if jsonOut {
				return printJSON(p)
			}
			if p.Agent {
				fmt.Printf("agent-authored: %s\n", p.Name)
			} else {
				fmt.Println("no agent signals found")
			}
			for _, s := range p.Signals {
				fmt.Println("  ", s)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base-ref", "", "base ref (commits in base..HEAD are inspected)")
	return cmd
}
