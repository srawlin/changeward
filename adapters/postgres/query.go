package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/srawlin/changegate/pkg/adapter"
	"github.com/jackc/pgx/v5"
)

var _ adapter.Adapter = (*Adapter)(nil)

// QueryResult is a capped, redacted read result.
type QueryResult struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Truncated bool       `json:"truncated"`
	Redacted  []string   `json:"redacted,omitempty"`
}

// Query runs read-only SQL in a READ ONLY transaction (so even functions
// cannot write), with a statement timeout, capped rows and sensitive
// columns redacted. This is the pass-through path for reads (§6.1).
func (a *Adapter) Query(ctx context.Context, t adapter.Target, sql string, maxRows int) (*QueryResult, error) {
	_, stmts, err := Classify(sql)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, errors.New("query takes exactly one statement")
	}
	if stmts[0].Access != adapter.Read {
		return nil, fmt.Errorf("%s is not read-only; use propose_change", stmts[0].Kind)
	}
	if maxRows <= 0 {
		maxRows = 100
	}
	pool, err := a.pool(ctx, t)
	if err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setLocal(ctx, tx, a.opts.PlanStatementTimeout, a.opts.PlanLockTimeout); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, stmts[0].Text)
	if err != nil {
		return nil, describePGError(1, stmts[0], err)
	}
	defer rows.Close()
	red := newRedactor(t)
	red.keysOnly = false
	fds := rows.FieldDescriptions()
	res := &QueryResult{}
	tables := map[uint32]string{}
	sensitive := make([]bool, len(fds))
	for _, fd := range fds {
		res.Columns = append(res.Columns, fd.Name)
		if fd.TableOID != 0 {
			if _, ok := tables[fd.TableOID]; !ok {
				tables[fd.TableOID] = ""
			}
		}
	}
	var all [][]any
	for rows.Next() {
		if len(all) >= maxRows {
			res.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		all = append(all, vals)
	}
	if err := rows.Err(); err != nil && !res.Truncated {
		return nil, err
	}
	rows.Close()
	for oid := range tables {
		var name string
		if err := tx.QueryRow(ctx, `SELECT relname FROM pg_class WHERE oid = $1`, oid).Scan(&name); err == nil {
			tables[oid] = name
		}
	}
	for i, fd := range fds {
		if red.sensitive(tables[fd.TableOID], fd.Name) {
			sensitive[i] = true
			res.Redacted = append(res.Redacted, fd.Name)
		}
	}
	for _, vals := range all {
		row := make([]string, len(vals))
		for i, v := range vals {
			switch {
			case sensitive[i]:
				row[i] = "•••"
			case v == nil:
				row[i] = "NULL"
			default:
				row[i] = formatValue(v)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func formatValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return fmt.Sprintf("\\x%x", x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case fmt.Stringer:
		return x.String()
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
