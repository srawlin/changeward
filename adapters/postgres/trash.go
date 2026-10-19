package postgres

import (
	"context"
	"fmt"
)

// TrashItem is a soft-dropped object awaiting purge.
type TrashItem struct {
	Kind string `json:"kind"` // "table", "column", "index", "sequence", "view"
	Name string `json:"name"`
}

// ListTrash lists soft-dropped relations and columns.
func ListTrash(ctx context.Context, q querier) ([]TrashItem, error) {
	rows, err := q.Query(ctx, `
		SELECT CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'view'
		         WHEN 'S' THEN 'sequence' WHEN 'f' THEN 'table' ELSE 'other' END, n.nspname || '.' || c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S','f')
		UNION ALL
		SELECT 'column', n.nspname || '.' || c.relname || '.' || a.attname
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE a.attname LIKE $2 AND NOT a.attisdropped AND a.attnum > 0 AND n.nspname <> $1
		ORDER BY 1, 2`, TrashSchema, TrashPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		var it TrashItem
		if err := rows.Scan(&it.Kind, &it.Name); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PurgeTrash permanently drops soft-dropped objects. Changes whose undo
// depends on them can no longer be undone.
func PurgeTrash(ctx context.Context, q querier) (int, error) {
	items, err := ListTrash(ctx, q)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		var sql string
		switch it.Kind {
		case "column":
			// name is schema.table.column
			var schema, table, col string
			if err := q.QueryRow(ctx, `SELECT split_part($1, '.', 1), split_part($1, '.', 2), split_part($1, '.', 3)`, it.Name).Scan(&schema, &table, &col); err != nil {
				return n, err
			}
			sql = fmt.Sprintf("ALTER TABLE %s.%s DROP COLUMN %s", QuoteIdent(schema), QuoteIdent(table), QuoteIdent(col))
		default:
			continue
		}
		if _, err := q.Exec(ctx, sql); err != nil {
			return n, err
		}
		n++
	}
	var count int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S','f')`, TrashSchema).Scan(&count); err != nil {
		return n, err
	}
	if _, err := q.Exec(ctx, "DROP SCHEMA IF EXISTS "+QuoteIdent(TrashSchema)+" CASCADE"); err != nil {
		return n, err
	}
	return n + count, nil
}
