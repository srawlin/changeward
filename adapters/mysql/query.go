package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/srawlin/changeward/pkg/adapter"
)

var _ adapter.Querier = (*Adapter)(nil)

// Query runs one read-only statement in a READ ONLY transaction with a time
// limit, capping rows and redacting sensitive columns by name.
func (a *Adapter) Query(ctx context.Context, t adapter.Target, q string, maxRows int) (*adapter.QueryResult, error) {
	_, stmts, err := Classify(q)
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
	d, err := a.db(t)
	if err != nil {
		return nil, err
	}
	conn, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.ExecContext(ctx, "SET SESSION MAX_EXECUTION_TIME = 10000")
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, stmts[0].Text)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var patterns []string
	for k, v := range t.Config {
		if strings.HasPrefix(k, "sensitive.") {
			patterns = append(patterns, strings.ToLower(v))
		}
	}
	res := &adapter.QueryResult{Columns: cols}
	sensitive := make([]bool, len(cols))
	for i, c := range cols {
		lc := strings.ToLower(c)
		for _, p := range patterns {
			if p == lc || strings.HasSuffix(p, "."+lc) {
				sensitive[i] = true
				res.Redacted = append(res.Redacted, c)
				break
			}
		}
	}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		vals := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r := make([]string, len(cols))
		for i, v := range vals {
			switch {
			case sensitive[i]:
				r[i] = "•••"
			case !v.Valid:
				r[i] = "NULL"
			default:
				r[i] = v.String
			}
		}
		res.Rows = append(res.Rows, r)
	}
	return res, rows.Err()
}
