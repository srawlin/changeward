package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TrashSchema holds soft-dropped objects until they are purged.
const TrashSchema = "_changegate_trash"

// userSchemaFilter excludes system schemas and the trash schema.
const userSchemaFilter = `n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','` + TrashSchema + `')
  AND n.nspname NOT LIKE 'pg_temp_%' AND n.nspname NOT LIKE 'pg_toast_temp_%'`

// querier is satisfied by pgx.Tx and *pgx.Conn.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// relRef identifies a relation.
type relRef struct {
	OID    uint32 `json:"oid"`
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

func (r *relRef) qualified() string { return QuoteIdent(r.Schema) + "." + QuoteIdent(r.Name) }
func (r *relRef) display() string   { return r.Schema + "." + r.Name }

// resolveRel looks up a relation by (possibly unqualified) name using the
// session search_path. It returns nil if it does not exist.
func resolveRel(ctx context.Context, q querier, name string) (*relRef, error) {
	var r relRef
	err := q.QueryRow(ctx, `SELECT c.oid, n.nspname, c.relname, c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass($1)`, name).
		Scan(&r.OID, &r.Schema, &r.Name, &r.Kind)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

type column struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	NotNull   bool   `json:"not_null"`
	Default   string `json:"default,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	Identity  string `json:"identity,omitempty"` // "a" always, "d" by default
}

func columns(ctx context.Context, q querier, oid uint32) ([]column, error) {
	rows, err := q.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
		coalesce(pg_get_expr(d.adbin, d.adrelid), ''), a.attgenerated <> '', a.attidentity::text
		FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.Generated, &c.Identity); err != nil {
			return nil, err
		}
		if c.Generated {
			c.Default = ""
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func primaryKey(ctx context.Context, q querier, oid uint32) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = $1 AND i.indisprimary ORDER BY array_position(i.indkey::int2[], a.attnum)`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// relSignature is a precondition fingerprint of a relation's shape.
func relSignature(ctx context.Context, q querier, name string) (string, error) {
	r, err := resolveRel(ctx, q, name)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "absent", nil
	}
	cols, err := columns(ctx, q, r.OID)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s.%s:%s;", r.Schema, r.Name, r.Kind)
	for _, c := range cols {
		fmt.Fprintf(h, "%s %s %v %s|", c.Name, c.Type, c.NotNull, c.Default)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// userTriggers lists non-internal triggers on a relation, and whether any
// trigger function looks like it has effects outside the database.
func userTriggers(ctx context.Context, q querier, oid uint32) (names []string, external bool, err error) {
	rows, err := q.Query(ctx, `SELECT t.tgname, p.prosrc FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE t.tgrelid = $1 AND NOT t.tgisinternal AND t.tgenabled <> 'D'`, oid)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, src string
		if err := rows.Scan(&n, &src); err != nil {
			return nil, false, err
		}
		names = append(names, n)
		ls := strings.ToLower(src)
		for _, k := range []string{"dblink", "http_", "http(", "pg_background", "lo_export", "copy "} {
			if strings.Contains(ls, k) {
				external = true
			}
		}
	}
	return names, external, rows.Err()
}

type cascadeFK struct {
	Name     string
	Child    string
	OnDelete string
	OnUpdate string
	Cols     []string // referenced columns in the parent
}

// cascadingFKs lists foreign keys referencing oid whose actions modify child rows.
func cascadingFKs(ctx context.Context, q querier, oid uint32) ([]cascadeFK, error) {
	rows, err := q.Query(ctx, `SELECT c.conname, c.conrelid::regclass::text, c.confdeltype::text, c.confupdtype::text,
		ARRAY(SELECT a.attname FROM pg_attribute a WHERE a.attrelid = c.confrelid AND a.attnum = ANY(c.confkey))
		FROM pg_constraint c WHERE c.contype = 'f' AND c.confrelid = $1
		AND (c.confdeltype IN ('c','n','d') OR c.confupdtype IN ('c','n','d'))`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cascadeFK
	for rows.Next() {
		var f cascadeFK
		if err := rows.Scan(&f.Name, &f.Child, &f.OnDelete, &f.OnUpdate, &f.Cols); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func cascades(action string) bool { return action == "c" || action == "n" || action == "d" }

// estimateRows returns an exact count for small tables and reltuples otherwise.
func estimateRows(ctx context.Context, q querier, r *relRef) (int64, bool, error) {
	var tuples float64
	var pages int64
	if err := q.QueryRow(ctx, `SELECT reltuples, relpages FROM pg_class WHERE oid = $1`, r.OID).Scan(&tuples, &pages); err != nil {
		return 0, false, err
	}
	if pages < 2000 || tuples < 0 {
		var n int64
		if err := q.QueryRow(ctx, `SELECT count(*) FROM `+r.qualified()).Scan(&n); err != nil {
			return 0, false, err
		}
		return n, true, nil
	}
	return int64(tuples), false, nil
}

func relfilenode(ctx context.Context, q querier, oid uint32) (uint32, error) {
	var n uint32
	err := q.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE oid = $1`, oid).Scan(&n)
	return n, err
}

// heldLocks lists relation locks held by this backend on user relations.
func heldLocks(ctx context.Context, q querier) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT n.nspname || '.' || c.relname, l.mode FROM pg_locks l
		JOIN pg_class c ON c.oid = l.relation JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND l.granted AND `+userSchemaFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var rel, mode string
		if err := rows.Scan(&rel, &mode); err != nil {
			return nil, err
		}
		if lockRank(mode) > lockRank(out[rel]) {
			out[rel] = mode
		}
	}
	return out, rows.Err()
}

var lockOrder = []string{"", "AccessShareLock", "RowShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock",
	"ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock"}

func lockRank(m string) int {
	for i, x := range lockOrder {
		if x == m {
			return i
		}
	}
	return 0
}

// BlocksWrites reports whether a lock mode blocks concurrent INSERT/UPDATE/DELETE.
func BlocksWrites(mode string) bool { return lockRank(mode) >= lockRank("ShareLock") }

// BlocksReads reports whether a lock mode blocks concurrent SELECT.
func BlocksReads(mode string) bool { return mode == "AccessExclusiveLock" }

// catalogObjects is a coarse inventory used to diff the catalog around DDL.
type catalogObjects struct {
	Relations   map[uint32]relRef     // tables, views, sequences, indexes, matviews
	Columns     map[string]string     // "reloid:attnum" -> attname
	Constraints map[uint32]constraint // constraint oid
	Functions   map[uint32]string     // oid -> regprocedure
	Types       map[uint32]string     // oid -> regtype
	Schemas     map[string]bool
	Extensions  map[string]bool
	Triggers    map[uint32]trigger // trigger oid
	IndexOwner  map[uint32]uint32 // index oid -> table oid
}

type constraint struct {
	RelOID uint32
	Rel    string
	Name   string
}

type trigger struct {
	RelOID uint32
	Rel    string
	Name   string
}

func loadCatalog(ctx context.Context, q querier) (*catalogObjects, error) {
	c := &catalogObjects{Relations: map[uint32]relRef{}, Columns: map[string]string{}, Constraints: map[uint32]constraint{},
		Functions: map[uint32]string{}, Types: map[uint32]string{}, Schemas: map[string]bool{}, Extensions: map[string]bool{},
		Triggers: map[uint32]trigger{}, IndexOwner: map[uint32]uint32{}}
	type load struct {
		sql  string
		scan func(pgx.Rows) error
	}
	loads := []load{
		{`SELECT c.oid, n.nspname, c.relname, c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relkind IN ('r','p','v','m','S','i','I','f') AND ` + userSchemaFilter, func(r pgx.Rows) error {
			var x relRef
			if err := r.Scan(&x.OID, &x.Schema, &x.Name, &x.Kind); err != nil {
				return err
			}
			c.Relations[x.OID] = x
			return nil
		}},
		{`SELECT a.attrelid, a.attnum, a.attname FROM pg_attribute a JOIN pg_class cl ON cl.oid = a.attrelid JOIN pg_namespace n ON n.oid = cl.relnamespace
		  WHERE a.attnum > 0 AND NOT a.attisdropped AND cl.relkind IN ('r','p','f') AND ` + userSchemaFilter, func(r pgx.Rows) error {
			var oid uint32
			var num int
			var name string
			if err := r.Scan(&oid, &num, &name); err != nil {
				return err
			}
			c.Columns[fmt.Sprintf("%d:%d", oid, num)] = name
			return nil
		}},
		{`SELECT co.oid, co.conrelid, co.conrelid::regclass::text, co.conname FROM pg_constraint co JOIN pg_namespace n ON n.oid = co.connamespace
		  WHERE co.conrelid <> 0 AND ` + userSchemaFilter, func(r pgx.Rows) error {
			var oid uint32
			var x constraint
			if err := r.Scan(&oid, &x.RelOID, &x.Rel, &x.Name); err != nil {
				return err
			}
			c.Constraints[oid] = x
			return nil
		}},
		{`SELECT p.oid, p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE ` + userSchemaFilter + ` AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')`, func(r pgx.Rows) error {
			var oid uint32
			var s string
			if err := r.Scan(&oid, &s); err != nil {
				return err
			}
			c.Functions[oid] = s
			return nil
		}},
		{`SELECT t.oid, t.oid::regtype::text FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		  WHERE (t.typtype IN ('e','d','r','m') OR (t.typtype = 'c' AND (SELECT relkind FROM pg_class WHERE oid = t.typrelid) = 'c'))
		  AND ` + userSchemaFilter, func(r pgx.Rows) error {
			var oid uint32
			var s string
			if err := r.Scan(&oid, &s); err != nil {
				return err
			}
			c.Types[oid] = s
			return nil
		}},
		{`SELECT n.nspname FROM pg_namespace n WHERE ` + userSchemaFilter, func(r pgx.Rows) error {
			var s string
			if err := r.Scan(&s); err != nil {
				return err
			}
			c.Schemas[s] = true
			return nil
		}},
		{`SELECT extname FROM pg_extension`, func(r pgx.Rows) error {
			var s string
			if err := r.Scan(&s); err != nil {
				return err
			}
			c.Extensions[s] = true
			return nil
		}},
		{`SELECT t.oid, t.tgrelid, t.tgname, t.tgrelid::regclass::text FROM pg_trigger t WHERE NOT t.tgisinternal`, func(r pgx.Rows) error {
			var oid uint32
			var x trigger
			if err := r.Scan(&oid, &x.RelOID, &x.Name, &x.Rel); err != nil {
				return err
			}
			c.Triggers[oid] = x
			return nil
		}},
		{`SELECT indexrelid, indrelid FROM pg_index`, func(r pgx.Rows) error {
			var i, t uint32
			if err := r.Scan(&i, &t); err != nil {
				return err
			}
			c.IndexOwner[i] = t
			return nil
		}},
	}
	for _, l := range loads {
		rows, err := q.Query(ctx, l.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			if err := l.scan(rows); err != nil {
				rows.Close()
				return nil, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// catalogDiff lists objects created between two inventories.
type catalogDiff struct {
	Relations   []relRef
	Columns     []struct{ Rel relRef; Name string }
	Constraints []constraint
	Functions   []string
	Types       []string
	Schemas     []string
	Extensions  []string
	Triggers    []struct{ Rel, Name string }
	// Removed relations (by OID) — used to detect drops we did not expect.
	RemovedRelations []relRef
}

func diffCatalog(before, after *catalogObjects) catalogDiff {
	var d catalogDiff
	// Objects re-created under the same name (e.g. indexes rebuilt by
	// ALTER COLUMN TYPE) are not new.
	beforeNames := map[string]bool{}
	for _, r := range before.Relations {
		beforeNames[r.Kind+":"+r.Schema+"."+r.Name] = true
	}
	newRel := map[uint32]bool{}
	for oid, r := range after.Relations {
		if _, ok := before.Relations[oid]; !ok && !beforeNames[r.Kind+":"+r.Schema+"."+r.Name] && !isTrashName(r.Name) {
			newRel[oid] = true
		}
	}
	for oid := range newRel {
		r := after.Relations[oid]
		if (r.Kind == "i" || r.Kind == "I") && newRel[after.IndexOwner[oid]] {
			continue // index on a new table goes with the table
		}
		if r.Kind == "S" && ownedByNewRel(after, oid, newRel) {
			continue
		}
		d.Relations = append(d.Relations, r)
	}
	for oid, r := range before.Relations {
		if _, ok := after.Relations[oid]; !ok {
			d.RemovedRelations = append(d.RemovedRelations, r)
		}
	}
	for k, name := range after.Columns {
		if _, ok := before.Columns[k]; ok || isTrashName(name) {
			continue
		}
		var oid uint32
		_, _ = fmt.Sscanf(k, "%d:", &oid)
		if newRel[oid] {
			continue
		}
		if r, ok := after.Relations[oid]; ok {
			d.Columns = append(d.Columns, struct {
				Rel  relRef
				Name string
			}{r, name})
		}
	}
	beforeCons := map[string]bool{}
	for _, c := range before.Constraints {
		beforeCons[fmt.Sprintf("%d:%s", c.RelOID, c.Name)] = true
	}
	for oid, c := range after.Constraints {
		if _, ok := before.Constraints[oid]; ok || newRel[c.RelOID] || beforeCons[fmt.Sprintf("%d:%s", c.RelOID, c.Name)] {
			continue
		}
		d.Constraints = append(d.Constraints, c)
	}
	for oid, f := range after.Functions {
		if _, ok := before.Functions[oid]; !ok {
			d.Functions = append(d.Functions, f)
		}
	}
	for oid, t := range after.Types {
		if _, ok := before.Types[oid]; !ok {
			d.Types = append(d.Types, t)
		}
	}
	for s := range after.Schemas {
		if !before.Schemas[s] {
			d.Schemas = append(d.Schemas, s)
		}
	}
	for s := range after.Extensions {
		if !before.Extensions[s] {
			d.Extensions = append(d.Extensions, s)
		}
	}
	for oid, t := range after.Triggers {
		if _, ok := before.Triggers[oid]; ok || newRel[t.RelOID] {
			continue
		}
		d.Triggers = append(d.Triggers, struct{ Rel, Name string }{t.Rel, t.Name})
	}
	sort.Slice(d.Relations, func(i, j int) bool { return d.Relations[i].OID > d.Relations[j].OID })
	sort.Slice(d.Columns, func(i, j int) bool { return d.Columns[i].Name < d.Columns[j].Name })
	sort.Slice(d.Constraints, func(i, j int) bool { return d.Constraints[i].Name < d.Constraints[j].Name })
	sort.Strings(d.Functions)
	sort.Strings(d.Types)
	sort.Strings(d.Schemas)
	return d
}

// TrashPrefix starts the names of soft-dropped objects and columns.
const TrashPrefix = "_cg_"

func isTrashName(n string) bool { return strings.HasPrefix(n, TrashPrefix) }

// ownedByNewRel is approximated by sequence naming (serial columns on new
// tables create <table>_<col>_seq); a missed match only adds a harmless
// DROP SEQUENCE IF EXISTS to the inverse.
func ownedByNewRel(c *catalogObjects, seq uint32, newRel map[uint32]bool) bool {
	s := c.Relations[seq]
	for oid := range newRel {
		r := c.Relations[oid]
		if (r.Kind == "r" || r.Kind == "p") && r.Schema == s.Schema && strings.HasPrefix(s.Name, r.Name+"_") && strings.HasSuffix(s.Name, "_seq") {
			return true
		}
	}
	return false
}

// inverseOfCreations returns statements that remove created objects, in a
// dependency-safe order. New tables are soft-dropped, never hard-dropped.
func (d catalogDiff) inverseOfCreations(trashName func(string) string) []string {
	var out []string
	for _, t := range d.Triggers {
		out = append(out, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", QuoteIdent(t.Name), t.Rel))
	}
	for _, c := range d.Constraints {
		out = append(out, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", c.Rel, QuoteIdent(c.Name)))
	}
	for _, r := range d.Relations {
		if r.Kind == "i" || r.Kind == "I" {
			out = append(out, "DROP INDEX IF EXISTS "+r.qualified())
		}
	}
	for _, c := range d.Columns {
		out = append(out, fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", c.Rel.qualified(), QuoteIdent(c.Name)))
	}
	for _, r := range d.Relations {
		switch r.Kind {
		case "v":
			out = append(out, "DROP VIEW IF EXISTS "+r.qualified())
		case "m":
			out = append(out, "DROP MATERIALIZED VIEW IF EXISTS "+r.qualified())
		}
	}
	for _, r := range d.Relations {
		switch r.Kind {
		case "r", "p", "f":
			out = append(out, softDropRelSQL(r, trashName(r.Name))...)
		}
	}
	for _, r := range d.Relations {
		if r.Kind == "S" {
			out = append(out, "DROP SEQUENCE IF EXISTS "+r.qualified())
		}
	}
	for _, f := range d.Functions {
		out = append(out, "DROP ROUTINE IF EXISTS "+f)
	}
	for _, t := range d.Types {
		out = append(out, "DROP TYPE IF EXISTS "+t)
	}
	for _, s := range d.Schemas {
		out = append(out, "DROP SCHEMA IF EXISTS "+QuoteIdent(s))
	}
	for _, e := range d.Extensions {
		out = append(out, "DROP EXTENSION IF EXISTS "+QuoteIdent(e))
	}
	return out
}

func relKeyword(kind string) string {
	switch kind {
	case "v":
		return "VIEW"
	case "m":
		return "MATERIALIZED VIEW"
	case "S":
		return "SEQUENCE"
	case "f":
		return "FOREIGN TABLE"
	}
	return "TABLE"
}

// softDropRelSQL moves a relation into the trash schema. Its indexes and
// owned sequences move with it, so they are renamed to unique trash names
// first (dynamically, since this also runs at undo time when the set of
// indexes is not known in advance).
func softDropRelSQL(r relRef, trash string) []string {
	kw := relKeyword(r.Kind)
	prefix := strings.TrimSuffix(trash, r.Name)
	return []string{
		"CREATE SCHEMA IF NOT EXISTS " + QuoteIdent(TrashSchema),
		fmt.Sprintf(`DO $cg$ DECLARE x record; BEGIN
  FOR x IN SELECT c.relname, c.relkind FROM pg_class c WHERE c.oid IN (
      SELECT indexrelid FROM pg_index WHERE indrelid = %[1]s::regclass
      UNION SELECT d.objid FROM pg_depend d JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
        WHERE d.refobjid = %[1]s::regclass AND d.deptype IN ('a','i'))
  LOOP
    EXECUTE format('ALTER %%s %%I.%%I RENAME TO %%I', CASE WHEN x.relkind = 'S' THEN 'SEQUENCE' ELSE 'INDEX' END,
      %[2]s, x.relname, left(%[3]s || x.relname, 63));
  END LOOP; END $cg$`, QuoteLiteral(r.qualified()), QuoteLiteral(r.Schema), QuoteLiteral(prefix)),
		fmt.Sprintf("ALTER %s %s RENAME TO %s", kw, r.qualified(), QuoteIdent(trash)),
		fmt.Sprintf("ALTER %s %s.%s SET SCHEMA %s", kw, QuoteIdent(r.Schema), QuoteIdent(trash), QuoteIdent(TrashSchema)),
	}
}

// softDropRelExplicit is softDropRelSQL for apply time: index and sequence
// names are known, so the inverse can restore them exactly.
func softDropRelExplicit(ctx context.Context, q querier, r relRef, trash string) (exec, inverse []string, err error) {
	kw := relKeyword(r.Kind)
	prefix := strings.TrimSuffix(trash, r.Name)
	rows, err := q.Query(ctx, `SELECT c.relname, c.relkind::text FROM pg_class c WHERE c.oid IN (
		SELECT indexrelid FROM pg_index WHERE indrelid = $1
		UNION SELECT d.objid FROM pg_depend d JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
		  WHERE d.refobjid = $1 AND d.deptype IN ('a','i')) ORDER BY 1`, r.OID)
	if err != nil {
		return nil, nil, err
	}
	type dep struct{ name, kind string }
	var deps []dep
	for rows.Next() {
		var d dep
		if err := rows.Scan(&d.name, &d.kind); err != nil {
			rows.Close()
			return nil, nil, err
		}
		deps = append(deps, d)
	}
	rows.Close()
	exec = []string{"CREATE SCHEMA IF NOT EXISTS " + QuoteIdent(TrashSchema)}
	var restoreNames []string
	for i, d := range deps {
		k := "INDEX"
		if d.kind == "S" {
			k = "SEQUENCE"
		}
		tn := fmt.Sprintf("%s%d_%s", prefix, i, d.name)
		if len(tn) > 63 {
			tn = tn[:63]
		}
		exec = append(exec, fmt.Sprintf("ALTER %s %s.%s RENAME TO %s", k, QuoteIdent(r.Schema), QuoteIdent(d.name), QuoteIdent(tn)))
		restoreNames = append(restoreNames, fmt.Sprintf("ALTER %s %s.%s RENAME TO %s", k, QuoteIdent(r.Schema), QuoteIdent(tn), QuoteIdent(d.name)))
	}
	exec = append(exec,
		fmt.Sprintf("ALTER %s %s RENAME TO %s", kw, r.qualified(), QuoteIdent(trash)),
		fmt.Sprintf("ALTER %s %s.%s SET SCHEMA %s", kw, QuoteIdent(r.Schema), QuoteIdent(trash), QuoteIdent(TrashSchema)))
	inverse = append(restoreRelSQL(r, trash), restoreNames...)
	return exec, inverse, nil
}

func restoreRelSQL(r relRef, trash string) []string {
	kw := relKeyword(r.Kind)
	return []string{
		fmt.Sprintf("ALTER %s %s.%s SET SCHEMA %s", kw, QuoteIdent(TrashSchema), QuoteIdent(trash), QuoteIdent(r.Schema)),
		fmt.Sprintf("ALTER %s %s.%s RENAME TO %s", kw, QuoteIdent(r.Schema), QuoteIdent(trash), QuoteIdent(r.Name)),
	}
}
