package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/changegate/changegate/pkg/adapter"
	"github.com/jackc/pgx/v5"
)

// Rehearsal tiers (plan §5.3.1).
const (
	TierT0 = "T0" // schema only
	TierT1 = "T1" // synthetic data
	TierT2 = "T2" // masked copy
	TierT3 = "T3" // production clone
)

// MigrationInput is one migration to rehearse.
type MigrationInput struct {
	ID            string `json:"id"`
	Framework     string `json:"framework"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Up            string `json:"-"`
	Down          string `json:"-"`
	HasDown       bool   `json:"has_down"`
	NoTransaction bool   `json:"no_transaction,omitempty"`
}

// Commands run code-based migrations (Rails, Django, Alembic, Liquibase XML).
// They run with DATABASE_URL set to the rehearsal copy; Down may use {n},
// the number of new migrations.
type Commands struct {
	Up   string `json:"up" yaml:"up"`
	Down string `json:"down" yaml:"down"`
}

// RehearsalOptions configure a migration rehearsal.
type RehearsalOptions struct {
	// AdminDSN connects to a server where Changegate may CREATE DATABASE.
	AdminDSN string
	// FromDatabase clones an existing database (golden image or clone) with
	// CREATE DATABASE ... TEMPLATE. Base migrations are assumed applied.
	FromDatabase string
	// Copy, if set, is an already provisioned copy (e.g. a Neon branch).
	Copy *adapter.Target
	// Tier labels the copy's fidelity. Defaults: T0 for an empty database,
	// T1 with synthetic data, T2 for FromDatabase/Copy unless set.
	Tier string
	// SchemaSQL is loaded instead of replaying base migrations (pg_dump --schema-only).
	SchemaSQL string
	// SeedSQL is run after the base state is built.
	SeedSQL string
	// SyntheticRows generates rows per table (T1). RowCounts overrides per table.
	SyntheticRows int64
	RowCounts     map[string]int64
	// Commands for code-based frameworks.
	Commands *Commands
	// BookkeepingTables are excluded from data comparisons.
	BookkeepingTables []string
	// Keep leaves the rehearsal database in place.
	Keep bool
	// StatementTimeout bounds each migration. Default 10 minutes.
	StatementTimeout time.Duration
}

// MigrationResult is the evidence for one migration.
type MigrationResult struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Path          string                `json:"path"`
	Framework     string                `json:"framework"`
	Reversibility adapter.Reversibility `json:"reversibility"`
	Destructive   bool                  `json:"destructive"`
	Passed        bool                  `json:"passed"`
	HasDown       bool                  `json:"has_down"`
	UpMs          int64                 `json:"up_ms"`
	DownMs        int64                 `json:"down_ms,omitempty"`
	Locks         map[string]string     `json:"locks,omitempty"`
	MaxLock       string                `json:"max_lock,omitempty"`
	Rewrites      []string              `json:"rewrites,omitempty"`
	RowsTouched   map[string]int64      `json:"rows_touched,omitempty"`
	TableRows     map[string]int64      `json:"table_rows,omitempty"`
	Effects       []string              `json:"effects,omitempty"`
	RollbackDiff  []string              `json:"rollback_diff,omitempty"`
	Findings      []Finding             `json:"findings,omitempty"`
	Flags         []string              `json:"flags,omitempty"`
	Error         string                `json:"error,omitempty"`
	Notes         []string              `json:"notes,omitempty"`
	Reapplied     *bool                 `json:"reapplied,omitempty"`
}

// RehearsalReport is the evidence for a rehearsal.
type RehearsalReport struct {
	Tier          string            `json:"tier"`
	Database      string            `json:"database"`
	ServerVersion int               `json:"server_version"`
	BaseCount     int               `json:"base_migrations"`
	Results       []MigrationResult `json:"results"`
	Passed        bool              `json:"passed"`
	Reversibility adapter.Reversibility `json:"reversibility"`
	Destructive   bool              `json:"destructive"`
	BlastRows     int64             `json:"blast_rows"`
	StartedAt     string            `json:"started_at"`
	DurationMs    int64             `json:"duration_ms"`
	Notes         []string          `json:"notes,omitempty"`
}

// RehearseMigrations builds a copy, applies base migrations (or loads a
// schema), then applies each new migration forward, observing locks,
// rewrites and rows touched; then runs the downs in reverse and compares the
// catalog and data with the state before each migration.
func RehearseMigrations(ctx context.Context, o RehearsalOptions, base, news []MigrationInput) (*RehearsalReport, error) {
	start := time.Now()
	if o.StatementTimeout == 0 {
		o.StatementTimeout = 10 * time.Minute
	}
	rep := &RehearsalReport{Passed: true, Reversibility: adapter.Reversible, StartedAt: start.UTC().Format(time.RFC3339)}
	exclude := map[string]bool{}
	for _, t := range o.BookkeepingTables {
		exclude[t] = true
	}

	dsn, cleanup, err := provision(ctx, &o, rep)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	if rep.ServerVersion, err = serverVersion(ctx, conn); err != nil {
		return nil, err
	}

	// Base state.
	if o.FromDatabase == "" && o.Copy == nil {
		if o.SchemaSQL != "" {
			if _, err := conn.Exec(ctx, o.SchemaSQL); err != nil {
				return nil, fmt.Errorf("load schema: %w", err)
			}
			rep.Notes = append(rep.Notes, "base state loaded from a schema dump")
		} else if o.Commands == nil {
			for _, m := range base {
				if _, err := conn.Exec(ctx, m.Up); err != nil {
					return nil, fmt.Errorf("replay base migration %s: %w", m.Path, err)
				}
			}
			rep.BaseCount = len(base)
		}
	}
	if o.SeedSQL != "" {
		if _, err := conn.Exec(ctx, o.SeedSQL); err != nil {
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	if o.SyntheticRows > 0 || len(o.RowCounts) > 0 {
		n, err := GenerateSynthetic(ctx, conn, o.SyntheticRows, o.RowCounts)
		if err != nil {
			return nil, fmt.Errorf("synthetic data: %w", err)
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("generated %d synthetic rows (no production values used)", n))
		if o.Tier == "" {
			o.Tier = TierT1
		}
	}
	if o.Tier == "" {
		o.Tier = TierT0
	}
	rep.Tier = o.Tier
	if rep.Tier == TierT0 {
		rep.Notes = append(rep.Notes, "T0 (schema only): data-dependent failures, real lock durations and backfill times are not covered")
	}

	if o.Commands != nil {
		rehearseCommands(ctx, conn, dsn, o, news, rep, exclude)
	} else {
		rehearseSQL(ctx, conn, o, news, rep, exclude)
	}
	for _, r := range rep.Results {
		rep.Passed = rep.Passed && r.Passed
		rep.Reversibility = adapter.Worst(rep.Reversibility, r.Reversibility)
		rep.Destructive = rep.Destructive || r.Destructive
		for _, n := range r.RowsTouched {
			rep.BlastRows += n
		}
	}
	rep.DurationMs = time.Since(start).Milliseconds()
	return rep, nil
}

func provision(ctx context.Context, o *RehearsalOptions, rep *RehearsalReport) (string, func(), error) {
	if o.Copy != nil {
		rep.Database = o.Copy.Name
		if o.Tier == "" {
			o.Tier = TierT2
		}
		return o.Copy.Config["dsn"], func() {}, nil
	}
	admin, err := pgx.Connect(ctx, o.AdminDSN)
	if err != nil {
		return "", nil, fmt.Errorf("connect admin DSN: %w", err)
	}
	name := fmt.Sprintf("cg_rehearse_%d", time.Now().UnixNano())
	create := "CREATE DATABASE " + QuoteIdent(name)
	if o.FromDatabase != "" {
		create += " TEMPLATE " + QuoteIdent(o.FromDatabase)
		if o.Tier == "" {
			o.Tier = TierT2
		}
	}
	if _, err := admin.Exec(ctx, create); err != nil {
		_ = admin.Close(ctx)
		return "", nil, fmt.Errorf("create rehearsal database: %w", err)
	}
	rep.Database = name
	dsn, err := WithDatabase(o.AdminDSN, name)
	if err != nil {
		_ = admin.Close(ctx)
		return "", nil, err
	}
	return dsn, func() {
		if !o.Keep {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+QuoteIdent(name)+" WITH (FORCE)")
		}
		_ = admin.Close(context.Background())
	}, nil
}

func catalogSnap(ctx context.Context, q querier) (*Snapshot, error) { return TakeSnapshot(ctx, q, false, nil) }

// checksums returns per-table data checksums for the named tables.
func checksums(ctx context.Context, q querier, tables []string) (map[string]TableData, error) {
	out := map[string]TableData{}
	for _, t := range tables {
		var td TableData
		parts := strings.SplitN(t, ".", 2)
		if len(parts) != 2 {
			continue
		}
		qn := QuoteIdent(parts[0]) + "." + QuoteIdent(parts[1])
		var exists bool
		if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, qn).Scan(&exists); err != nil || !exists {
			continue
		}
		if err := q.QueryRow(ctx, `SELECT count(*), coalesce(md5(string_agg(h, '' ORDER BY h)), '') FROM (SELECT md5(x::text) h FROM `+qn+` x) s`).
			Scan(&td.Rows, &td.Checksum); err != nil {
			return nil, err
		}
		out[t] = td
	}
	return out, nil
}

func tableNames(s *Snapshot) []string {
	var out []string
	for k, v := range s.Objects {
		if strings.HasPrefix(k, "relation ") && (strings.HasPrefix(v, "r") || strings.HasPrefix(v, "p")) {
			out = append(out, strings.TrimPrefix(k, "relation "))
		}
	}
	sort.Strings(out)
	return out
}

func filenodes(ctx context.Context, q querier) (map[string]uint32, error) {
	rows, err := q.Query(ctx, `SELECT n.nspname || '.' || c.relname, c.relfilenode FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','m') AND `+userSchemaFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint32{}
	for rows.Next() {
		var k string
		var v uint32
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func xactStats(ctx context.Context, q querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT schemaname || '.' || relname, n_tup_ins + n_tup_upd + n_tup_del FROM pg_stat_xact_user_tables
		WHERE n_tup_ins + n_tup_upd + n_tup_del > 0`)
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
		out[k] = v
	}
	return out, rows.Err()
}

func liveRows(ctx context.Context, q querier, tables []string) map[string]int64 {
	out := map[string]int64{}
	for _, t := range tables {
		parts := strings.SplitN(t, ".", 2)
		if len(parts) != 2 {
			continue
		}
		var n int64
		if err := q.QueryRow(ctx, `SELECT count(*) FROM `+QuoteIdent(parts[0])+"."+QuoteIdent(parts[1])).Scan(&n); err == nil {
			out[t] = n
		}
	}
	return out
}

type state struct {
	cat  *Snapshot
	data map[string]TableData
}

// runMigration applies sql (transactionally when possible) and observes it.
func runMigration(ctx context.Context, conn *pgx.Conn, sql string, noTx bool, timeout time.Duration, r *MigrationResult) error {
	stmts, err := Parse(sql)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if !s.Transactional {
			noTx = true
		}
	}
	before, err := filenodes(ctx, conn)
	if err != nil {
		return err
	}
	t0 := time.Now()
	if noTx {
		for _, s := range stmts {
			if _, err := conn.Exec(ctx, s.Text); err != nil {
				return err
			}
		}
		r.Notes = append(r.Notes, "ran outside a transaction (CONCURRENTLY or no-transaction migration); locks not observed")
	} else {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
			return err
		}
		// pg_stat_xact can include not-yet-flushed counts from earlier work in
		// this backend, so take a delta.
		statsBefore, err := xactStats(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			return err
		}
		if r.Locks, err = heldLocks(ctx, tx); err != nil {
			return err
		}
		statsAfter, err := xactStats(ctx, tx)
		if err != nil {
			return err
		}
		r.RowsTouched = map[string]int64{}
		for k, v := range statsAfter {
			if d := v - statsBefore[k]; d > 0 {
				r.RowsTouched[k] = d
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	ms := time.Since(t0).Milliseconds()
	r.UpMs = ms
	after, err := filenodes(ctx, conn)
	if err != nil {
		return err
	}
	for k, v := range after {
		if b, ok := before[k]; ok && b != v {
			r.Rewrites = append(r.Rewrites, k)
		}
	}
	sort.Strings(r.Rewrites)
	for rel, mode := range r.Locks {
		if _, existed := before[rel]; !existed || lockRank(mode) < lockRank("ShareUpdateExclusiveLock") {
			delete(r.Locks, rel)
			continue
		}
		if lockRank(mode) > lockRank(r.MaxLock) {
			r.MaxLock = mode
		}
	}
	return nil
}

func rehearseSQL(ctx context.Context, conn *pgx.Conn, o RehearsalOptions, news []MigrationInput, rep *RehearsalReport, exclude map[string]bool) {
	var states []state
	var results []*MigrationResult
	snap := func(tables []string) (state, error) {
		cat, err := catalogSnap(ctx, conn)
		if err != nil {
			return state{}, err
		}
		var keep []string
		for _, t := range tables {
			if !exclude[t] {
				keep = append(keep, t)
			}
		}
		data, err := checksums(ctx, conn, keep)
		return state{cat, data}, err
	}
	cur, err := snap(nil)
	if err != nil {
		rep.Notes = append(rep.Notes, "snapshot failed: "+err.Error())
		rep.Passed = false
		return
	}
	applied := 0
	for _, m := range news {
		r := &MigrationResult{ID: m.ID, Name: m.Name, Path: m.Path, Framework: m.Framework, HasDown: m.HasDown, Passed: true}
		results = append(results, r)
		cls, stmts, perr := Classify(m.Up)
		if perr != nil {
			r.Passed, r.Error, r.Reversibility = false, "parse: "+perr.Error(), adapter.Irreversible
			break
		}
		r.Flags = cls.Flags
		r.Findings = Lint(stmts)
		r.Destructive = cls.Access == adapter.Destructive
		// Tables this migration may touch: everything (cheap to filter later).
		tables := tableNames(cur.cat)
		pre, err := snap(tables)
		if err != nil {
			r.Passed, r.Error = false, err.Error()
			break
		}
		r.TableRows = liveRows(ctx, conn, tables)
		if err := runMigration(ctx, conn, m.Up, m.NoTransaction, o.StatementTimeout, r); err != nil {
			r.Passed, r.Error, r.Reversibility = false, "forward migration failed: "+err.Error(), adapter.Irreversible
			break
		}
		applied++
		post, err := snap(tableNames(cur.cat))
		if err != nil {
			r.Passed, r.Error = false, err.Error()
			break
		}
		r.Effects = DiffSnapshots(pre.cat, post.cat)
		for t, d := range post.data {
			if p, ok := pre.data[t]; ok && p.Checksum != d.Checksum && r.RowsTouched[t] == 0 {
				r.Effects = append(r.Effects, fmt.Sprintf("~ data %s: contents changed (%d → %d rows)", t, p.Rows, d.Rows))
			}
		}
		states = append(states, pre)
		cur = post
	}
	// Roll back in reverse and compare with the state before each migration.
	blocked := ""
	for i := applied - 1; i >= 0; i-- {
		r, m := results[i], news[i]
		if blocked != "" {
			r.Reversibility = adapter.Worst(r.Reversibility, adapter.CheckpointOnly)
			r.Notes = append(r.Notes, "rollback not rehearsed: a later migration ("+blocked+") could not be rolled back cleanly")
			continue
		}
		if !m.HasDown || strings.TrimSpace(m.Down) == "" {
			blocked = m.Name
			if r.Destructive {
				r.Reversibility = adapter.Irreversible
				r.Notes = append(r.Notes, "destructive and has no down migration")
			} else {
				r.Reversibility = adapter.CheckpointOnly
				r.Notes = append(r.Notes, "no down migration; undo requires restoring a snapshot")
			}
			continue
		}
		t0 := time.Now()
		if _, err := conn.Exec(ctx, m.Down); err != nil {
			r.Reversibility, r.Passed = adapter.Irreversible, false
			r.RollbackDiff = []string{"down migration failed: " + err.Error()}
			blocked = m.Name
			continue
		}
		r.DownMs = time.Since(t0).Milliseconds()
		pre := states[i]
		var tables []string
		for t := range pre.data {
			tables = append(tables, t)
		}
		after, err := snap(tables)
		if err != nil {
			r.Error = err.Error()
			continue
		}
		diff := DiffSnapshots(pre.cat, after.cat)
		for t, d := range pre.data {
			a, ok := after.data[t]
			switch {
			case !ok:
				diff = append(diff, fmt.Sprintf("- data %s: table missing after rollback", t))
			case a.Rows != d.Rows:
				diff = append(diff, fmt.Sprintf("~ data %s: %d rows before, %d after rollback", t, d.Rows, a.Rows))
			case a.Checksum != d.Checksum:
				diff = append(diff, fmt.Sprintf("~ data %s: row contents differ after rollback (%d rows)", t, a.Rows))
			}
		}
		sort.Strings(diff)
		r.RollbackDiff = diff
		if len(diff) == 0 {
			if r.Reversibility == "" {
				r.Reversibility = adapter.Reversible
			}
		} else {
			r.Reversibility = adapter.Irreversible
			r.Notes = append(r.Notes, "rollback does not restore the previous state")
			blocked = m.Name // earlier comparisons would inherit this difference
		}
	}
	// Re-apply: the forward migrations must still work after their rollback.
	if blocked == "" && applied == len(news) && applied > 0 {
		for i, m := range news {
			ok := true
			if _, err := conn.Exec(ctx, m.Up); err != nil {
				ok = false
				results[i].Passed = false
				results[i].Notes = append(results[i].Notes, "re-applying after rollback failed: "+err.Error())
			}
			results[i].Reapplied = &ok
			if !ok {
				break
			}
		}
	}
	for _, r := range results {
		if r.Reversibility == "" {
			r.Reversibility = adapter.CheckpointOnly
		}
		rep.Results = append(rep.Results, *r)
	}
}

// rehearseCommands rehearses code-based migrations through their CLI:
// up (to head), down {n}, then up again; compares head before and after.
func rehearseCommands(ctx context.Context, conn *pgx.Conn, dsn string, o RehearsalOptions, news []MigrationInput, rep *RehearsalReport, exclude map[string]bool) {
	run := func(cmdline string) (int64, map[string]string, error) {
		stop := make(chan struct{})
		var mu sync.Mutex
		locks := map[string]string{}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // sample locks held by the migration process
			defer wg.Done()
			sampler, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return
			}
			defer func() { _ = sampler.Close(context.Background()) }()
			for {
				select {
				case <-stop:
					return
				case <-time.After(25 * time.Millisecond):
				}
				rows, err := sampler.Query(ctx, `SELECT n.nspname || '.' || c.relname, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
					JOIN pg_namespace n ON n.oid = c.relnamespace WHERE l.pid <> pg_backend_pid() AND l.granted AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND `+userSchemaFilter)
				if err != nil {
					continue
				}
				for rows.Next() {
					var rel, mode string
					if rows.Scan(&rel, &mode) == nil {
						mu.Lock()
						if lockRank(mode) > lockRank(locks[rel]) {
							locks[rel] = mode
						}
						mu.Unlock()
					}
				}
				rows.Close()
			}
		}()
		t0 := time.Now()
		cmd := exec.CommandContext(ctx, "sh", "-c", strings.ReplaceAll(cmdline, "{n}", fmt.Sprint(len(news))))
		cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn, "CHANGEGATE_REHEARSAL=1", fmt.Sprintf("CHANGEGATE_NEW_MIGRATIONS=%d", len(news)))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		close(stop)
		wg.Wait()
		if err != nil {
			return 0, locks, fmt.Errorf("%v: %s", err, lastLines(out.String(), 15))
		}
		return time.Since(t0).Milliseconds(), locks, nil
	}
	r := MigrationResult{ID: "command", Name: fmt.Sprintf("%d new migration(s)", len(news)), Framework: firstFramework(news), Passed: true, HasDown: o.Commands.Down != ""}
	for _, m := range news {
		r.Notes = append(r.Notes, "includes "+m.Path)
	}
	if ms, locks, err := run(o.Commands.Up); err != nil {
		r.Passed, r.Error, r.Reversibility = false, "up failed: "+err.Error(), adapter.Irreversible
		rep.Results = append(rep.Results, r)
		return
	} else {
		r.UpMs = ms
		r.Locks = locks
	}
	head, _ := catalogSnap(ctx, conn)
	if o.Commands.Down == "" {
		r.Reversibility = adapter.CheckpointOnly
		r.Notes = append(r.Notes, "no down command configured")
		rep.Results = append(rep.Results, r)
		return
	}
	ms, _, err := run(o.Commands.Down)
	if err != nil {
		r.Reversibility, r.Passed = adapter.Irreversible, false
		r.RollbackDiff = []string{"down failed: " + err.Error()}
		rep.Results = append(rep.Results, r)
		return
	}
	r.DownMs = ms
	baseSnap, _ := catalogSnap(ctx, conn)
	r.Effects = DiffSnapshots(baseSnap, head)
	if _, _, err := run(o.Commands.Up); err != nil {
		ok := false
		r.Reapplied, r.Passed = &ok, false
		r.Notes = append(r.Notes, "re-applying after rollback failed: "+err.Error())
	} else {
		ok := true
		r.Reapplied = &ok
		again, _ := catalogSnap(ctx, conn)
		if d := DiffSnapshots(head, again); len(d) > 0 {
			r.RollbackDiff = d
			r.Reversibility = adapter.Irreversible
			r.Notes = append(r.Notes, "up → down → up does not reproduce the same schema")
		} else {
			r.Reversibility = adapter.Reversible
		}
	}
	for _, l := range r.Effects {
		if strings.HasPrefix(l, "- ") {
			r.Destructive = true
		}
	}
	for rel, mode := range r.Locks {
		if lockRank(mode) > lockRank(r.MaxLock) {
			r.MaxLock = mode
		}
		_ = rel
	}
	_ = exclude
	r.Notes = append(r.Notes, "code-based migrations: data comparison is not performed; locks sampled every 25ms")
	rep.Results = append(rep.Results, r)
}

func firstFramework(ms []MigrationInput) string {
	if len(ms) > 0 {
		return ms[0].Framework
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ErrNoMigrations is returned when there is nothing to rehearse.
var ErrNoMigrations = errors.New("no new migrations to rehearse")
