package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Snapshot is a comparable picture of a database: catalog definitions and,
// optionally, per-table row counts and checksums. Rehearsals compare the
// snapshot before a migration with the one after its rollback.
type Snapshot struct {
	Objects map[string]string    `json:"objects"`
	Tables  map[string]TableData `json:"tables,omitempty"`
}

// TableData is a row count and an order-independent checksum.
type TableData struct {
	Rows     int64  `json:"rows"`
	Checksum string `json:"checksum"`
}

// Fingerprint hashes a snapshot.
func (s *Snapshot) Fingerprint() string {
	h := sha256.New()
	keys := make([]string, 0, len(s.Objects))
	for k := range s.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, s.Objects[k])
	}
	keys = keys[:0]
	for k := range s.Tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s:%d:%s\n", k, s.Tables[k].Rows, s.Tables[k].Checksum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TakeSnapshot reads the catalog (and data, if withData) of user schemas.
// excludeTables skips data checksums for named tables (e.g. migration
// bookkeeping tables).
func TakeSnapshot(ctx context.Context, q querier, withData bool, excludeTables map[string]bool) (*Snapshot, error) {
	s := &Snapshot{Objects: map[string]string{}, Tables: map[string]TableData{}}
	queries := []struct{ prefix, sql string }{
		{"schema", `SELECT n.nspname, '' FROM pg_namespace n WHERE ` + userSchemaFilter},
		{"relation", `SELECT n.nspname || '.' || c.relname, c.relkind::text || coalesce(' ' || c.relpersistence::text, '')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r','p','v','m','S','f','c') AND ` + userSchemaFilter},
		{"column", `SELECT n.nspname || '.' || c.relname || '.' || a.attname,
			format_type(a.atttypid, a.atttypmod) || CASE WHEN a.attnotnull THEN ' not null' ELSE '' END ||
			coalesce(' default ' || pg_get_expr(d.adbin, d.adrelid), '')
			FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f','c') AND ` + userSchemaFilter},
		{"index", `SELECT n.nspname || '.' || c.relname, pg_get_indexdef(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('i','I') AND ` + userSchemaFilter},
		{"constraint", `SELECT co.conrelid::regclass::text || '.' || co.conname, pg_get_constraintdef(co.oid)
			FROM pg_constraint co JOIN pg_namespace n ON n.oid = co.connamespace WHERE co.conrelid <> 0 AND ` + userSchemaFilter},
		{"view", `SELECT n.nspname || '.' || c.relname, pg_get_viewdef(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('v','m') AND ` + userSchemaFilter},
		{"function", `SELECT p.oid::regprocedure::text, md5(p.prosrc || coalesce(pg_get_function_result(p.oid), '')) FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace WHERE ` + userSchemaFilter},
		{"enum", `SELECT t.oid::regtype::text, string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_type t
			JOIN pg_enum e ON e.enumtypid = t.oid JOIN pg_namespace n ON n.oid = t.typnamespace WHERE ` + userSchemaFilter + ` GROUP BY 1`},
		{"trigger", `SELECT t.tgrelid::regclass::text || '.' || t.tgname, pg_get_triggerdef(t.oid) FROM pg_trigger t WHERE NOT t.tgisinternal
			AND t.tgrelid IN (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE ` + userSchemaFilter + `)`},
		{"extension", `SELECT extname, '' FROM pg_extension`},
	}
	for _, qq := range queries {
		rows, err := q.Query(ctx, qq.sql)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", qq.prefix, err)
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return nil, err
			}
			s.Objects[qq.prefix+" "+k] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if !withData {
		return s, nil
	}
	rows, err := q.Query(ctx, `SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname), n.nspname || '.' || c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p') AND `+userSchemaFilter)
	if err != nil {
		return nil, err
	}
	type tbl struct{ quoted, name string }
	var tables []tbl
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.quoted, &t.name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	for _, t := range tables {
		if excludeTables[t.name] {
			continue
		}
		var td TableData
		err := q.QueryRow(ctx, `SELECT count(*), coalesce(md5(string_agg(h, '' ORDER BY h)), '') FROM (SELECT md5(x::text) h FROM `+t.quoted+` x) s`).
			Scan(&td.Rows, &td.Checksum)
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", t.name, err)
		}
		s.Tables[t.name] = td
	}
	return s, nil
}

// DiffSnapshots describes how b differs from a, one line per object.
func DiffSnapshots(a, b *Snapshot) []string {
	var out []string
	for k, v := range b.Objects {
		old, ok := a.Objects[k]
		switch {
		case !ok:
			out = append(out, "+ "+k+describe(v))
		case old != v:
			out = append(out, fmt.Sprintf("~ %s: %s → %s", k, short(old), short(v)))
		}
	}
	for k, v := range a.Objects {
		if _, ok := b.Objects[k]; !ok {
			out = append(out, "- "+k+describe(v))
		}
	}
	for k, v := range b.Tables {
		old, ok := a.Tables[k]
		if !ok {
			continue
		}
		if old.Rows != v.Rows {
			out = append(out, fmt.Sprintf("~ data %s: %d → %d rows", k, old.Rows, v.Rows))
		} else if old.Checksum != v.Checksum {
			out = append(out, fmt.Sprintf("~ data %s: row contents differ (%d rows)", k, v.Rows))
		}
	}
	sort.Strings(out)
	return out
}

func describe(v string) string {
	if v == "" {
		return ""
	}
	return ": " + short(v)
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
