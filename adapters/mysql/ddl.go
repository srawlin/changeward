package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/srawlin/changeward/pkg/adapter"
)

func exists(ctx context.Context, q queryer, schema, table string) bool {
	return tableSignature(ctx, q, schema, table) != "absent"
}

func schemaOf(ctx context.Context, q queryer, schema string) string {
	if schema != "" {
		return schema
	}
	_ = q.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema)
	return schema
}

func trashName(db, table, planID string) string {
	n := fmt.Sprintf("%s__%s_%s", db, strings.TrimPrefix(planID, "p_")[:6], table)
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// planDDL statically validates a DDL statement and decides its reversibility.
func (a *Adapter) planDDL(ctx context.Context, q queryer, s *Stmt) (planStep, []string, error) {
	st := planStep{SQL: s.Text, Kind: s.Kind, Category: s.Category, Reversibility: adapter.CheckpointOnly}
	if s.Table != "" {
		st.Signature = tableSignature(ctx, q, s.Schema, s.Table)
	}
	var notes []string
	switch n := s.Node.(type) {
	case *ast.CreateTableStmt:
		if exists(ctx, q, s.Schema, s.Table) && !n.IfNotExists {
			return st, nil, fmt.Errorf("table %s already exists", s.Table)
		}
		st.Reversibility = adapter.Reversible
		notes = append(notes, "undo moves the new table to "+TrashDB)
	case *ast.DropTableStmt:
		if len(n.Tables) != 1 || n.IsView || n.TemporaryKeyword != ast.TemporaryNone {
			notes = append(notes, "only single-table DROP TABLE is soft-dropped")
			break
		}
		if !exists(ctx, q, s.Schema, s.Table) {
			if n.IfExists {
				st.Reversibility = adapter.Reversible
				break
			}
			return st, nil, fmt.Errorf("table %s does not exist", s.Table)
		}
		var refs int
		_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE UNIQUE_CONSTRAINT_SCHEMA = ? AND REFERENCED_TABLE_NAME = ? AND TABLE_NAME <> ?`,
			schemaOf(ctx, q, s.Schema), s.Table, s.Table).Scan(&refs)
		if refs > 0 {
			return st, nil, fmt.Errorf("cannot drop table %s: referenced by %d foreign key(s)", s.Table, refs)
		}
		st.Reversibility = adapter.Reversible
		notes = append(notes, "soft-drop: renamed into "+TrashDB+" instead of dropped; undo renames it back")
	case *ast.TruncateTableStmt:
		if !exists(ctx, q, s.Schema, s.Table) {
			return st, nil, fmt.Errorf("table %s does not exist", s.Table)
		}
		st.Reversibility = adapter.Reversible
		notes = append(notes, "soft-truncate: rows are copied into "+TrashDB+" first; undo copies them back")
	case *ast.CreateIndexStmt:
		st.Reversibility = adapter.Reversible
		notes = append(notes, "undo drops the index")
	case *ast.RenameTableStmt:
		if len(n.TableToTables) == 1 {
			st.Reversibility = adapter.Reversible
			notes = append(notes, "undo renames back")
		}
	case *ast.AlterTableStmt:
		if !exists(ctx, q, s.Schema, s.Table) {
			return st, nil, fmt.Errorf("table %s does not exist", s.Table)
		}
		st.Reversibility = adapter.Reversible
		for _, sp := range n.Specs {
			switch sp.Tp {
			case ast.AlterTableAddColumns:
			case ast.AlterTableAddConstraint:
				if sp.Constraint == nil || sp.Constraint.Name == "" {
					st.Reversibility = adapter.CheckpointOnly
					notes = append(notes, "unnamed constraints cannot be dropped reliably; name the constraint to make this reversible")
				}
			case ast.AlterTableDropColumn:
				if len(n.Specs) != 1 {
					st.Reversibility = adapter.CheckpointOnly
					notes = append(notes, "DROP COLUMN combined with other changes cannot be soft-dropped")
				} else {
					notes = append(notes, "soft-drop: column renamed and made nullable; undo restores its exact definition")
				}
			default:
				st.Reversibility = adapter.CheckpointOnly
				notes = append(notes, "no automatic inverse for this ALTER TABLE change")
			}
		}
	default:
		notes = append(notes, "no automatic inverse for "+s.Kind)
	}
	return st, notes, nil
}

func columnDef(ctx context.Context, q queryer, schema, table, col string) (string, error) {
	var tn, ddl string
	if err := q.QueryRowContext(ctx, "SHOW CREATE TABLE "+qi(schema)+"."+qi(table)).Scan(&tn, &ddl); err != nil {
		return "", err
	}
	prefix := qi(col) + " "
	for _, line := range strings.Split(ddl, "\n") {
		l := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if strings.HasPrefix(l, prefix) {
			return l, nil
		}
	}
	return "", fmt.Errorf("column %s not found in %s", col, table)
}

func columnType(ctx context.Context, q queryer, schema, table, col string) (string, error) {
	var ct string
	err := q.QueryRowContext(ctx, `SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, schema, table, col).Scan(&ct)
	return ct, err
}

// applyDDL runs a DDL statement (soft where possible) and returns its inverse.
func (a *Adapter) applyDDL(ctx context.Context, d *sql.DB, s *Stmt, st planStep, planID string) (undoStep, error) {
	if s.Table != "" && tableSignature(ctx, d, s.Schema, s.Table) != st.Signature {
		return undoStep{}, &adapter.ConflictError{Reason: "schema changed since plan; re-plan required", Conflicts: []string{s.Table}}
	}
	schema := schemaOf(ctx, d, s.Schema)
	full := qi(schema) + "." + qi(s.Table)
	exec := func(stmts ...string) error {
		for _, x := range stmts {
			if _, err := d.ExecContext(ctx, x); err != nil {
				return fmt.Errorf("%s: %w", firstLine(x), err)
			}
		}
		return nil
	}
	u := undoStep{Kind: "ddl"}
	switch n := s.Node.(type) {
	case *ast.CreateTableStmt:
		existed := exists(ctx, d, s.Schema, s.Table)
		if err := exec(s.Text); err != nil {
			return u, err
		}
		if !existed {
			trash := qi(TrashDB) + "." + qi(trashName(schema, s.Table, planID))
			u.Inverse = []string{"CREATE DATABASE IF NOT EXISTS " + qi(TrashDB), "RENAME TABLE " + full + " TO " + trash}
		}
	case *ast.DropTableStmt:
		if st.Reversibility != adapter.Reversible || !exists(ctx, d, s.Schema, s.Table) {
			return u, exec(s.Text)
		}
		trash := qi(TrashDB) + "." + qi(trashName(schema, s.Table, planID))
		if err := exec("CREATE DATABASE IF NOT EXISTS "+qi(TrashDB), "RENAME TABLE "+full+" TO "+trash); err != nil {
			return u, err
		}
		u.Guards = []string{fmt.Sprintf("SELECT COUNT(*) = 0 FROM information_schema.TABLES WHERE TABLE_SCHEMA = '%s' AND TABLE_NAME = '%s'", sqlStr(schema), sqlStr(s.Table))}
		u.Inverse = []string{"RENAME TABLE " + trash + " TO " + full}
	case *ast.TruncateTableStmt:
		trash := qi(TrashDB) + "." + qi(trashName(schema, s.Table, planID))
		ti, err := loadTable(ctx, d, schema, s.Table)
		if err != nil {
			return u, err
		}
		var cols []string
		for _, c := range ti.settable() {
			cols = append(cols, qi(c.Name))
		}
		cl := strings.Join(cols, ", ")
		if err := exec("CREATE DATABASE IF NOT EXISTS "+qi(TrashDB), "CREATE TABLE "+trash+" LIKE "+full,
			"INSERT INTO "+trash+" ("+cl+") SELECT "+cl+" FROM "+full, s.Text); err != nil {
			return u, err
		}
		u.Inverse = []string{"INSERT INTO " + full + " (" + cl + ") SELECT " + cl + " FROM " + trash, "DROP TABLE " + trash}
	case *ast.CreateIndexStmt:
		if err := exec(s.Text); err != nil {
			return u, err
		}
		u.Inverse = []string{"DROP INDEX " + qi(n.IndexName) + " ON " + full}
	case *ast.RenameTableStmt:
		if err := exec(s.Text); err != nil {
			return u, err
		}
		if len(n.TableToTables) == 1 {
			tt := n.TableToTables[0]
			u.Inverse = []string{"RENAME TABLE " + qtn(ctx, d, tt.NewTable) + " TO " + qtn(ctx, d, tt.OldTable)}
		}
	case *ast.AlterTableStmt:
		if len(n.Specs) == 1 && n.Specs[0].Tp == ast.AlterTableDropColumn && st.Reversibility == adapter.Reversible {
			col := n.Specs[0].OldColumnName.Name.O
			def, err := columnDef(ctx, d, schema, s.Table, col)
			if err != nil {
				return u, err
			}
			ct, err := columnType(ctx, d, schema, s.Table, col)
			if err != nil {
				return u, err
			}
			trashCol := "_cw_" + strings.TrimPrefix(planID, "p_")[:6] + "_" + col
			if len(trashCol) > 64 {
				trashCol = trashCol[:64]
			}
			if err := exec(fmt.Sprintf("ALTER TABLE %s CHANGE COLUMN %s %s %s NULL", full, qi(col), qi(trashCol), ct)); err != nil {
				return u, err
			}
			u.Inverse = []string{fmt.Sprintf("ALTER TABLE %s CHANGE COLUMN %s %s", full, qi(trashCol), def)}
			break
		}
		if err := exec(s.Text); err != nil {
			return u, err
		}
		if st.Reversibility != adapter.Reversible {
			break
		}
		var inv []string
		for i := len(n.Specs) - 1; i >= 0; i-- {
			sp := n.Specs[i]
			switch sp.Tp {
			case ast.AlterTableAddColumns:
				for _, c := range sp.NewColumns {
					inv = append(inv, "ALTER TABLE "+full+" DROP COLUMN "+qi(c.Name.Name.O))
				}
			case ast.AlterTableAddConstraint:
				if sp.Constraint != nil && sp.Constraint.Name != "" {
					switch sp.Constraint.Tp {
					case ast.ConstraintForeignKey:
						inv = append(inv, "ALTER TABLE "+full+" DROP FOREIGN KEY "+qi(sp.Constraint.Name))
					case ast.ConstraintCheck:
						inv = append(inv, "ALTER TABLE "+full+" DROP CHECK "+qi(sp.Constraint.Name))
					default:
						inv = append(inv, "ALTER TABLE "+full+" DROP INDEX "+qi(sp.Constraint.Name))
					}
				}
			}
		}
		u.Inverse = inv
	default:
		return undoStep{Kind: "none"}, exec(s.Text)
	}
	return u, nil
}

func qtn(ctx context.Context, q queryer, t *ast.TableName) string {
	return qi(schemaOf(ctx, q, t.Schema.O)) + "." + qi(t.Name.O)
}

func sqlStr(s string) string { return strings.ReplaceAll(s, "'", "''") }
