package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MaxSyntheticRows caps rows generated per table.
const MaxSyntheticRows = 1_000_000

type synCol struct {
	name, typ, typtype, elemType string
	notNull, hasDefault, generated  bool
	maxLen                          int
	enumType                        string
	fkTable, fkCol                  string
}

type synTable struct {
	oid    uint32
	name   string // schema.table, quoted
	plain  string // schema.table
	cols   []synCol
	parent map[string]bool
}

// GenerateSynthetic fills every user table with rows that respect types,
// NOT NULL, foreign keys and (by construction) most unique constraints, for
// T1 rehearsals. Values are generated, never sampled from production:
// counts may come from production statistics (reltuples) via counts.
// Tables that cannot be filled are skipped. It returns rows inserted.
func GenerateSynthetic(ctx context.Context, conn *pgx.Conn, perTable int64, counts map[string]int64) (int64, error) {
	tables, err := synTables(ctx, conn)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, t := range orderTables(tables) {
		n := perTable
		if c, ok := counts[t.plain]; ok {
			n = c
		}
		if n <= 0 {
			continue
		}
		if n > MaxSyntheticRows {
			n = MaxSyntheticRows
		}
		sql, ok := t.insertSQL(n)
		if !ok {
			continue
		}
		if _, err := conn.Exec(ctx, "SAVEPOINT cg_syn"); err != nil {
			// Not in a transaction: run directly.
			if tag, err := conn.Exec(ctx, sql); err == nil {
				total += tag.RowsAffected()
			}
			continue
		}
		if tag, err := conn.Exec(ctx, sql); err == nil {
			total += tag.RowsAffected()
		} else {
			_, _ = conn.Exec(ctx, "ROLLBACK TO SAVEPOINT cg_syn")
		}
	}
	_, _ = conn.Exec(ctx, "ANALYZE")
	return total, nil
}

func synTables(ctx context.Context, conn *pgx.Conn) (map[uint32]*synTable, error) {
	rows, err := conn.Query(ctx, `SELECT c.oid, quote_ident(n.nspname) || '.' || quote_ident(c.relname), n.nspname || '.' || c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND `+userSchemaFilter)
	if err != nil {
		return nil, err
	}
	tables := map[uint32]*synTable{}
	for rows.Next() {
		t := &synTable{parent: map[string]bool{}}
		if err := rows.Scan(&t.oid, &t.name, &t.plain); err != nil {
			rows.Close()
			return nil, err
		}
		tables[t.oid] = t
	}
	rows.Close()
	for _, t := range tables {
		cr, err := conn.Query(ctx, `SELECT a.attname, format_type(a.atttypid, NULL), ty.typtype::text, coalesce(format_type(ty.typelem, NULL), ''),
			a.attnotnull, (d.adbin IS NOT NULL OR a.attidentity <> ''), a.attgenerated <> '',
			CASE WHEN a.atttypmod > 4 AND ty.typname IN ('varchar','bpchar') THEN a.atttypmod - 4 ELSE 0 END,
			coalesce(fk.reltable, ''), coalesce(fk.relcol, '')
			FROM pg_attribute a JOIN pg_type ty ON ty.oid = a.atttypid
			LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			LEFT JOIN LATERAL (
			  SELECT quote_ident(pn.nspname) || '.' || quote_ident(pc.relname) AS reltable, quote_ident(pa.attname) AS relcol
			  FROM pg_constraint co JOIN pg_class pc ON pc.oid = co.confrelid JOIN pg_namespace pn ON pn.oid = pc.relnamespace
			  JOIN pg_attribute pa ON pa.attrelid = co.confrelid AND pa.attnum = co.confkey[1]
			  WHERE co.conrelid = a.attrelid AND co.contype = 'f' AND array_length(co.conkey, 1) = 1 AND co.conkey[1] = a.attnum LIMIT 1) fk ON true
			WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, t.oid)
		if err != nil {
			return nil, err
		}
		for cr.Next() {
			var c synCol
			if err := cr.Scan(&c.name, &c.typ, &c.typtype, &c.elemType, &c.notNull, &c.hasDefault, &c.generated, &c.maxLen, &c.fkTable, &c.fkCol); err != nil {
				cr.Close()
				return nil, err
			}
			if c.typtype == "e" {
				c.enumType = c.typ
			}
			if c.fkTable != "" {
				t.parent[c.fkTable] = true
			}
			t.cols = append(t.cols, c)
		}
		cr.Close()
	}
	return tables, nil
}

// orderTables sorts tables so parents come before children.
func orderTables(tables map[uint32]*synTable) []*synTable {
	byName := map[string]*synTable{}
	var names []string
	for _, t := range tables {
		byName[t.name] = t
		names = append(names, t.name)
	}
	sort.Strings(names)
	var out []*synTable
	seen := map[string]bool{}
	var visit func(n string, depth int)
	visit = func(n string, depth int) {
		if seen[n] || depth > 50 {
			return
		}
		seen[n] = true
		t := byName[n]
		if t == nil {
			return
		}
		for p := range t.parent {
			if p != n {
				visit(p, depth+1)
			}
		}
		out = append(out, t)
	}
	for _, n := range names {
		visit(n, 0)
	}
	return out
}

func scalarExpr(typ string, maxLen int) (string, bool) {
	text := "('v' || g || '_' || substr(md5(g::text), 1, 8))"
	switch typ {
	case "smallint":
		return "(g % 32000)::smallint", true
	case "integer", "bigint", "numeric", "real", "double precision", "oid":
		return "g::" + typ, true
	case "text", "character varying", "character", "citext", "name":
		if maxLen > 0 {
			return fmt.Sprintf("left(%s, %d)", text, maxLen), true
		}
		return text, true
	case "boolean":
		return "(g % 2 = 0)", true
	case "date":
		return "(DATE '2020-01-01' + (g % 3650))", true
	case "timestamp without time zone":
		return "(TIMESTAMP '2020-01-01' + g * interval '1 minute')", true
	case "timestamp with time zone":
		return "(TIMESTAMPTZ '2020-01-01 00:00:00+00' + g * interval '1 minute')", true
	case "time without time zone":
		return "(TIME '00:00' + (g % 86400) * interval '1 second')", true
	case "interval":
		return "(g * interval '1 minute')", true
	case "uuid":
		return "md5(g::text)::uuid", true
	case "json":
		return "json_build_object('n', g)", true
	case "jsonb":
		return "jsonb_build_object('n', g)", true
	case "bytea":
		return "decode(md5(g::text), 'hex')", true
	case "inet", "cidr":
		return "('10.' || (g / 65536) % 256 || '.' || (g / 256) % 256 || '.' || g % 256)::" + typ, true
	case "money":
		return "(g * 1.25)::money", true
	}
	return "", false
}

func (t *synTable) insertSQL(n int64) (string, bool) {
	var cols, exprs, joins []string
	for i, c := range t.cols {
		if c.generated || (c.hasDefault && c.fkTable == "") {
			continue
		}
		var e string
		ok := true
		switch {
		case c.fkTable != "":
			if c.fkTable == t.name {
				if c.notNull {
					return "", false
				}
				continue
			}
			alias := fmt.Sprintf("p%d", i)
			joins = append(joins, fmt.Sprintf("(SELECT array_agg(%s) AS k FROM %s) %s", c.fkCol, c.fkTable, alias))
			e = fmt.Sprintf("%s.k[1 + (g - 1) %% greatest(coalesce(array_length(%s.k, 1), 1), 1)]", alias, alias)
		case c.enumType != "":
			e = fmt.Sprintf("(enum_range(NULL::%s))[1 + g %% array_length(enum_range(NULL::%s), 1)]", c.enumType, c.enumType)
		case strings.HasSuffix(c.typ, "[]"):
			var el string
			el, ok = scalarExpr(c.elemType, 0)
			e = "ARRAY[" + el + "]"
		default:
			e, ok = scalarExpr(c.typ, c.maxLen)
		}
		if !ok {
			if c.notNull && !c.hasDefault {
				return "", false
			}
			continue
		}
		cols = append(cols, QuoteIdent(c.name))
		exprs = append(exprs, e)
	}
	if len(cols) == 0 {
		return fmt.Sprintf("INSERT INTO %s SELECT FROM generate_series(1, %d) g", t.name, n), false
	}
	from := fmt.Sprintf("generate_series(1, %d) g", n)
	for _, j := range joins {
		from += " CROSS JOIN " + j
	}
	return fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s", t.name, strings.Join(cols, ", "), strings.Join(exprs, ", "), from), true
}
