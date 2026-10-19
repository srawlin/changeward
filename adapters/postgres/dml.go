package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
)

// Capture modes for DML before/after images.
const (
	CaptureReturning = "returning-old-new" // PG18+: RETURNING old/new
	CapturePreselect = "preselect"         // older Postgres, simple single-table statements
	CaptureNone      = "none"              // not capturable: checkpoint-only
)

// pair is a before/after image of one updated row (jsonb text).
type pair struct {
	B json.RawMessage `json:"b"`
	A json.RawMessage `json:"a"`
}

// capture is the observed effect of one DML statement.
type capture struct {
	Inserted   []json.RawMessage `json:"inserted,omitempty"`
	Deleted    []json.RawMessage `json:"deleted,omitempty"`
	Updated    []pair            `json:"updated,omitempty"`
	Count      int64             `json:"count"`
	BeforeHash string            `json:"before_hash"`
}

func (c *capture) computeHash() {
	var olds []string
	for _, d := range c.Deleted {
		olds = append(olds, string(d))
	}
	for _, u := range c.Updated {
		olds = append(olds, string(u.B))
	}
	sort.Strings(olds)
	h := sha256.New()
	fmt.Fprintf(h, "count=%d inserted=%d;", c.Count, len(c.Inserted))
	for _, o := range olds {
		h.Write([]byte(o))
		h.Write([]byte{0})
	}
	c.BeforeHash = hex.EncodeToString(h.Sum(nil))
}

// deparseNode renders one statement node back to SQL.
func deparseNode(n *pg.Node) (string, error) {
	return pg.Deparse(&pg.ParseResult{Version: parseTreeVersion(), Stmts: []*pg.RawStmt{{Stmt: n}}})
}

var (
	treeVersionOnce sync.Once
	treeVersion     int32
)

// parseTreeVersion is the libpg_query protobuf version, required by Deparse.
func parseTreeVersion() int32 {
	treeVersionOnce.Do(func() {
		if t, err := pg.Parse("SELECT 1"); err == nil {
			treeVersion = t.Version
		}
	})
	return treeVersion
}

// alias returns the name by which the target row can be referenced.
func alias(rv *pg.RangeVar) string {
	if rv.Alias != nil && rv.Alias.Aliasname != "" {
		return QuoteIdent(rv.Alias.Aliasname)
	}
	return QuoteIdent(rv.Relname)
}

// dmlStatements builds the SQL used to capture images for a DML statement.
// It strips any RETURNING list the caller wrote (Changegate's own RETURNING
// replaces it).
func dmlStatements(s *Stmt, serverVersion int) (mode, execSQL, preSQL string, err error) {
	n := proto.Clone(s.Node).(*pg.Node)
	switch {
	case n.GetInsertStmt() != nil:
		n.GetInsertStmt().ReturningList = nil
	case n.GetUpdateStmt() != nil:
		n.GetUpdateStmt().ReturningList = nil
	case n.GetDeleteStmt() != nil:
		n.GetDeleteStmt().ReturningList = nil
	case n.GetMergeStmt() != nil:
		n.GetMergeStmt().ReturningList = nil
	default:
		return CaptureNone, s.Text, "", nil
	}
	base, err := deparseNode(n)
	if err != nil {
		return "", "", "", fmt.Errorf("deparse: %w", err)
	}
	if serverVersion >= 180000 {
		return CaptureReturning, base + " RETURNING WITH (OLD AS cg_old, NEW AS cg_new) to_jsonb(cg_old)::text, to_jsonb(cg_new)::text", "", nil
	}
	// Fallback: simple single-table statements only.
	if s.HasFlag(FlagMultiTable) || s.HasFlag(FlagWithClause) {
		return CaptureNone, s.Text, "", nil
	}
	switch {
	case n.GetInsertStmt() != nil:
		if n.GetInsertStmt().OnConflictClause != nil {
			return CaptureNone, s.Text, "", nil
		}
		return CapturePreselect, base + " RETURNING to_jsonb(" + alias(n.GetInsertStmt().Relation) + ")::text", "", nil
	case n.GetUpdateStmt() != nil:
		up := n.GetUpdateStmt()
		pre, err := preselect(up.Relation, up.WhereClause)
		if err != nil {
			return "", "", "", err
		}
		return CapturePreselect, base + " RETURNING to_jsonb(" + alias(up.Relation) + ")::text", pre, nil
	case n.GetDeleteStmt() != nil:
		del := n.GetDeleteStmt()
		return CapturePreselect, base + " RETURNING to_jsonb(" + alias(del.Relation) + ")::text", "", nil
	}
	return CaptureNone, s.Text, "", nil
}

// preselect builds "SELECT to_jsonb(t)::text FROM rel t WHERE ... FOR UPDATE"
// by deparsing an equivalent DELETE and rewriting its head.
func preselect(rel *pg.RangeVar, where *pg.Node) (string, error) {
	del := &pg.Node{Node: &pg.Node_DeleteStmt{DeleteStmt: &pg.DeleteStmt{Relation: rel, WhereClause: where}}}
	sql, err := deparseNode(del)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(sql, "DELETE FROM ") {
		return "", errors.New("unexpected deparse output")
	}
	return "SELECT to_jsonb(" + alias(rel) + ")::text FROM " + strings.TrimPrefix(sql, "DELETE FROM ") + " FOR UPDATE", nil
}

// runDML executes a DML statement inside tx and captures its images.
func runDML(ctx context.Context, q querier, mode, execSQL, preSQL, kind string, pk []string) (*capture, error) {
	c := &capture{}
	switch mode {
	case CaptureReturning:
		rows, err := q.Query(ctx, execSQL)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var o, n *string
			if err := rows.Scan(&o, &n); err != nil {
				rows.Close()
				return nil, err
			}
			switch {
			case o == nil && n != nil:
				c.Inserted = append(c.Inserted, json.RawMessage(*n))
			case o != nil && n == nil:
				c.Deleted = append(c.Deleted, json.RawMessage(*o))
			case o != nil && n != nil:
				c.Updated = append(c.Updated, pair{B: json.RawMessage(*o), A: json.RawMessage(*n)})
			}
			c.Count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	case CapturePreselect:
		var before []string
		if preSQL != "" {
			var err error
			if before, err = queryStrings(ctx, q, preSQL); err != nil {
				return nil, err
			}
		}
		after, err := queryStrings(ctx, q, execSQL)
		if err != nil {
			return nil, err
		}
		c.Count = int64(len(after))
		switch kind {
		case "INSERT":
			for _, a := range after {
				c.Inserted = append(c.Inserted, json.RawMessage(a))
			}
		case "DELETE":
			for _, a := range after {
				c.Deleted = append(c.Deleted, json.RawMessage(a))
			}
		case "UPDATE":
			if len(pk) == 0 {
				return nil, errNoPairing
			}
			byKey := map[string]string{}
			for _, b := range before {
				k, err := keyOf(b, pk)
				if err != nil {
					return nil, err
				}
				byKey[k] = b
			}
			for _, a := range after {
				k, err := keyOf(a, pk)
				if err != nil {
					return nil, err
				}
				b, ok := byKey[k]
				if !ok {
					return nil, errNoPairing // primary key changed, or rows outside the predicate
				}
				c.Updated = append(c.Updated, pair{B: json.RawMessage(b), A: json.RawMessage(a)})
			}
		}
	default:
		tag, err := q.Exec(ctx, execSQL)
		if err != nil {
			return nil, err
		}
		c.Count = tag.RowsAffected()
	}
	c.computeHash()
	return c, nil
}

var errNoPairing = errors.New("cannot pair before/after images by primary key")

func queryStrings(ctx context.Context, q querier, sql string) ([]string, error) {
	rows, err := q.Query(ctx, sql)
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

func keyOf(img string, pk []string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(img), &m); err != nil {
		return "", err
	}
	var parts []string
	for _, k := range pk {
		parts = append(parts, string(m[k]))
	}
	return strings.Join(parts, "\x00"), nil
}

// dmlUndo is the undo handle for one DML statement.
type dmlUndo struct {
	Table      string   `json:"table"` // quoted, schema-qualified
	Display    string   `json:"display"`
	PK         []string `json:"pk,omitempty"`
	SetCols    []string `json:"set_cols"`    // non-generated columns
	InsertCols []string `json:"insert_cols"` // non-generated columns
	capture
}

func pkJoin(pk []string, left, right string) string {
	var parts []string
	for _, k := range pk {
		parts = append(parts, fmt.Sprintf("%s.%s = %s.%s", left, QuoteIdent(k), right, QuoteIdent(k)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " AND " + strings.Join(parts, " AND ")
}

func quoteAll(cols []string, prefix string) string {
	var out []string
	for _, c := range cols {
		out = append(out, prefix+QuoteIdent(c))
	}
	return strings.Join(out, ", ")
}

func rawArray(xs []json.RawMessage) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range xs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(x)
	}
	b.WriteByte(']')
	return b.String()
}

// undoDML reverses one captured DML statement. Each inverse is guarded:
// it touches a row only if the row still equals its after-image. If any row
// no longer matches, the whole undo is refused with a conflict.
func undoDML(ctx context.Context, q querier, u *dmlUndo) (int64, []string, error) {
	var total int64
	var conflicts []string
	// 1. Remove inserted rows.
	if len(u.Inserted) > 0 {
		sql := fmt.Sprintf(`WITH img AS (SELECT e AS j FROM jsonb_array_elements($1::jsonb) e)
			DELETE FROM %s t USING img, LATERAL jsonb_populate_record(NULL::%s, img.j) r
			WHERE to_jsonb(t) = img.j%s`, u.Table, u.Table, pkJoin(u.PK, "t", "r"))
		tag, err := q.Exec(ctx, sql, rawArray(u.Inserted))
		if err != nil {
			return 0, nil, err
		}
		if tag.RowsAffected() != int64(len(u.Inserted)) {
			conflicts = append(conflicts, fmt.Sprintf("%s: %d of %d inserted rows were changed or removed since apply",
				u.Display, int64(len(u.Inserted))-tag.RowsAffected(), len(u.Inserted)))
		}
		total += tag.RowsAffected()
	}
	// 2. Revert updated rows.
	if len(u.Updated) > 0 {
		var arr []json.RawMessage
		for _, p := range u.Updated {
			b, _ := json.Marshal(p)
			arr = append(arr, b)
		}
		sql := fmt.Sprintf(`WITH img AS (SELECT e->'b' AS b, e->'a' AS a FROM jsonb_array_elements($1::jsonb) e)
			UPDATE %s t SET (%s) = ROW(%s)
			FROM img, LATERAL jsonb_populate_record(NULL::%s, img.b) r, LATERAL jsonb_populate_record(NULL::%s, img.a) ra
			WHERE to_jsonb(t) = img.a%s`, u.Table, quoteAll(u.SetCols, ""), quoteAll(u.SetCols, "r."), u.Table, u.Table, pkJoin(u.PK, "t", "ra"))
		tag, err := q.Exec(ctx, sql, rawArray(arr))
		if err != nil {
			return 0, nil, err
		}
		if tag.RowsAffected() != int64(len(u.Updated)) {
			conflicts = append(conflicts, fmt.Sprintf("%s: %d of %d updated rows were changed since apply",
				u.Display, int64(len(u.Updated))-tag.RowsAffected(), len(u.Updated)))
		}
		total += tag.RowsAffected()
	}
	// 3. Re-insert deleted rows. A unique violation means the row was re-created.
	if len(u.Deleted) > 0 {
		sql := fmt.Sprintf(`INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM jsonb_populate_recordset(NULL::%s, $1::jsonb) r`,
			u.Table, quoteAll(u.InsertCols, ""), quoteAll(u.InsertCols, "r."), u.Table)
		tag, err := q.Exec(ctx, sql, rawArray(u.Deleted))
		if err != nil {
			var pgErr interface{ SQLState() string }
			if errors.As(err, &pgErr) && (pgErr.SQLState() == "23505" || pgErr.SQLState() == "23503") {
				return 0, []string{fmt.Sprintf("%s: deleted rows cannot be restored: %v", u.Display, err)}, nil
			}
			return 0, nil, err
		}
		total += tag.RowsAffected()
	}
	return total, conflicts, nil
}
