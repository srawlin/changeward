// Package postgres is the Changegate Postgres adapter.
//
// Lane B (direct): a change is planned by executing it inside a REPEATABLE
// READ transaction that is always rolled back, so the plan reflects real
// rows, constraints and triggers. On Postgres 18+ DML is rewritten with
// RETURNING OLD/NEW for exact per-row before/after images; older versions
// pre-select target rows for simple single-table statements. Apply re-runs
// the statement and aborts if the affected rows differ from the plan
// (drift). Undo applies guarded inverses that touch a row only if it still
// equals its after-image. Destructive DDL is soft-dropped into
// _changegate_trash so it stays reversible without backups.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srawlin/changegate/pkg/adapter"
)

// Version is the adapter version.
const Version = "0.1.0"

// Options tune the adapter.
type Options struct {
	// StateDir holds local checkpoint dumps.
	StateDir string
	// MaxUndoRows caps the before/after images kept per change. Above it a
	// change is checkpoint-only. Default 50,000.
	MaxUndoRows int
	// DisplayRows is how many rows per statement the diff shows. Default 10.
	DisplayRows int
	// PlanStatementTimeout and PlanLockTimeout bound the dry run. Defaults 10s / 2s.
	PlanStatementTimeout time.Duration
	PlanLockTimeout      time.Duration
	// ApplyLockTimeout bounds lock waits at apply and undo. Default 5s.
	ApplyLockTimeout time.Duration
}

func (o *Options) defaults() {
	if o.MaxUndoRows == 0 {
		o.MaxUndoRows = 50000
	}
	if o.DisplayRows == 0 {
		o.DisplayRows = 10
	}
	if o.PlanStatementTimeout == 0 {
		o.PlanStatementTimeout = 10 * time.Second
	}
	if o.PlanLockTimeout == 0 {
		o.PlanLockTimeout = 2 * time.Second
	}
	if o.ApplyLockTimeout == 0 {
		o.ApplyLockTimeout = 5 * time.Second
	}
}

// Adapter implements adapter.Adapter for Postgres.
type Adapter struct {
	opts  Options
	mu    sync.Mutex
	pools map[string]*pgxpool.Pool
}

// New returns a Postgres adapter.
func New(o Options) *Adapter {
	o.defaults()
	return &Adapter{opts: o, pools: map[string]*pgxpool.Pool{}}
}

// Close closes all connection pools.
func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, p := range a.pools {
		p.Close()
		delete(a.pools, k)
	}
}

func (a *Adapter) pool(ctx context.Context, t adapter.Target) (*pgxpool.Pool, error) {
	dsn := t.Config["dsn"]
	if dsn == "" {
		return nil, fmt.Errorf("target %q has no dsn", t.Name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.pools[dsn]; ok {
		return p, nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", t.Name, err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["application_name"] = "changegate"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	a.pools[dsn] = p
	return p, nil
}

func (a *Adapter) dropPool(dsn string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.pools[dsn]; ok {
		p.Close()
		delete(a.pools, dsn)
	}
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "postgres", Version: Version, Kinds: []string{"sql"}, Capabilities: []string{
		adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo,
		adapter.CapRehearse, adapter.CapCheckpoint, adapter.CapRestore, adapter.CapProvisionCopy,
	}}, nil
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, _ adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	c, _, err := Classify(op.Payload)
	return c, err
}

type planData struct {
	V                int        `json:"v"`
	ServerVersion    int        `json:"server_version"`
	NonTransactional bool       `json:"non_transactional,omitempty"`
	Steps            []planStep `json:"steps"`
}

type planStep struct {
	SQL           string                `json:"sql"`
	Kind          string                `json:"kind"`
	Category      string                `json:"category"`
	Rel           string                `json:"rel,omitempty"`
	Capture       string                `json:"capture,omitempty"`
	ExecSQL       string                `json:"exec_sql,omitempty"`
	PreSQL        string                `json:"pre_sql,omitempty"`
	Rows          int64                 `json:"rows"`
	BeforeHash    string                `json:"before_hash,omitempty"`
	Soft          string                `json:"soft,omitempty"`
	Signature     string                `json:"signature,omitempty"`
	Reversibility adapter.Reversibility `json:"reversibility"`
}

type undoHandle struct {
	V                int        `json:"v"`
	NonTransactional bool       `json:"non_transactional,omitempty"`
	Steps            []undoStep `json:"steps"`
}

type undoStep struct {
	Index int        `json:"index"`
	Kind  string     `json:"kind"`
	DML   *dmlUndo   `json:"dml,omitempty"`
	DDL   *ddlResult `json:"ddl,omitempty"`
}

func validate(stmts []*Stmt) error {
	if len(stmts) == 0 {
		return errors.New("no SQL statements")
	}
	writes := 0
	for _, s := range stmts {
		if s.HasFlag(FlagTxControl) {
			return errors.New("transaction control (BEGIN/COMMIT/ROLLBACK) is not allowed; each change runs in its own transaction")
		}
		if s.Kind == "SET" && !strings.Contains(strings.ToUpper(s.Text), "LOCAL") {
			return errors.New("session-level SET is not allowed in a change; use SET LOCAL")
		}
		if !s.Transactional && len(stmts) > 1 {
			return fmt.Errorf("%s cannot run inside a transaction and must be submitted on its own", s.Kind)
		}
		if s.Access != adapter.Read {
			writes++
		}
	}
	if writes == 0 {
		return ErrReadOnly
	}
	return nil
}

// ErrReadOnly is returned when asked to plan an operation that only reads.
var ErrReadOnly = errors.New("operation is read-only; use query instead of proposing a change")

func serverVersion(ctx context.Context, q querier) (int, error) {
	var s string
	if err := q.QueryRow(ctx, `SHOW server_version_num`).Scan(&s); err != nil {
		return 0, err
	}
	var v int
	_, err := fmt.Sscan(s, &v)
	return v, err
}

func setLocal(ctx context.Context, tx pgx.Tx, stmt, lock time.Duration) error {
	if stmt > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", stmt.Milliseconds())); err != nil {
			return err
		}
	}
	if lock > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", lock.Milliseconds())); err != nil {
			return err
		}
	}
	return nil
}

// Plan implements adapter.Adapter: a live dry run in a rolled-back transaction.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	switch op.Kind {
	case "changegate.fingerprint":
		return a.fingerprintPlan(ctx, t)
	case "sql", "":
	default:
		return nil, fmt.Errorf("postgres: unsupported operation kind %q", op.Kind)
	}
	cls, stmts, err := Classify(op.Payload)
	if err != nil {
		return nil, err
	}
	if err := validate(stmts); err != nil {
		return nil, err
	}
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	sv, err := serverVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	b := &planBuilder{a: a, t: t, sv: sv, plan: &adapter.Plan{
		ID: adapter.NewID("p"), Adapter: "postgres", Target: t.Name, Env: t.Env, Operation: op,
		Classification: cls, Reversibility: adapter.Reversible, Facts: map[string]string{},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, data: &planData{V: 1, ServerVersion: sv}, redact: newRedactor(t)}
	b.plan.Facts["server_version"] = fmt.Sprint(sv)

	if !stmts[0].Transactional {
		err = b.static(stmts[0])
	} else {
		err = b.dryRun(ctx, conn.Conn(), stmts)
	}
	if err != nil {
		return nil, err
	}
	return b.finish()
}

type planBuilder struct {
	a      *Adapter
	t      adapter.Target
	sv     int
	plan   *adapter.Plan
	data   *planData
	diff   strings.Builder
	locks  map[string]string
	redact *redactor
}

func (b *planBuilder) warn(w string) {
	for _, x := range b.plan.Warnings {
		if x == w {
			return
		}
	}
	b.plan.Warnings = append(b.plan.Warnings, w)
}

func (b *planBuilder) worst(r adapter.Reversibility) {
	b.plan.Reversibility = adapter.Worst(b.plan.Reversibility, r)
}

// static plans a non-transactional statement without executing it.
func (b *planBuilder) static(s *Stmt) error {
	b.data.NonTransactional = true
	b.plan.RehearsalTier = "static"
	st := classifyDDL(s)
	b.worst(st.Reversibility)
	b.data.Steps = append(b.data.Steps, planStep{SQL: s.Text, Kind: s.Kind, Category: s.Category, Rel: RelName(s.Rel), Reversibility: st.Reversibility})
	b.warn(s.Kind + " cannot run in a transaction, so it was not dry-run on the target; rehearse on a copy for evidence")
	fmt.Fprintf(&b.diff, "#1 %s\n   not dry-run (non-transactional)\n", firstLine(s.Text))
	for _, n := range st.Notes {
		fmt.Fprintf(&b.diff, "   %s\n", n)
	}
	b.plan.Destructive = s.Access == adapter.Destructive
	return nil
}

func (b *planBuilder) dryRun(ctx context.Context, conn *pgx.Conn, stmts []*Stmt) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // always rolled back
	if err := setLocal(ctx, tx, b.a.opts.PlanStatementTimeout, b.a.opts.PlanLockTimeout); err != nil {
		return err
	}
	b.plan.RehearsalTier = "live-dry-run"
	b.locks = map[string]string{}
	var totalMs int64
	for i, s := range stmts {
		start := time.Now()
		step := planStep{SQL: s.Text, Kind: s.Kind, Category: s.Category, Rel: RelName(s.Rel), Reversibility: adapter.Reversible}
		if unsafeToDryRun(s) {
			// Effects outside the transaction (terminating backends, dblink,
			// server files, arbitrary code) would happen even though the dry
			// run rolls back, so these statements are never executed here.
			step.Reversibility, step.Capture = adapter.Irreversible, CaptureNone
			b.worst(adapter.Irreversible)
			b.data.Steps = append(b.data.Steps, step)
			b.plan.Destructive = true
			b.warn(fmt.Sprintf("#%d %s has effects outside the transaction; it was not executed in the dry run and cannot be undone", i+1, s.Kind))
			fmt.Fprintf(&b.diff, "#%d %s (not dry-run: effects outside the transaction)\n", i+1, firstLine(s.Text))
			continue
		}
		var err error
		switch s.Category {
		case CatDML:
			err = b.planDML(ctx, tx, i, s, &step)
		case CatDDL:
			err = b.planDDL(ctx, tx, i, s, &step)
		default:
			if s.Access == adapter.Read {
				_, err = tx.Exec(ctx, s.Text)
				step.Reversibility = ""
				fmt.Fprintf(&b.diff, "#%d %s (read)\n", i+1, firstLine(s.Text))
			} else {
				err = b.planOther(ctx, tx, i, s, &step)
			}
		}
		if err != nil {
			return describePGError(i+1, s, err)
		}
		ms := time.Since(start).Milliseconds()
		totalMs += ms
		b.worst(step.Reversibility)
		b.data.Steps = append(b.data.Steps, step)
		if s.Access == adapter.Destructive {
			b.plan.Destructive = true
		}
		for _, f := range s.Flags {
			switch f {
			case FlagSideEffectFunction, FlagServerFile, FlagCodeBlock, FlagProcedureCall:
				b.worst(adapter.Irreversible)
				b.warn(fmt.Sprintf("#%d %s", i+1, lintRules[f].message))
			}
		}
	}
	locks, err := heldLocks(ctx, tx)
	if err != nil {
		return err
	}
	for rel, mode := range b.locks {
		if lockRank(mode) > lockRank(locks[rel]) {
			locks[rel] = mode
		}
	}
	var parts []string
	maxMode := ""
	for rel, mode := range locks {
		if lockRank(mode) < lockRank("ShareUpdateExclusiveLock") {
			continue
		}
		parts = append(parts, rel+"="+mode)
		if lockRank(mode) > lockRank(maxMode) {
			maxMode = mode
		}
	}
	if len(parts) > 0 {
		b.plan.Facts["locks"] = strings.Join(sorted(parts), ",")
		b.plan.Facts["max_lock"] = maxMode
		b.plan.Effects = append(b.plan.Effects, adapter.Effect{Kind: "lock", Description: "locks taken: " + strings.Join(sorted(parts), ", ")})
		fmt.Fprintf(&b.diff, "locks: %s\n", strings.Join(sorted(parts), ", "))
	}
	b.plan.Facts["dry_run_ms"] = fmt.Sprint(totalMs)
	return nil
}

// unsafeToDryRun reports statements whose effects a rollback cannot undo.
func unsafeToDryRun(s *Stmt) bool {
	return s.HasFlag(FlagSideEffectFunction) || s.HasFlag(FlagServerFile) || s.HasFlag(FlagCodeBlock) || s.HasFlag(FlagProcedureCall)
}

func describePGError(n int, s *Stmt, err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "55P03":
			return fmt.Errorf("statement #%d (%s) could not get its locks within the dry-run lock timeout; another session holds them: %s", n, s.Kind, pe.Message)
		case "57014":
			return fmt.Errorf("statement #%d (%s) exceeded the dry-run statement timeout; rehearse it on a copy instead", n, s.Kind)
		}
		return fmt.Errorf("statement #%d (%s) failed: %s (SQLSTATE %s)", n, s.Kind, pe.Message, pe.Code)
	}
	return fmt.Errorf("statement #%d (%s): %w", n, s.Kind, err)
}

func (b *planBuilder) planDML(ctx context.Context, tx pgx.Tx, i int, s *Stmt, step *planStep) error {
	if s.Kind == "COPY" {
		step.Reversibility = adapter.CheckpointOnly
		b.warn(fmt.Sprintf("#%d COPY FROM is not captured; undo needs a checkpoint", i+1))
		_, err := tx.Exec(ctx, s.Text)
		return err
	}
	rel, err := resolveRel(ctx, tx, QuoteRel(s.Rel))
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("relation %s does not exist", RelName(s.Rel))
	}
	step.Rel = rel.display()
	mode, execSQL, preSQL, err := dmlStatements(s, b.sv)
	if err != nil {
		return err
	}
	pk, err := primaryKey(ctx, tx, rel.OID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT cg_step"); err != nil {
		return err
	}
	c, err := runDML(ctx, tx, mode, execSQL, preSQL, s.Kind, pk)
	if errors.Is(err, errNoPairing) {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT cg_step"); err != nil {
			return err
		}
		mode, execSQL, preSQL = CaptureNone, s.Text, ""
		c, err = runDML(ctx, tx, mode, execSQL, "", s.Kind, pk)
	}
	if err != nil {
		return err
	}
	step.Capture, step.ExecSQL, step.PreSQL, step.Rows, step.BeforeHash = mode, execSQL, preSQL, c.Count, c.BeforeHash
	b.plan.BlastRows += c.Count

	// Reversibility of this statement.
	switch {
	case mode == CaptureNone:
		step.Reversibility = adapter.CheckpointOnly
		why := "statement shape"
		if b.sv < 180000 {
			why = "Postgres < 18 and the statement is not a simple single-table statement with a primary key"
		}
		b.warn(fmt.Sprintf("#%d row images could not be captured (%s); undo needs a checkpoint", i+1, why))
	case c.Count > int64(b.a.opts.MaxUndoRows):
		step.Reversibility = adapter.CheckpointOnly
		b.warn(fmt.Sprintf("#%d touches %d rows, above the undo image cap (%d); undo needs a checkpoint or branch", i+1, c.Count, b.a.opts.MaxUndoRows))
	}
	if rel.Kind == "f" {
		step.Reversibility = adapter.Worst(step.Reversibility, adapter.CheckpointOnly)
		b.warn(fmt.Sprintf("#%d %s is a foreign table; remote effects are outside Changegate's control", i+1, rel.display()))
	}
	trigs, external, err := userTriggers(ctx, tx, rel.OID)
	if err != nil {
		return err
	}
	if len(trigs) > 0 {
		step.Reversibility = adapter.Worst(step.Reversibility, adapter.Compensatable)
		b.warn(fmt.Sprintf("#%d %s has triggers (%s); their effects on other tables are not undone", i+1, rel.display(), strings.Join(trigs, ", ")))
		if external {
			b.warn(fmt.Sprintf("#%d a trigger on %s may have effects outside the database (side effects possible, even in the dry run)", i+1, rel.display()))
		}
	}
	if fks, err := cascadingFKs(ctx, tx, rel.OID); err != nil {
		return err
	} else if len(fks) > 0 && c.Count > 0 {
		for _, fk := range fks {
			hit := false
			if (s.Kind == "DELETE" || s.Kind == "MERGE") && cascades(fk.OnDelete) && len(c.Deleted) > 0 {
				hit = true
			}
			if cascades(fk.OnUpdate) && keysChanged(c.Updated, fk.Cols) {
				hit = true
			}
			if mode == CaptureNone && (cascades(fk.OnDelete) || cascades(fk.OnUpdate)) {
				hit = true
			}
			if hit {
				step.Reversibility = adapter.Worst(step.Reversibility, adapter.CheckpointOnly)
				b.warn(fmt.Sprintf("#%d cascades through foreign key %s to %s; cascaded rows are not captured", i+1, fk.Name, fk.Child))
			}
		}
	}
	verb := map[string]string{"INSERT": "rows-inserted", "UPDATE": "rows-updated", "DELETE": "rows-deleted", "MERGE": "rows-merged"}[s.Kind]
	b.plan.Effects = append(b.plan.Effects, adapter.Effect{Kind: verb, Object: rel.display(), Rows: c.Count,
		Description: fmt.Sprintf("%s %s: %d rows", s.Kind, rel.display(), c.Count)})
	b.renderDML(i, s, rel, pk, c)
	return nil
}

func keysChanged(ups []pair, cols []string) bool {
	for _, u := range ups {
		var bm, am map[string]json.RawMessage
		if json.Unmarshal(u.B, &bm) != nil || json.Unmarshal(u.A, &am) != nil {
			return true
		}
		for _, c := range cols {
			if string(bm[c]) != string(am[c]) {
				return true
			}
		}
	}
	return false
}

func (b *planBuilder) planDDL(ctx context.Context, tx pgx.Tx, i int, s *Stmt, step *planStep) error {
	st := classifyDDL(s)
	step.Soft, step.Reversibility = st.Soft, st.Reversibility
	var rel *relRef
	var err error
	if s.Rel != nil {
		if rel, err = resolveRel(ctx, tx, QuoteRel(s.Rel)); err != nil {
			return err
		}
		if step.Signature, err = relSignature(ctx, tx, QuoteRel(s.Rel)); err != nil {
			return err
		}
	}
	var fileBefore uint32
	var rows int64
	exact := false
	if rel != nil && (rel.Kind == "r" || rel.Kind == "p") {
		if fileBefore, err = relfilenode(ctx, tx, rel.OID); err != nil {
			return err
		}
		if rows, exact, err = estimateRows(ctx, tx, rel); err != nil {
			return err
		}
	}
	// Validate the statement exactly as written (not the soft rewrite).
	if _, err := tx.Exec(ctx, s.Text); err != nil {
		return err
	}
	fmt.Fprintf(&b.diff, "#%d %s\n", i+1, firstLine(s.Text))
	if rel != nil && (rel.Kind == "r" || rel.Kind == "p") {
		approx := "~"
		if exact {
			approx = ""
		}
		after, err := relfilenode(ctx, tx, rel.OID)
		if err != nil {
			// Dropped by this statement: pg_locks can no longer name it.
			b.locks[rel.display()] = "AccessExclusiveLock"
		}
		rewrite := err == nil && after != fileBefore && after != 0
		desc := fmt.Sprintf("%s on %s (%s%d rows)", s.Kind, rel.display(), approx, rows)
		if rewrite {
			desc += "; rewrites the table"
			b.plan.Facts["rewrite"] = "true"
			b.warn(fmt.Sprintf("#%d rewrites %s (%s%d rows) while holding an ACCESS EXCLUSIVE lock", i+1, rel.display(), approx, rows))
		}
		b.plan.Effects = append(b.plan.Effects, adapter.Effect{Kind: strings.ToLower(strings.ReplaceAll(s.Kind, " ", "-")), Object: rel.display(), Rows: rows, Description: desc})
		fmt.Fprintf(&b.diff, "   %s\n", desc)
		if s.Kind == "TRUNCATE" || (s.Kind == "DROP TABLE") {
			b.plan.BlastRows += rows
		}
	} else {
		b.plan.Effects = append(b.plan.Effects, adapter.Effect{Kind: strings.ToLower(strings.ReplaceAll(s.Kind, " ", "-")), Object: strings.Join(s.Objects, ","), Description: s.Kind + " " + strings.Join(s.Objects, ", ")})
	}
	for _, n := range st.Notes {
		fmt.Fprintf(&b.diff, "   %s\n", n)
	}
	for _, f := range Lint([]*Stmt{s}) {
		if f.Severity != "info" {
			b.warn(fmt.Sprintf("#%d %s", i+1, f.Message))
		}
	}
	return nil
}

func (b *planBuilder) planOther(ctx context.Context, tx pgx.Tx, i int, s *Stmt, step *planStep) error {
	st := classifyDDL(s)
	step.Reversibility = st.Reversibility
	if s.Kind == "SET" {
		step.Reversibility = adapter.Reversible
	}
	if st.Reversibility == adapter.Irreversible {
		b.warn(fmt.Sprintf("#%d %s cannot be previewed safely and was not executed in the dry run", i+1, s.Kind))
		fmt.Fprintf(&b.diff, "#%d %s (not dry-run: arbitrary code)\n", i+1, firstLine(s.Text))
		return nil
	}
	if _, err := tx.Exec(ctx, s.Text); err != nil {
		return err
	}
	fmt.Fprintf(&b.diff, "#%d %s\n", i+1, firstLine(s.Text))
	return nil
}

func (b *planBuilder) finish() (*adapter.Plan, error) {
	h := sha256.New()
	for _, s := range b.data.Steps {
		fmt.Fprintf(h, "%s|%s|%d;", s.BeforeHash, s.Signature, s.Rows)
	}
	b.plan.PreconditionHash = hex.EncodeToString(h.Sum(nil))
	raw, err := json.Marshal(b.data)
	if err != nil {
		return nil, err
	}
	b.plan.AdapterData = raw
	if b.plan.Reversibility == "" {
		b.plan.Reversibility = adapter.Reversible
	}
	header := fmt.Sprintf("plan %s · %s (%s) · %s · %d rows · %s\n", b.plan.ID, b.t.Name, b.t.Env, b.plan.Reversibility, b.plan.BlastRows, b.plan.RehearsalTier)
	b.plan.Diff = header + b.diff.String()
	if len(b.plan.Warnings) > 0 {
		b.plan.Diff += "warnings:\n"
		for _, w := range b.plan.Warnings {
			b.plan.Diff += "  ! " + w + "\n"
		}
	}
	b.plan.Hash = adapter.HashPlan(b.plan)
	return b.plan, nil
}

func trashNamer(planID string, step int) func(string) string {
	short := strings.TrimPrefix(planID, "p_")
	if len(short) > 6 {
		short = short[:6]
	}
	return func(name string) string {
		n := fmt.Sprintf("%s%s%d_%s", TrashPrefix, short, step, name)
		if len(n) > 63 {
			n = n[:63]
		}
		return n
	}
}

// Apply implements adapter.Adapter. It re-runs each statement, aborts on
// drift, and returns before-images and inverses as the undo handle.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var pd planData
	if err := json.Unmarshal(p.AdapterData, &pd); err != nil {
		return nil, fmt.Errorf("plan data: %w", err)
	}
	_, stmts, err := Classify(p.Operation.Payload)
	if err != nil {
		return nil, err
	}
	if len(stmts) != len(pd.Steps) {
		return nil, errors.New("plan does not match its operation")
	}
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	uh := &undoHandle{V: 1, NonTransactional: pd.NonTransactional}
	rc := &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, Reversibility: p.Reversibility}
	var out []string

	if pd.NonTransactional {
		s := stmts[0]
		res, err := applyDDL(ctx, conn.Conn(), s, classifyDDL(s), trashNamer(p.ID, 0))
		if err != nil {
			return nil, describePGError(1, s, err)
		}
		uh.Steps = append(uh.Steps, undoStep{Index: 0, Kind: "ddl", DDL: res})
		out = append(out, s.Kind)
	} else {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := setLocal(ctx, tx, 0, a.opts.ApplyLockTimeout); err != nil {
			return nil, err
		}
		for i, s := range stmts {
			step := pd.Steps[i]
			switch s.Category {
			case CatDML:
				if s.Kind == "COPY" {
					tag, err := tx.Exec(ctx, s.Text)
					if err != nil {
						return nil, describePGError(i+1, s, err)
					}
					rc.RowsAffected += tag.RowsAffected()
					uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "none"})
					continue
				}
				rel, err := resolveRel(ctx, tx, QuoteRel(s.Rel))
				if err != nil {
					return nil, err
				}
				if rel == nil {
					return nil, &adapter.ConflictError{Reason: "relation " + RelName(s.Rel) + " no longer exists; re-plan required"}
				}
				pk, err := primaryKey(ctx, tx, rel.OID)
				if err != nil {
					return nil, err
				}
				c, err := runDML(ctx, tx, step.Capture, step.ExecSQL, step.PreSQL, s.Kind, pk)
				if err != nil {
					return nil, describePGError(i+1, s, err)
				}
				if c.BeforeHash != step.BeforeHash {
					return nil, &adapter.ConflictError{Reason: "data changed since plan; re-plan required",
						Conflicts: []string{fmt.Sprintf("#%d %s %s: planned %d rows, now %d (or row contents differ)", i+1, s.Kind, rel.display(), step.Rows, c.Count)}}
				}
				rc.RowsAffected += c.Count
				if step.Capture == CaptureNone {
					uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "none"})
					continue
				}
				cols, err := columns(ctx, tx, rel.OID)
				if err != nil {
					return nil, err
				}
				u := &dmlUndo{Table: rel.qualified(), Display: rel.display(), PK: pk, capture: *c}
				for _, col := range cols {
					if col.Generated {
						continue
					}
					u.InsertCols = append(u.InsertCols, col.Name)
					if col.Identity != "a" { // GENERATED ALWAYS identities cannot be updated (and never change)
						u.SetCols = append(u.SetCols, col.Name)
					}
				}
				uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "dml", DML: u})
				out = append(out, fmt.Sprintf("%s %d", s.Kind, c.Count))
			case CatDDL:
				if step.Signature != "" {
					sig, err := relSignature(ctx, tx, QuoteRel(s.Rel))
					if err != nil {
						return nil, err
					}
					if sig != step.Signature {
						return nil, &adapter.ConflictError{Reason: "schema changed since plan; re-plan required",
							Conflicts: []string{fmt.Sprintf("#%d %s: %s changed", i+1, s.Kind, RelName(s.Rel))}}
					}
				}
				res, err := applyDDL(ctx, tx, s, classifyDDL(s), trashNamer(p.ID, i))
				if err != nil {
					return nil, describePGError(i+1, s, err)
				}
				uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "ddl", DDL: res})
				out = append(out, s.Kind)
			default:
				if classifyDDL(s).Reversibility == adapter.Irreversible && s.Access != adapter.Read {
					if _, err := tx.Exec(ctx, s.Text); err != nil {
						return nil, describePGError(i+1, s, err)
					}
					uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "none"})
					continue
				}
				if _, err := tx.Exec(ctx, s.Text); err != nil {
					return nil, describePGError(i+1, s, err)
				}
				uh.Steps = append(uh.Steps, undoStep{Index: i, Kind: "none"})
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(uh)
	if err != nil {
		return nil, err
	}
	rc.UndoHandle = raw
	rc.AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rc.Output = strings.Join(out, "; ")
	for _, s := range uh.Steps {
		if s.DDL != nil {
			rc.Warnings = append(rc.Warnings, s.DDL.Notes...)
		}
	}
	return rc, nil
}

// Undo implements adapter.Adapter with guarded inverses in one transaction.
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	if r.Reversibility == adapter.CheckpointOnly || r.Reversibility == adapter.Irreversible {
		return nil, fmt.Errorf("change is %s; restore a checkpoint instead", r.Reversibility)
	}
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, fmt.Errorf("undo handle: %w", err)
	}
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setLocal(ctx, tx, 0, a.opts.ApplyLockTimeout); err != nil {
		return nil, err
	}
	var total int64
	var conflicts []string
	for i := len(uh.Steps) - 1; i >= 0; i-- {
		s := uh.Steps[i]
		switch s.Kind {
		case "dml":
			n, c, err := undoDML(ctx, tx, s.DML)
			if err != nil {
				return nil, err
			}
			total += n
			conflicts = append(conflicts, c...)
		case "ddl":
			for _, g := range s.DDL.Guards {
				var ok bool
				if err := tx.QueryRow(ctx, g.SQL).Scan(&ok); err != nil {
					return nil, err
				}
				if !ok {
					conflicts = append(conflicts, g.Reason)
				}
			}
			if len(conflicts) > 0 {
				break
			}
			for _, sql := range s.DDL.Inverse {
				if _, err := tx.Exec(ctx, "SAVEPOINT cg_inv"); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(ctx, sql); err != nil {
					var pe *pgconn.PgError
					if errors.As(err, &pe) {
						conflicts = append(conflicts, fmt.Sprintf("inverse %q failed: %s", firstLine(sql), pe.Message))
						_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT cg_inv")
						continue
					}
					return nil, err
				}
			}
		}
		if len(conflicts) > 0 {
			break
		}
	}
	if len(conflicts) > 0 {
		return nil, &adapter.ConflictError{Reason: "target changed since apply; undo refused (nothing was changed)", Conflicts: conflicts}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RowsAffected: total, Reversibility: adapter.Reversible, Output: fmt.Sprintf("undid %d steps", len(uh.Steps))}, nil
}

// Verify implements adapter.Adapter: after-images of inserted and updated
// rows must be present in the live target.
func (a *Adapter) Verify(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Verification, error) {
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	v := &adapter.Verification{Passed: true}
	for _, s := range uh.Steps {
		if s.Kind != "dml" {
			continue
		}
		var imgs []json.RawMessage
		imgs = append(imgs, s.DML.Inserted...)
		for _, u := range s.DML.Updated {
			imgs = append(imgs, u.A)
		}
		if len(imgs) == 0 {
			continue
		}
		if len(imgs) > 1000 {
			imgs = imgs[:1000]
		}
		var n int
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM jsonb_array_elements($1::jsonb) e
			WHERE EXISTS (SELECT 1 FROM %s t WHERE to_jsonb(t) = e)`, s.DML.Table), rawArray(imgs)).Scan(&n)
		if err != nil {
			return nil, err
		}
		ok := n == len(imgs)
		v.Checks = append(v.Checks, adapter.Check{Name: "after-images " + s.DML.Display, Passed: ok, Detail: fmt.Sprintf("%d of %d rows match", n, len(imgs))})
		v.Passed = v.Passed && ok
	}
	if len(v.Checks) == 0 {
		v.Checks = append(v.Checks, adapter.Check{Name: "applied", Passed: true, Detail: "transaction committed"})
	}
	return v, nil
}

// Rehearse implements adapter.Adapter: plan, apply, verify, undo and compare
// snapshots on a provided copy.
func (a *Adapter) Rehearse(ctx context.Context, copy adapter.Target, op adapter.Operation) (*adapter.RehearsalResult, error) {
	pool, err := a.pool(ctx, copy)
	if err != nil {
		return nil, err
	}
	before, err := snapshotPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	res := &adapter.RehearsalResult{Tier: copy.Config["tier"], Passed: true}
	if res.Tier == "" {
		res.Tier = "copy"
	}
	add := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, adapter.Check{Name: name, Passed: ok, Detail: detail})
		res.Passed = res.Passed && ok
	}
	p, err := a.Plan(ctx, copy, op)
	if err != nil {
		add("plan", false, err.Error())
		return res, nil
	}
	res.Effects = p.Effects
	rc, err := a.Apply(ctx, copy, p)
	if err != nil {
		add("apply", false, err.Error())
		return res, nil
	}
	add("apply", true, rc.Output)
	v, err := a.Verify(ctx, copy, p, rc)
	if err != nil || !v.Passed {
		add("verify", false, fmt.Sprint(err))
	} else {
		add("verify", true, "")
	}
	if rc.Reversibility == adapter.Reversible || rc.Reversibility == adapter.Compensatable {
		if _, err := a.Undo(ctx, copy, p, rc); err != nil {
			add("undo", false, err.Error())
			return res, nil
		}
		after, err := snapshotPool(ctx, pool)
		if err != nil {
			return nil, err
		}
		d := DiffSnapshots(before, after)
		add("compare", len(d) == 0, strings.Join(d, "; "))
	}
	res.Report = p.Diff
	return res, nil
}

func snapshotPool(ctx context.Context, pool *pgxpool.Pool) (*Snapshot, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	return TakeSnapshot(ctx, conn.Conn(), true, nil)
}

func (a *Adapter) fingerprintPlan(ctx context.Context, t adapter.Target) (*adapter.Plan, error) {
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	s, err := snapshotPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	return &adapter.Plan{Facts: map[string]string{"fingerprint": s.Fingerprint()}}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
