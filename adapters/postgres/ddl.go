package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/changegate/changegate/pkg/adapter"
	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
)

// ddlStatic is the plan-time decision for a DDL statement.
type ddlStatic struct {
	Reversibility adapter.Reversibility
	Soft          string // "drop-relation", "drop-column", "truncate", ""
	Notes         []string
}

var softDropKinds = map[pg.ObjectType]bool{
	pg.ObjectType_OBJECT_TABLE: true, pg.ObjectType_OBJECT_VIEW: true, pg.ObjectType_OBJECT_MATVIEW: true,
	pg.ObjectType_OBJECT_SEQUENCE: true, pg.ObjectType_OBJECT_FOREIGN_TABLE: true,
}

// classifyDDL decides how a DDL statement will be applied and undone.
func classifyDDL(s *Stmt) ddlStatic {
	n := s.Node
	rev := func(r adapter.Reversibility, notes ...string) ddlStatic { return ddlStatic{Reversibility: r, Notes: notes} }
	switch {
	case n.GetCreateStmt() != nil, n.GetCreateTableAsStmt() != nil, n.GetCreateSchemaStmt() != nil,
		n.GetCreateSeqStmt() != nil, n.GetCreateExtensionStmt() != nil, n.GetCreateEnumStmt() != nil,
		n.GetCompositeTypeStmt() != nil, n.GetCreateTrigStmt() != nil:
		return rev(adapter.Reversible, "undo removes the created objects (tables are soft-dropped)")
	case n.GetSelectStmt() != nil && n.GetSelectStmt().IntoClause != nil:
		return rev(adapter.Reversible, "undo soft-drops the created table")
	case n.GetIndexStmt() != nil:
		return rev(adapter.Reversible, "undo drops the index")
	case n.GetViewStmt() != nil:
		return rev(adapter.Reversible, "undo restores the previous view definition or drops the view")
	case n.GetCreateFunctionStmt() != nil:
		if n.GetCreateFunctionStmt().Replace {
			return rev(adapter.Compensatable, "undo restores the previous function body (privileges are not restored)")
		}
		return rev(adapter.Reversible, "undo drops the function")
	case n.GetRenameStmt() != nil:
		if _, ok := invertRename(n.GetRenameStmt()); ok {
			return rev(adapter.Reversible, "undo renames back")
		}
		return rev(adapter.CheckpointOnly, "this rename cannot be inverted automatically")
	case n.GetDropStmt() != nil:
		d := n.GetDropStmt()
		if d.Behavior == pg.DropBehavior_DROP_CASCADE {
			return rev(adapter.CheckpointOnly, "CASCADE drops unlisted dependents; restore requires a checkpoint")
		}
		switch {
		case softDropKinds[d.RemoveType]:
			return ddlStatic{Reversibility: adapter.Reversible, Soft: "drop-relation",
				Notes: []string{"soft-drop: moved to schema " + TrashSchema + " instead of dropped; undo moves it back"}}
		case d.RemoveType == pg.ObjectType_OBJECT_INDEX:
			return rev(adapter.Reversible, "undo re-creates the index from its definition (rebuild cost)")
		case d.RemoveType == pg.ObjectType_OBJECT_SCHEMA:
			return rev(adapter.Reversible, "undo re-creates the (empty) schema")
		case d.RemoveType == pg.ObjectType_OBJECT_FUNCTION || d.RemoveType == pg.ObjectType_OBJECT_PROCEDURE:
			return rev(adapter.Compensatable, "undo re-creates the routine (privileges are not restored)")
		case d.RemoveType == pg.ObjectType_OBJECT_TRIGGER:
			return rev(adapter.Reversible, "undo re-creates the trigger from its definition")
		}
		return rev(adapter.CheckpointOnly, "no automatic inverse for "+s.Kind)
	case n.GetTruncateStmt() != nil:
		t := n.GetTruncateStmt()
		if len(t.Relations) != 1 || t.Behavior == pg.DropBehavior_DROP_CASCADE {
			return rev(adapter.CheckpointOnly, "multi-table or CASCADE truncate requires a checkpoint")
		}
		r := adapter.Reversible
		notes := []string{"soft-truncate: rows are copied to " + TrashSchema + " first; undo copies them back"}
		if t.RestartSeqs {
			r = adapter.Compensatable
			notes = append(notes, "RESTART IDENTITY: sequence values are not restored")
		}
		return ddlStatic{Reversibility: r, Soft: "truncate", Notes: notes}
	case n.GetAlterTableStmt() != nil:
		return classifyAlterDDL(n.GetAlterTableStmt())
	case n.GetGrantStmt() != nil:
		return rev(adapter.Compensatable, "undo issues the opposite GRANT/REVOKE (prior partial privileges may differ)")
	case n.GetDoStmt() != nil, n.GetCallStmt() != nil:
		return rev(adapter.Irreversible, "arbitrary code cannot be previewed or undone")
	}
	return rev(adapter.CheckpointOnly, "no automatic inverse for "+s.Kind)
}

func classifyAlterDDL(a *pg.AlterTableStmt) ddlStatic {
	out := ddlStatic{Reversibility: adapter.Reversible}
	for _, c := range a.Cmds {
		cmd := c.GetAlterTableCmd()
		if cmd == nil {
			continue
		}
		switch cmd.Subtype {
		case pg.AlterTableType_AT_AddColumn, pg.AlterTableType_AT_AddConstraint, pg.AlterTableType_AT_ColumnDefault,
			pg.AlterTableType_AT_DropNotNull, pg.AlterTableType_AT_SetNotNull, pg.AlterTableType_AT_ValidateConstraint,
			pg.AlterTableType_AT_EnableTrig, pg.AlterTableType_AT_DisableTrig, pg.AlterTableType_AT_AddIndex:
		case pg.AlterTableType_AT_DropColumn:
			if cmd.Behavior == pg.DropBehavior_DROP_CASCADE {
				out.Reversibility = adapter.Worst(out.Reversibility, adapter.CheckpointOnly)
				out.Notes = append(out.Notes, "DROP COLUMN CASCADE requires a checkpoint")
				continue
			}
			out.Soft = "drop-column"
			out.Notes = append(out.Notes, "soft-drop: column "+cmd.Name+" is renamed and made nullable instead of dropped; undo renames it back")
		case pg.AlterTableType_AT_DropConstraint:
			if cmd.Behavior == pg.DropBehavior_DROP_CASCADE {
				out.Reversibility = adapter.Worst(out.Reversibility, adapter.CheckpointOnly)
				continue
			}
			out.Notes = append(out.Notes, "undo re-adds constraint "+cmd.Name+" (re-validates rows)")
		case pg.AlterTableType_AT_AlterColumnType:
			out.Reversibility = adapter.Worst(out.Reversibility, adapter.Compensatable)
			out.Notes = append(out.Notes, "undo casts column "+cmd.Name+" back to its previous type (may be lossy)")
		default:
			out.Reversibility = adapter.Worst(out.Reversibility, adapter.CheckpointOnly)
			out.Notes = append(out.Notes, fmt.Sprintf("no automatic inverse for ALTER TABLE subcommand %s", strings.TrimPrefix(cmd.Subtype.String(), "AT_")))
		}
	}
	return out
}

var relRenameTypes = map[pg.ObjectType]bool{
	pg.ObjectType_OBJECT_TABLE: true, pg.ObjectType_OBJECT_INDEX: true, pg.ObjectType_OBJECT_VIEW: true,
	pg.ObjectType_OBJECT_MATVIEW: true, pg.ObjectType_OBJECT_SEQUENCE: true, pg.ObjectType_OBJECT_FOREIGN_TABLE: true,
}

// invertRename returns the SQL that undoes a rename.
func invertRename(r *pg.RenameStmt) (string, bool) {
	inv := proto.Clone(r).(*pg.RenameStmt)
	inv.MissingOk = false
	switch {
	case relRenameTypes[r.RenameType] && r.Relation != nil:
		inv.Relation.Relname, inv.Newname = r.Newname, r.Relation.Relname
	case (r.RenameType == pg.ObjectType_OBJECT_COLUMN || r.RenameType == pg.ObjectType_OBJECT_TABCONSTRAINT ||
		r.RenameType == pg.ObjectType_OBJECT_TRIGGER || r.RenameType == pg.ObjectType_OBJECT_POLICY) && r.Subname != "":
		inv.Subname, inv.Newname = r.Newname, r.Subname
	case r.RenameType == pg.ObjectType_OBJECT_SCHEMA:
		inv.Subname, inv.Newname = r.Newname, r.Subname
	default:
		return "", false
	}
	sql, err := deparseNode(&pg.Node{Node: &pg.Node_RenameStmt{RenameStmt: inv}})
	return sql, err == nil
}

// ddlResult is what applying one DDL statement produced.
type ddlResult struct {
	Inverse []string `json:"inverse"`
	Guards  []guard  `json:"guards,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Applied []string `json:"applied"`
}

// guard is a query that must return true before an inverse is executed.
type guard struct {
	SQL    string `json:"sql"`
	Reason string `json:"reason"`
}

// applyDDL executes one DDL statement (soft-rewritten where applicable) and
// returns its inverse. q may be a transaction or, for non-transactional
// statements, a plain connection.
func applyDDL(ctx context.Context, q querier, s *Stmt, st ddlStatic, trashName func(string) string) (*ddlResult, error) {
	before, err := loadCatalog(ctx, q)
	if err != nil {
		return nil, err
	}
	res := &ddlResult{}
	var modInverse [][]string // inverse units of modifications, in apply order
	n := s.Node
	exec := []string{s.Text}

	switch {
	case n.GetDropStmt() != nil && st.Soft == "drop-relation":
		exec = nil
		d := n.GetDropStmt()
		for _, o := range d.Objects {
			name := nameFromNode(o)
			r, err := resolveRel(ctx, q, quoteDotted(name))
			if err != nil {
				return nil, err
			}
			if r == nil {
				if d.MissingOk {
					continue
				}
				return nil, fmt.Errorf("relation %q does not exist", name)
			}
			trash := trashName(r.Name)
			fwd, inv, err := softDropRelExplicit(ctx, q, *r, trash)
			if err != nil {
				return nil, err
			}
			exec = append(exec, fwd...)
			modInverse = append(modInverse, inv)
			res.Guards = append(res.Guards, guard{SQL: fmt.Sprintf("SELECT to_regclass(%s) IS NULL", QuoteLiteral(r.qualified())),
				Reason: r.display() + " was re-created since the soft-drop"})
			res.Notes = append(res.Notes, fmt.Sprintf("%s moved to %s.%s", r.display(), TrashSchema, trash))
		}
	case n.GetDropStmt() != nil && n.GetDropStmt().RemoveType == pg.ObjectType_OBJECT_INDEX:
		for _, o := range n.GetDropStmt().Objects {
			var def string
			err := q.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`, quoteDotted(nameFromNode(o))).Scan(&def)
			if err == nil && def != "" {
				modInverse = append(modInverse, []string{def})
			}
		}
	case n.GetDropStmt() != nil && n.GetDropStmt().RemoveType == pg.ObjectType_OBJECT_SCHEMA:
		for _, o := range n.GetDropStmt().Objects {
			modInverse = append(modInverse, []string{"CREATE SCHEMA "+QuoteIdent(nameFromNode(o))})
		}
	case n.GetDropStmt() != nil && (n.GetDropStmt().RemoveType == pg.ObjectType_OBJECT_FUNCTION || n.GetDropStmt().RemoveType == pg.ObjectType_OBJECT_PROCEDURE):
		for _, o := range n.GetDropStmt().Objects {
			ow := o.GetObjectWithArgs()
			if ow == nil {
				continue
			}
			sig, err := deparseObjectWithArgs(n.GetDropStmt(), o)
			if err != nil {
				return nil, err
			}
			var def string
			if err := q.QueryRow(ctx, `SELECT pg_get_functiondef(to_regprocedure($1))`, sig).Scan(&def); err != nil {
				return nil, fmt.Errorf("capture definition of %s: %w", sig, err)
			}
			modInverse = append(modInverse, []string{def})
		}
	case n.GetDropStmt() != nil && n.GetDropStmt().RemoveType == pg.ObjectType_OBJECT_TRIGGER:
		for _, o := range n.GetDropStmt().Objects {
			parts := strings.Split(nameFromNode(o), ".")
			if len(parts) < 2 {
				continue
			}
			tg := parts[len(parts)-1]
			rel := quoteDotted(strings.Join(parts[:len(parts)-1], "."))
			var def string
			if err := q.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = $2`, rel, tg).Scan(&def); err == nil {
				modInverse = append(modInverse, []string{def})
			}
		}
	case n.GetTruncateStmt() != nil && st.Soft == "truncate":
		rv := n.GetTruncateStmt().Relations[0].GetRangeVar()
		r, err := resolveRel(ctx, q, QuoteRel(rv))
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, fmt.Errorf("relation %s does not exist", RelName(rv))
		}
		cols, err := columns(ctx, q, r.OID)
		if err != nil {
			return nil, err
		}
		var ins []string
		for _, c := range cols {
			if !c.Generated {
				ins = append(ins, c.Name)
			}
		}
		trash := QuoteIdent(TrashSchema) + "." + QuoteIdent(trashName(r.Name))
		exec = []string{
			"CREATE SCHEMA IF NOT EXISTS " + QuoteIdent(TrashSchema),
			fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM ONLY %s", trash, r.qualified()),
			s.Text,
		}
		modInverse = append(modInverse, []string{
			fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s", r.qualified(), quoteAll(ins, ""), quoteAll(ins, ""), trash),
			"DROP TABLE " + trash})
		res.Notes = append(res.Notes, fmt.Sprintf("rows of %s copied to %s", r.display(), trash))
	case n.GetRenameStmt() != nil:
		if inv, ok := invertRename(n.GetRenameStmt()); ok {
			modInverse = append(modInverse, []string{inv})
		}
	case n.GetGrantStmt() != nil:
		g := proto.Clone(n.GetGrantStmt()).(*pg.GrantStmt)
		g.IsGrant = !g.IsGrant
		g.GrantOption = false
		if inv, err := deparseNode(&pg.Node{Node: &pg.Node_GrantStmt{GrantStmt: g}}); err == nil {
			modInverse = append(modInverse, []string{inv})
		}
	case n.GetViewStmt() != nil && n.GetViewStmt().Replace:
		v := n.GetViewStmt().View
		var def *string
		_ = q.QueryRow(ctx, `SELECT pg_get_viewdef(to_regclass($1))`, QuoteRel(v)).Scan(&def)
		if def != nil {
			modInverse = append(modInverse, []string{fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s", QuoteRel(v), strings.TrimSuffix(strings.TrimSpace(*def), ";"))})
		}
	case n.GetCreateFunctionStmt() != nil && n.GetCreateFunctionStmt().Replace:
		// Capture the previous body if the routine exists with the same signature.
		if sig, err := createFunctionSignature(n.GetCreateFunctionStmt()); err == nil {
			var def *string
			_ = q.QueryRow(ctx, `SELECT pg_get_functiondef(to_regprocedure($1))`, sig).Scan(&def)
			if def != nil {
				modInverse = append(modInverse, []string{*def})
			}
		}
	case n.GetAlterTableStmt() != nil:
		var err error
		exec, modInverse, err = alterTableInverse(ctx, q, s, n.GetAlterTableStmt(), trashName, res)
		if err != nil {
			return nil, err
		}
	}

	for _, sql := range exec {
		if _, err := q.Exec(ctx, sql); err != nil {
			return nil, err
		}
	}
	res.Applied = exec

	after, err := loadCatalog(ctx, q)
	if err != nil {
		return nil, err
	}
	diff := diffCatalog(before, after)
	// Inverse: first remove what was created, then revert modifications in reverse order.
	res.Inverse = diff.inverseOfCreations(trashName)
	for i := len(modInverse) - 1; i >= 0; i-- {
		res.Inverse = append(res.Inverse, modInverse[i]...)
	}
	return res, nil
}

// alterTableInverse handles ALTER TABLE subcommands: it captures prior state
// for each modification and rewrites DROP COLUMN as a soft-drop.
func alterTableInverse(ctx context.Context, q querier, s *Stmt, a *pg.AlterTableStmt, trashName func(string) string, res *ddlResult) (exec []string, modInverse [][]string, err error) {
	r, err := resolveRel(ctx, q, QuoteRel(a.Relation))
	if err != nil {
		return nil, nil, err
	}
	if r == nil {
		if a.MissingOk {
			return []string{s.Text}, nil, nil
		}
		return nil, nil, fmt.Errorf("relation %s does not exist", RelName(a.Relation))
	}
	cols, err := columns(ctx, q, r.OID)
	if err != nil {
		return nil, nil, err
	}
	colByName := map[string]column{}
	for _, c := range cols {
		colByName[c.Name] = c
	}
	tbl := r.qualified()
	rest := proto.Clone(a).(*pg.AlterTableStmt)
	rest.Cmds = nil
	for _, c := range a.Cmds {
		cmd := c.GetAlterTableCmd()
		if cmd == nil {
			continue
		}
		col := colByName[cmd.Name]
		switch cmd.Subtype {
		case pg.AlterTableType_AT_DropColumn:
			if cmd.Behavior == pg.DropBehavior_DROP_CASCADE {
				rest.Cmds = append(rest.Cmds, c)
				continue
			}
			if _, ok := colByName[cmd.Name]; !ok {
				if cmd.MissingOk {
					continue
				}
				return nil, nil, fmt.Errorf("column %q of relation %s does not exist", cmd.Name, r.display())
			}
			trash := trashName(cmd.Name)
			exec = append(exec, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", tbl, QuoteIdent(cmd.Name), QuoteIdent(trash)))
			unit := []string{fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", tbl, QuoteIdent(trash), QuoteIdent(cmd.Name))}
			if col.NotNull {
				exec = append(exec, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", tbl, QuoteIdent(trash)))
				unit = append(unit, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", tbl, QuoteIdent(cmd.Name)))
			}
			modInverse = append(modInverse, unit)
			res.Guards = append(res.Guards, guard{
				SQL:    fmt.Sprintf("SELECT NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = %d AND attname = %s AND NOT attisdropped)", r.OID, QuoteLiteral(cmd.Name)),
				Reason: "a column named " + cmd.Name + " was re-added since the soft-drop"})
			res.Notes = append(res.Notes, fmt.Sprintf("column %s.%s renamed to %s", r.display(), cmd.Name, trash))
			continue
		case pg.AlterTableType_AT_ColumnDefault:
			if col.Default != "" {
				modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", tbl, QuoteIdent(cmd.Name), col.Default)})
			} else {
				modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", tbl, QuoteIdent(cmd.Name))})
			}
		case pg.AlterTableType_AT_SetNotNull, pg.AlterTableType_AT_DropNotNull:
			if col.NotNull {
				modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", tbl, QuoteIdent(cmd.Name))})
			} else {
				modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", tbl, QuoteIdent(cmd.Name))})
			}
		case pg.AlterTableType_AT_AlterColumnType:
			unit := []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", tbl, QuoteIdent(cmd.Name)),
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s", tbl, QuoteIdent(cmd.Name), col.Type, QuoteIdent(cmd.Name), col.Type)}
			if col.Default != "" {
				unit = append(unit, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", tbl, QuoteIdent(cmd.Name), col.Default))
			}
			modInverse = append(modInverse, unit)
		case pg.AlterTableType_AT_DropConstraint:
			var def *string
			_ = q.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1 AND conname = $2`, r.OID, cmd.Name).Scan(&def)
			if def != nil {
				modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s", tbl, QuoteIdent(cmd.Name), *def)})
			}
		case pg.AlterTableType_AT_EnableTrig:
			modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s DISABLE TRIGGER %s", tbl, QuoteIdent(cmd.Name))})
		case pg.AlterTableType_AT_DisableTrig:
			modInverse = append(modInverse, []string{fmt.Sprintf("ALTER TABLE %s ENABLE TRIGGER %s", tbl, QuoteIdent(cmd.Name))})
		}
		rest.Cmds = append(rest.Cmds, c)
	}
	if len(rest.Cmds) > 0 {
		if len(rest.Cmds) == len(a.Cmds) {
			exec = append(exec, s.Text)
		} else {
			sql, err := deparseNode(&pg.Node{Node: &pg.Node_AlterTableStmt{AlterTableStmt: rest}})
			if err != nil {
				return nil, nil, err
			}
			exec = append(exec, sql)
		}
	}
	return exec, modInverse, nil
}

// quoteDotted quotes each part of a dotted name.
func quoteDotted(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = QuoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// deparseObjectWithArgs renders "schema.fn(argtypes)" for a DROP FUNCTION object.
func deparseObjectWithArgs(d *pg.DropStmt, o *pg.Node) (string, error) {
	one := proto.Clone(d).(*pg.DropStmt)
	one.Objects = []*pg.Node{o}
	one.MissingOk = false
	one.Behavior = pg.DropBehavior_DROP_RESTRICT
	sql, err := deparseNode(&pg.Node{Node: &pg.Node_DropStmt{DropStmt: one}})
	if err != nil {
		return "", err
	}
	for _, p := range []string{"DROP FUNCTION ", "DROP PROCEDURE "} {
		sql = strings.TrimPrefix(sql, p)
	}
	return sql, nil
}

func createFunctionSignature(f *pg.CreateFunctionStmt) (string, error) {
	var name []string
	for _, x := range f.Funcname {
		name = append(name, QuoteIdent(x.GetString_().GetSval()))
	}
	var args []string
	for _, p := range f.Parameters {
		fp := p.GetFunctionParameter()
		if fp == nil || fp.Mode == pg.FunctionParameterMode_FUNC_PARAM_OUT || fp.Mode == pg.FunctionParameterMode_FUNC_PARAM_TABLE {
			continue
		}
		tn := &pg.Node{Node: &pg.Node_TypeName{TypeName: fp.ArgType}}
		// Render the type by deparsing a cast.
		sel, err := deparseNode(&pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: &pg.SelectStmt{
			TargetList: []*pg.Node{{Node: &pg.Node_ResTarget{ResTarget: &pg.ResTarget{Val: &pg.Node{Node: &pg.Node_TypeCast{TypeCast: &pg.TypeCast{
				Arg: &pg.Node{Node: &pg.Node_AConst{AConst: &pg.A_Const{Isnull: true}}}, TypeName: tn.GetTypeName()}}}}}}},
		}}})
		if err != nil {
			return "", err
		}
		sel = strings.TrimPrefix(sel, "SELECT NULL::")
		args = append(args, sel)
	}
	return strings.Join(name, ".") + "(" + strings.Join(args, ", ") + ")", nil
}
