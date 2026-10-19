// Package mysql is the Changegate adapter for MySQL 8 (InnoDB).
//
// DML is planned in a transaction that is always rolled back: target rows
// are pre-selected FOR UPDATE by the statement's own WHERE/ORDER BY/LIMIT,
// the statement runs, and changed rows are read back by primary key. Values
// are captured losslessly as HEX(CAST(col AS BINARY)) and a SHA-256 row
// hash computed in SQL; apply aborts on drift, and undo touches a row only
// if its row hash still equals the after-image's.
//
// MySQL DDL commits implicitly and cannot be dry-run, so DDL plans are
// static (validated against information_schema). DROP TABLE and TRUNCATE
// are soft (renamed or copied into the _changegate_trash database), DROP
// COLUMN is a soft rename restored exactly from SHOW CREATE TABLE.
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/srawlin/changegate/pkg/adapter"
)

// TrashDB holds soft-dropped tables.
const TrashDB = "_changegate_trash"

// Adapter implements adapter.Adapter for MySQL.
type Adapter struct {
	adapter.Unimplemented
	mu  sync.Mutex
	dbs map[string]*sql.DB
	// MaxUndoRows caps captured rows per change (default 50,000).
	MaxUndoRows int
}

// New returns a MySQL adapter.
func New() *Adapter { return &Adapter{dbs: map[string]*sql.DB{}, MaxUndoRows: 50000} }

var _ adapter.Adapter = (*Adapter)(nil)

// Close closes connection pools.
func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, d := range a.dbs {
		_ = d.Close()
		delete(a.dbs, k)
	}
}

func (a *Adapter) db(t adapter.Target) (*sql.DB, error) {
	dsn := t.Config["dsn"]
	if dsn == "" {
		return nil, fmt.Errorf("target %q has no dsn", t.Name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if d, ok := a.dbs[dsn]; ok {
		return d, nil
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MultiStatements = false
	cfg.ParseTime = false
	d, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(4)
	a.dbs[dsn] = d
	return d, nil
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "mysql", Version: "0.1.0", Kinds: []string{"sql"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo}}, nil
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, _ adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	c, _, err := Classify(op.Payload)
	return c, err
}

func qi(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

type column struct {
	Name      string `json:"name"`
	DataType  string `json:"data_type"`
	Generated bool   `json:"generated,omitempty"`
}

type tableInfo struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []column `json:"columns"`
	PK      []string `json:"pk"`
}

func (t *tableInfo) q() string { return qi(t.Schema) + "." + qi(t.Name) }

func loadTable(ctx context.Context, q queryer, schema, name string) (*tableInfo, error) {
	if schema == "" {
		if err := q.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
			return nil, err
		}
	}
	ti := &tableInfo{Schema: schema, Name: name}
	rows, err := q.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, EXTRA FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c column
		var extra string
		if err := rows.Scan(&c.Name, &c.DataType, &extra); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.Generated = strings.Contains(strings.ToUpper(extra), "GENERATED")
		ti.Columns = append(ti.Columns, c)
	}
	_ = rows.Close()
	if len(ti.Columns) == 0 {
		return nil, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY ORDINAL_POSITION`, schema, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		ti.PK = append(ti.PK, c)
	}
	return ti, rows.Err()
}

func (t *tableInfo) col(name string) column {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return column{Name: name}
}

// valueExpr turns a hex-captured value back into a column value.
func valueExpr(c column) string {
	switch c.DataType {
	case "json":
		return "CAST(CONVERT(UNHEX(?) USING utf8mb4) AS JSON)"
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "bit", "geometry", "point", "linestring", "polygon":
		return "UNHEX(?)"
	}
	return "CONVERT(UNHEX(?) USING utf8mb4)"
}

func hexExpr(prefix, col string) string { return "HEX(CAST(" + prefix + qi(col) + " AS BINARY))" }

func (t *tableInfo) rowHashExpr(prefix string) string {
	var parts []string
	for _, c := range t.Columns {
		parts = append(parts, "IFNULL("+hexExpr(prefix, c.Name)+",'~')")
	}
	return "SHA2(CONCAT_WS(','," + strings.Join(parts, ",") + "),256)"
}

func (t *tableInfo) selectList(prefix string) string {
	var parts []string
	for _, c := range t.Columns {
		parts = append(parts, hexExpr(prefix, c.Name))
	}
	return strings.Join(parts, ", ") + ", " + t.rowHashExpr(prefix)
}

// row is a captured row: hex values by column, and its row hash.
type row struct {
	Vals map[string]*string `json:"vals"`
	Hash string             `json:"hash"`
}

func (t *tableInfo) scanRows(rows *sql.Rows) ([]row, error) {
	defer func() { _ = rows.Close() }()
	var out []row
	for rows.Next() {
		dest := make([]any, len(t.Columns)+1)
		vals := make([]sql.NullString, len(t.Columns)+1)
		for i := range dest {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r := row{Vals: map[string]*string{}, Hash: vals[len(t.Columns)].String}
		for i, c := range t.Columns {
			if vals[i].Valid {
				v := vals[i].String
				r.Vals[c.Name] = &v
			} else {
				r.Vals[c.Name] = nil
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *tableInfo) pkWhere(r row, prefix string) (string, []any) {
	var conds []string
	var args []any
	for _, k := range t.PK {
		conds = append(conds, prefix+qi(k)+" = "+valueExpr(t.col(k)))
		args = append(args, deref(r.Vals[k]))
	}
	return strings.Join(conds, " AND "), args
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (t *tableInfo) fetchByPK(ctx context.Context, q queryer, keys []row) ([]row, error) {
	var out []row
	for _, k := range keys {
		w, args := t.pkWhere(k, "")
		rows, err := q.QueryContext(ctx, "SELECT "+t.selectList("")+" FROM "+t.q()+" WHERE "+w, args...)
		if err != nil {
			return nil, err
		}
		rs, err := t.scanRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// capture is the observed effect of one DML statement.
type capture struct {
	Table      *tableInfo `json:"table"`
	Inserted   []row      `json:"inserted,omitempty"`
	Deleted    []row      `json:"deleted,omitempty"`
	Before     []row      `json:"before,omitempty"` // updated rows, before
	After      []row      `json:"after,omitempty"`  // updated rows, after (same order)
	Count      int64      `json:"count"`
	BeforeHash string     `json:"before_hash"`
	Captured   bool       `json:"captured"`
}

func (c *capture) hash() {
	var hs []string
	for _, r := range append(append([]row{}, c.Deleted...), c.Before...) {
		hs = append(hs, r.Hash)
	}
	sort.Strings(hs)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s", c.Count, len(c.Inserted), strings.Join(hs, ","))))
	c.BeforeHash = hex.EncodeToString(sum[:])
}

// preselect builds SELECT ... FOR UPDATE matching an UPDATE/DELETE.
func preselect(s *Stmt, t *tableInfo) (string, error) {
	var where, order, limit string
	var err error
	switch n := s.Node.(type) {
	case *ast.UpdateStmt:
		if n.Where != nil {
			where, err = Restore(n.Where)
		}
		if err == nil && n.Order != nil {
			order, err = Restore(n.Order)
		}
		if err == nil && n.Limit != nil {
			limit, err = Restore(n.Limit)
		}
	case *ast.DeleteStmt:
		if n.Where != nil {
			where, err = Restore(n.Where)
		}
		if err == nil && n.Order != nil {
			order, err = Restore(n.Order)
		}
		if err == nil && n.Limit != nil {
			limit, err = Restore(n.Limit)
		}
	}
	if err != nil {
		return "", err
	}
	prefix, from := "", t.q()
	if s.Alias != "" {
		prefix, from = qi(s.Alias)+".", t.q()+" AS "+qi(s.Alias)
	}
	q := "SELECT " + t.selectList(prefix) + " FROM " + from
	if where != "" {
		q += " WHERE " + where
	}
	if order != "" {
		q += " " + order
	}
	if limit != "" {
		q += " " + limit
	}
	return q + " FOR UPDATE", nil
}

// insertedKeys returns the primary keys an INSERT ... VALUES created.
func insertedKeys(ctx context.Context, q queryer, s *Stmt, t *tableInfo, res sql.Result) ([]row, bool) {
	n, _ := res.RowsAffected()
	ins := s.Node.(*ast.InsertStmt)
	if len(t.PK) != 1 || ins.Select != nil || len(ins.OnDuplicate) > 0 || ins.IsReplace || len(ins.Lists) == 0 {
		return nil, false
	}
	pk := t.PK[0]
	idx := -1
	for i, c := range ins.Columns {
		if strings.EqualFold(c.Name.O, pk) {
			idx = i
		}
	}
	var keys []row
	if idx >= 0 {
		for _, l := range ins.Lists {
			if idx >= len(l) {
				return nil, false
			}
			var v string
			if err := q.QueryRowContext(ctx, "SELECT HEX(CAST(("+mustRestore(l[idx])+") AS BINARY))").Scan(&v); err != nil {
				return nil, false
			}
			keys = append(keys, row{Vals: map[string]*string{pk: &v}})
		}
	} else {
		first, err := res.LastInsertId()
		if err != nil || first == 0 {
			return nil, false
		}
		for i := int64(0); i < n; i++ {
			v := strings.ToUpper(hex.EncodeToString([]byte(fmt.Sprint(first + i))))
			keys = append(keys, row{Vals: map[string]*string{pk: &v}})
		}
	}
	return keys, int64(len(keys)) == n
}

func mustRestore(n ast.Node) string { s, _ := Restore(n); return s }

// runDML executes one DML statement in tx and captures its images.
func (a *Adapter) runDML(ctx context.Context, q queryer, s *Stmt) (*capture, error) {
	t, err := loadTable(ctx, q, s.Schema, s.Table)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("table %s does not exist", s.Table)
	}
	c := &capture{Table: t}
	simple := len(t.PK) > 0 && !s.HasFlag("multi-table-dml") && s.Table != ""
	switch s.Kind {
	case "UPDATE", "DELETE":
		var before []row
		if simple {
			pre, err := preselect(s, t)
			if err != nil {
				return nil, err
			}
			rows, err := q.QueryContext(ctx, pre)
			if err != nil {
				return nil, err
			}
			if before, err = t.scanRows(rows); err != nil {
				return nil, err
			}
		}
		res, err := q.ExecContext(ctx, s.Text)
		if err != nil {
			return nil, err
		}
		c.Count, _ = res.RowsAffected()
		if !simple {
			break
		}
		if s.Kind == "DELETE" {
			c.Deleted, c.Captured = before, int64(len(before)) == c.Count
			break
		}
		after, err := t.fetchByPK(ctx, q, before)
		if err != nil {
			return nil, err
		}
		if len(after) == len(before) {
			c.Before, c.After, c.Captured = before, after, true
			// UPDATE's RowsAffected counts only rows whose values changed.
			var b, aa []row
			for i := range before {
				if before[i].Hash != after[i].Hash {
					b, aa = append(b, before[i]), append(aa, after[i])
				}
			}
			c.Before, c.After = b, aa
			c.Captured = int64(len(b)) == c.Count
		}
	case "INSERT", "REPLACE":
		res, err := q.ExecContext(ctx, s.Text)
		if err != nil {
			return nil, err
		}
		c.Count, _ = res.RowsAffected()
		if keys, ok := insertedKeys(ctx, q, s, t, res); ok && s.Kind == "INSERT" {
			if c.Inserted, err = t.fetchByPK(ctx, q, keys); err != nil {
				return nil, err
			}
			c.Captured = int64(len(c.Inserted)) == c.Count
		}
	default:
		res, err := q.ExecContext(ctx, s.Text)
		if err != nil {
			return nil, err
		}
		c.Count, _ = res.RowsAffected()
	}
	if !c.Captured {
		c.Inserted, c.Deleted, c.Before, c.After = nil, nil, nil, nil
	}
	c.hash()
	return c, nil
}

type planStep struct {
	SQL           string                `json:"sql"`
	Kind          string                `json:"kind"`
	Category      string                `json:"category"`
	Rows          int64                 `json:"rows"`
	BeforeHash    string                `json:"before_hash,omitempty"`
	Captured      bool                  `json:"captured,omitempty"`
	Signature     string                `json:"signature,omitempty"`
	Reversibility adapter.Reversibility `json:"reversibility"`
}

type planData struct {
	DDL   bool       `json:"ddl"`
	Steps []planStep `json:"steps"`
}

// ErrReadOnly is returned when asked to plan an operation that only reads.
var ErrReadOnly = errors.New("operation is read-only; use query instead of proposing a change")

func validate(stmts []*Stmt) (ddl bool, err error) {
	writes := 0
	for _, s := range stmts {
		if s.HasFlag("transaction-control") {
			return false, errors.New("transaction control is not allowed; each change runs in its own transaction")
		}
		if s.HasFlag("session-setting") {
			return false, errors.New("SET is not allowed in a change")
		}
		if s.Category == CatDDL {
			ddl = true
		}
		if s.Access != adapter.Read {
			writes++
		}
	}
	if writes == 0 {
		return false, ErrReadOnly
	}
	if ddl && len(stmts) > 1 {
		return false, errors.New("MySQL DDL commits implicitly: submit each DDL statement as its own change")
	}
	return ddl, nil
}

func tableSignature(ctx context.Context, q queryer, schema, name string) string {
	var tn, ddl string
	ref := qi(name)
	if schema != "" {
		ref = qi(schema) + "." + ref
	}
	if err := q.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&tn, &ddl); err != nil {
		return "absent"
	}
	// AUTO_INCREMENT counters change with every insert; they are not schema.
	ddl = autoIncRe(ddl)
	sum := sha256.Sum256([]byte(ddl))
	return hex.EncodeToString(sum[:8])
}

func autoIncRe(ddl string) string {
	out := []string{}
	for _, f := range strings.Fields(ddl) {
		if !strings.HasPrefix(f, "AUTO_INCREMENT=") {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	if op.Kind == "changegate.fingerprint" {
		return a.fingerprint(ctx, t)
	}
	cls, stmts, err := Classify(op.Payload)
	if err != nil {
		return nil, err
	}
	ddl, err := validate(stmts)
	if err != nil {
		return nil, err
	}
	d, err := a.db(t)
	if err != nil {
		return nil, err
	}
	p := &adapter.Plan{ID: adapter.NewID("p"), Adapter: "mysql", Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Reversibility: adapter.Reversible, Facts: map[string]string{}, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	pd := planData{DDL: ddl}
	var diff strings.Builder
	if ddl {
		s := stmts[0]
		st, notes, err := a.planDDL(ctx, d, s)
		if err != nil {
			return nil, err
		}
		p.RehearsalTier = "static"
		p.Reversibility = st.Reversibility
		pd.Steps = append(pd.Steps, st)
		fmt.Fprintf(&diff, "#1 %s\n", firstLine(s.Text))
		for _, n := range notes {
			fmt.Fprintf(&diff, "   %s\n", n)
		}
		p.Warnings = append(p.Warnings, "MySQL DDL commits implicitly and cannot be dry-run; this plan was checked against information_schema only")
		p.Destructive = s.Access == adapter.Destructive
		p.Effects = append(p.Effects, adapter.Effect{Kind: strings.ToLower(strings.ReplaceAll(s.Kind, " ", "-")), Object: s.Table, Description: s.Kind + " " + strings.Join(s.Objects, ", ")})
	} else {
		p.RehearsalTier = "live-dry-run"
		conn, err := d.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 2")
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		for i, s := range stmts {
			st := planStep{SQL: s.Text, Kind: s.Kind, Category: s.Category, Reversibility: adapter.Reversible}
			if s.Access == adapter.Read {
				st.Reversibility = ""
				pd.Steps = append(pd.Steps, st)
				continue
			}
			if s.HasFlag("side-effect-function") || s.HasFlag("server-file-access") || s.HasFlag("procedure-call") {
				st.Reversibility = adapter.Irreversible
				pd.Steps = append(pd.Steps, st)
				p.Reversibility = adapter.Irreversible
				p.Warnings = append(p.Warnings, fmt.Sprintf("#%d %s has effects outside the transaction; it was not executed in the dry run", i+1, s.Kind))
				continue
			}
			c, err := a.runDML(ctx, tx, s)
			if err != nil {
				return nil, fmt.Errorf("statement #%d (%s) failed in the dry run: %w", i+1, s.Kind, err)
			}
			st.Rows, st.BeforeHash, st.Captured = c.Count, c.BeforeHash, c.Captured
			if !c.Captured {
				st.Reversibility = adapter.CheckpointOnly
				p.Warnings = append(p.Warnings, fmt.Sprintf("#%d row images could not be captured (needs a single table with a primary key; no INSERT ... SELECT, upsert or REPLACE)", i+1))
			} else if c.Count > int64(a.MaxUndoRows) {
				st.Reversibility = adapter.CheckpointOnly
				p.Warnings = append(p.Warnings, fmt.Sprintf("#%d touches %d rows, above the undo cap", i+1, c.Count))
			}
			if s.Table != "" {
				if trig, casc := a.sideEffects(ctx, tx, c.Table); casc && (s.Kind == "DELETE" || s.Kind == "UPDATE") {
					st.Reversibility = adapter.Worst(st.Reversibility, adapter.CheckpointOnly)
					p.Warnings = append(p.Warnings, fmt.Sprintf("#%d cascades through foreign keys; cascaded rows are not captured", i+1))
				} else if trig {
					st.Reversibility = adapter.Worst(st.Reversibility, adapter.Compensatable)
					p.Warnings = append(p.Warnings, fmt.Sprintf("#%d %s has triggers; their effects are not undone", i+1, s.Table))
				}
			}
			p.Reversibility = adapter.Worst(p.Reversibility, st.Reversibility)
			p.BlastRows += c.Count
			if s.Access == adapter.Destructive {
				p.Destructive = true
			}
			p.Effects = append(p.Effects, adapter.Effect{Kind: "rows-" + strings.ToLower(s.Kind), Object: s.Table, Rows: c.Count, Description: fmt.Sprintf("%s %s: %d rows", s.Kind, s.Table, c.Count)})
			fmt.Fprintf(&diff, "#%d %s %s — %d rows\n", i+1, s.Kind, s.Table, c.Count)
			renderRows(&diff, c, t.Env)
			pd.Steps = append(pd.Steps, st)
		}
	}
	h := sha256.New()
	for _, s := range pd.Steps {
		fmt.Fprintf(h, "%s|%s|%d;", s.BeforeHash, s.Signature, s.Rows)
	}
	p.PreconditionHash = hex.EncodeToString(h.Sum(nil))
	p.AdapterData, _ = json.Marshal(pd)
	p.Diff = fmt.Sprintf("plan %s · %s (%s) · %s · %d rows · %s\n%s", p.ID, t.Name, t.Env, p.Reversibility, p.BlastRows, p.RehearsalTier, diff.String())
	if len(p.Warnings) > 0 {
		p.Diff += "warnings:\n"
		for _, w := range p.Warnings {
			p.Diff += "  ! " + w + "\n"
		}
	}
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

func (a *Adapter) sideEffects(ctx context.Context, q queryer, t *tableInfo) (triggers, cascades bool) {
	var n int
	if q.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ?`, t.Schema, t.Name).Scan(&n) == nil && n > 0 {
		triggers = true
	}
	if q.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE UNIQUE_CONSTRAINT_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?
		AND (DELETE_RULE IN ('CASCADE','SET NULL','SET DEFAULT') OR UPDATE_RULE IN ('CASCADE','SET NULL','SET DEFAULT'))`, t.Schema, t.Name).Scan(&n) == nil && n > 0 {
		cascades = true
	}
	return
}

func unhexStr(s *string) string {
	if s == nil {
		return "NULL"
	}
	b, err := hex.DecodeString(*s)
	if err != nil {
		return *s
	}
	v := string(b)
	if len(v) > 40 {
		v = v[:37] + "..."
	}
	return fmt.Sprintf("%q", v)
}

func renderRows(b *strings.Builder, c *capture, env string) {
	values := env == adapter.EnvPersonal
	key := func(r row) string {
		var parts []string
		for _, k := range c.Table.PK {
			if values {
				parts = append(parts, k+"="+unhexStr(r.Vals[k]))
			} else {
				sum := sha256.Sum256([]byte(deref2(r.Vals[k])))
				parts = append(parts, k+"=h:"+hex.EncodeToString(sum[:4]))
			}
		}
		return strings.Join(parts, ",")
	}
	shown := 0
	for i := range c.Before {
		if shown == 10 {
			break
		}
		var ch []string
		for _, col := range c.Table.Columns {
			bv, av := c.Before[i].Vals[col.Name], c.After[i].Vals[col.Name]
			if deref2(bv) != deref2(av) || (bv == nil) != (av == nil) {
				if values {
					ch = append(ch, fmt.Sprintf("%s: %s → %s", col.Name, unhexStr(bv), unhexStr(av)))
				} else {
					ch = append(ch, col.Name)
				}
			}
		}
		fmt.Fprintf(b, "   ~ %s  %s\n", key(c.Before[i]), strings.Join(ch, "; "))
		shown++
	}
	for _, r := range c.Deleted {
		if shown == 10 {
			break
		}
		fmt.Fprintf(b, "   - %s\n", key(r))
		shown++
	}
	for _, r := range c.Inserted {
		if shown == 10 {
			break
		}
		fmt.Fprintf(b, "   + %s\n", key(r))
		shown++
	}
	if int64(shown) < c.Count && c.Captured {
		fmt.Fprintf(b, "   … %d more\n", c.Count-int64(shown))
	}
}

func deref2(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

type undoStep struct {
	Kind    string   `json:"kind"` // dml, ddl, none
	DML     *capture `json:"dml,omitempty"`
	Inverse []string `json:"inverse,omitempty"`
	Guards  []string `json:"guards,omitempty"` // queries returning 1 when the inverse may run
}

type undoHandle struct {
	Steps []undoStep `json:"steps"`
}

// Apply implements adapter.Adapter.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var pd planData
	if err := json.Unmarshal(p.AdapterData, &pd); err != nil {
		return nil, err
	}
	_, stmts, err := Classify(p.Operation.Payload)
	if err != nil {
		return nil, err
	}
	d, err := a.db(t)
	if err != nil {
		return nil, err
	}
	rc := &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, Reversibility: p.Reversibility}
	var uh undoHandle
	if pd.DDL {
		step, err := a.applyDDL(ctx, d, stmts[0], pd.Steps[0], p.ID)
		if err != nil {
			return nil, err
		}
		uh.Steps = append(uh.Steps, step)
		rc.Output = stmts[0].Kind
	} else {
		conn, err := d.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 5")
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		for i, s := range stmts {
			if s.Access == adapter.Read {
				uh.Steps = append(uh.Steps, undoStep{Kind: "none"})
				continue
			}
			if pd.Steps[i].Reversibility == adapter.Irreversible {
				if _, err := tx.ExecContext(ctx, s.Text); err != nil {
					return nil, err
				}
				uh.Steps = append(uh.Steps, undoStep{Kind: "none"})
				continue
			}
			c, err := a.runDML(ctx, tx, s)
			if err != nil {
				return nil, fmt.Errorf("statement #%d (%s): %w", i+1, s.Kind, err)
			}
			if c.BeforeHash != pd.Steps[i].BeforeHash {
				return nil, &adapter.ConflictError{Reason: "data changed since plan; re-plan required",
					Conflicts: []string{fmt.Sprintf("#%d %s %s: planned %d rows, now %d (or row contents differ)", i+1, s.Kind, s.Table, pd.Steps[i].Rows, c.Count)}}
			}
			rc.RowsAffected += c.Count
			if c.Captured {
				uh.Steps = append(uh.Steps, undoStep{Kind: "dml", DML: c})
			} else {
				uh.Steps = append(uh.Steps, undoStep{Kind: "none"})
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		rc.Output = fmt.Sprintf("%d rows", rc.RowsAffected)
	}
	rc.UndoHandle, _ = json.Marshal(uh)
	rc.AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return rc, nil
}

func (t *tableInfo) settable() []column {
	var out []column
	for _, c := range t.Columns {
		if !c.Generated {
			out = append(out, c)
		}
	}
	return out
}

// undoDML reverses one captured statement, guarded by row hashes.
func undoDML(ctx context.Context, q queryer, c *capture) ([]string, error) {
	t := c.Table
	var conflicts []string
	for _, r := range c.Inserted {
		w, args := t.pkWhere(r, "")
		res, err := q.ExecContext(ctx, "DELETE FROM "+t.q()+" WHERE "+w+" AND "+t.rowHashExpr("")+" = ?", append(args, r.Hash)...)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			conflicts = append(conflicts, t.Name+": an inserted row was changed or removed since apply")
		}
	}
	for i, after := range c.After {
		before := c.Before[i]
		var sets []string
		var args []any
		for _, col := range t.settable() {
			sets = append(sets, qi(col.Name)+" = "+valueExpr(col))
			args = append(args, deref(before.Vals[col.Name]))
		}
		w, wargs := t.pkWhere(after, "")
		args = append(append(args, wargs...), after.Hash)
		res, err := q.ExecContext(ctx, "UPDATE "+t.q()+" SET "+strings.Join(sets, ", ")+" WHERE "+w+" AND "+t.rowHashExpr("")+" = ?", args...)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			conflicts = append(conflicts, t.Name+": an updated row was changed since apply")
		}
	}
	for _, r := range c.Deleted {
		var cols, vals []string
		var args []any
		for _, col := range t.settable() {
			cols, vals = append(cols, qi(col.Name)), append(vals, valueExpr(col))
			args = append(args, deref(r.Vals[col.Name]))
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO "+t.q()+" ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(vals, ", ")+")", args...); err != nil {
			var me *mysqldrv.MySQLError
			if errors.As(err, &me) && (me.Number == 1062 || me.Number == 1452) {
				conflicts = append(conflicts, t.Name+": a deleted row cannot be restored: "+me.Message)
				continue
			}
			return nil, err
		}
	}
	return conflicts, nil
}

// Undo implements adapter.Adapter.
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	if r.Reversibility == adapter.CheckpointOnly || r.Reversibility == adapter.Irreversible {
		return nil, fmt.Errorf("change is %s; restore a backup instead", r.Reversibility)
	}
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	d, err := a.db(t)
	if err != nil {
		return nil, err
	}
	for _, s := range uh.Steps {
		if s.Kind != "ddl" {
			continue
		}
		for _, g := range s.Guards {
			var ok int
			if err := d.QueryRowContext(ctx, g).Scan(&ok); err != nil || ok != 1 {
				return nil, &adapter.ConflictError{Reason: "target changed since apply; undo refused (nothing was changed)", Conflicts: []string{g}}
			}
		}
		for _, sqlText := range s.Inverse {
			if _, err := d.ExecContext(ctx, sqlText); err != nil {
				return nil, fmt.Errorf("inverse %q: %w", firstLine(sqlText), err)
			}
		}
		return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano), Reversibility: adapter.Reversible, Output: "DDL reversed"}, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var conflicts []string
	for i := len(uh.Steps) - 1; i >= 0; i-- {
		if uh.Steps[i].Kind != "dml" {
			continue
		}
		c, err := undoDML(ctx, tx, uh.Steps[i].DML)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, c...)
	}
	if len(conflicts) > 0 {
		return nil, &adapter.ConflictError{Reason: "target changed since apply; undo refused (nothing was changed)", Conflicts: conflicts}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano), Reversibility: adapter.Reversible, Output: "undone"}, nil
}

// fingerprint hashes the current database's schema and table contents (conformance convention).
func (a *Adapter) fingerprint(ctx context.Context, t adapter.Target) (*adapter.Plan, error) {
	d, err := a.db(t)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	rows, err := d.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	for _, n := range tables {
		fmt.Fprintf(h, "table %s %s\n", n, tableSignature(ctx, d, "", n))
		ti, err := loadTable(ctx, d, "", n)
		if err != nil || ti == nil {
			continue
		}
		hr, err := d.QueryContext(ctx, "SELECT "+ti.rowHashExpr("")+" AS h FROM "+ti.q()+" ORDER BY h")
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(h, "data %s\n", n)
		for hr.Next() {
			var rh string
			if err := hr.Scan(&rh); err != nil {
				_ = hr.Close()
				return nil, err
			}
			fmt.Fprintf(h, "%s\n", rh)
		}
		_ = hr.Close()
	}
	return &adapter.Plan{Facts: map[string]string{"fingerprint": hex.EncodeToString(h.Sum(nil))}}, nil
}
