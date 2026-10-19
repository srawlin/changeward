package postgres

import (
	"fmt"
	"sort"
	"strings"

	"github.com/srawlin/changegate/pkg/adapter"
	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Statement categories.
const (
	CatRead    = "read"
	CatDML     = "dml"
	CatDDL     = "ddl"
	CatUtility = "utility"
	CatTx      = "transaction"
)

// Risk flags raised by the classifier (Squawk-style static rules plus
// agent-specific ones). They feed policy (change.flags) and reports.
const (
	FlagDeleteWithoutWhere   = "delete-without-where"
	FlagUpdateWithoutWhere   = "update-without-where"
	FlagDropTable            = "drop-table"
	FlagDropColumn           = "drop-column"
	FlagDropSchema           = "drop-schema"
	FlagDropDatabase         = "drop-database"
	FlagDropIndex            = "drop-index"
	FlagDropOther            = "drop-object"
	FlagTruncate             = "truncate"
	FlagCascade              = "cascade"
	FlagNonConcurrentIndex   = "non-concurrent-index"
	FlagConcurrent           = "concurrent-not-transactional"
	FlagAlterColumnType      = "alter-column-type"
	FlagSetNotNull           = "set-not-null"
	FlagVolatileDefault      = "add-column-volatile-default"
	FlagAddColumnNotNull     = "add-column-not-null-without-default"
	FlagFKWithoutNotValid    = "foreign-key-without-not-valid"
	FlagCheckWithoutNotValid = "check-without-not-valid"
	FlagAddUnique            = "add-unique-constraint"
	FlagAddPrimaryKey        = "add-primary-key"
	FlagRenameTable          = "rename-table"
	FlagRenameColumn         = "rename-column"
	FlagPrivilege            = "privilege-change"
	FlagMultiTable           = "multi-table-dml"
	FlagWithClause           = "dml-with-cte"
	FlagUpsert               = "upsert"
	FlagTxControl            = "transaction-control"
	FlagSessionSetting       = "session-setting"
	FlagCodeBlock            = "anonymous-code-block"
	FlagProcedureCall        = "procedure-call"
	FlagSideEffectFunction   = "side-effect-function"
	FlagCallsFunctions       = "calls-functions"
	FlagHeavyLock            = "heavy-lock"
	FlagServerFile           = "server-file-access"
	FlagDisablesTriggers     = "disables-triggers"
	FlagUnparsed             = "unparsed"
)

// sideEffectFuncs have effects that a rolled-back transaction does not undo,
// or that bypass read-only checks.
var sideEffectFuncs = map[string]bool{
	"pg_terminate_backend": true, "pg_cancel_backend": true, "pg_notify": true,
	"dblink": true, "dblink_exec": true, "dblink_send_query": true,
	"lo_import": true, "lo_export": true, "pg_read_file": true, "pg_read_binary_file": true,
	"pg_ls_dir": true, "pg_reload_conf": true, "pg_rotate_logfile": true,
	"pg_advisory_lock": true, "pg_advisory_xact_lock": true, "nextval": true, "setval": true,
	"pg_switch_wal": true, "pg_create_restore_point": true, "pg_promote": true,
	"http_get": true, "http_post": true, "http": true, "pg_sleep": true,
}

// Stmt is one parsed, classified statement.
type Stmt struct {
	Text          string
	Kind          string
	Category      string
	Access        adapter.Access
	Objects       []string
	Flags         []string
	Transactional bool
	Node          *pg.Node
	// Rel is the primary relation for DML and ALTER/CREATE TABLE/INDEX.
	Rel *pg.RangeVar
}

func (s *Stmt) flag(f string) {
	for _, x := range s.Flags {
		if x == f {
			return
		}
	}
	s.Flags = append(s.Flags, f)
}

// HasFlag reports whether f was raised.
func (s *Stmt) HasFlag(f string) bool {
	for _, x := range s.Flags {
		if x == f {
			return true
		}
	}
	return false
}

func (s *Stmt) obj(o string) {
	if o == "" {
		return
	}
	for _, x := range s.Objects {
		if x == o {
			return
		}
	}
	s.Objects = append(s.Objects, o)
}

// RelName renders a RangeVar as written (schema-qualified if it was).
func RelName(rv *pg.RangeVar) string {
	if rv == nil {
		return ""
	}
	if rv.Schemaname != "" {
		return rv.Schemaname + "." + rv.Relname
	}
	return rv.Relname
}

// QuoteRel renders a RangeVar as a safely quoted identifier.
func QuoteRel(rv *pg.RangeVar) string {
	if rv.Schemaname != "" {
		return QuoteIdent(rv.Schemaname) + "." + QuoteIdent(rv.Relname)
	}
	return QuoteIdent(rv.Relname)
}

// QuoteIdent quotes a Postgres identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteLiteral quotes a Postgres string literal.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func nameFromNode(n *pg.Node) string {
	if l := n.GetList(); l != nil {
		var parts []string
		for _, it := range l.Items {
			if s := it.GetString_(); s != nil {
				parts = append(parts, s.Sval)
			}
		}
		return strings.Join(parts, ".")
	}
	if s := n.GetString_(); s != nil {
		return s.Sval
	}
	if tn := n.GetTypeName(); tn != nil {
		var parts []string
		for _, it := range tn.Names {
			if s := it.GetString_(); s != nil {
				parts = append(parts, s.Sval)
			}
		}
		return strings.Join(parts, ".")
	}
	if ow := n.GetObjectWithArgs(); ow != nil {
		var parts []string
		for _, it := range ow.Objname {
			if s := it.GetString_(); s != nil {
				parts = append(parts, s.Sval)
			}
		}
		return strings.Join(parts, ".")
	}
	return ""
}

var objectTypeNames = map[pg.ObjectType]string{
	pg.ObjectType_OBJECT_TABLE: "TABLE", pg.ObjectType_OBJECT_INDEX: "INDEX", pg.ObjectType_OBJECT_VIEW: "VIEW",
	pg.ObjectType_OBJECT_MATVIEW: "MATERIALIZED VIEW", pg.ObjectType_OBJECT_SCHEMA: "SCHEMA",
	pg.ObjectType_OBJECT_SEQUENCE: "SEQUENCE", pg.ObjectType_OBJECT_FUNCTION: "FUNCTION",
	pg.ObjectType_OBJECT_PROCEDURE: "PROCEDURE", pg.ObjectType_OBJECT_TYPE: "TYPE", pg.ObjectType_OBJECT_DOMAIN: "DOMAIN",
	pg.ObjectType_OBJECT_TRIGGER: "TRIGGER", pg.ObjectType_OBJECT_EXTENSION: "EXTENSION",
	pg.ObjectType_OBJECT_DATABASE: "DATABASE", pg.ObjectType_OBJECT_ROLE: "ROLE", pg.ObjectType_OBJECT_COLUMN: "COLUMN",
	pg.ObjectType_OBJECT_FOREIGN_TABLE: "FOREIGN TABLE", pg.ObjectType_OBJECT_POLICY: "POLICY", pg.ObjectType_OBJECT_RULE: "RULE",
}

func objTypeName(t pg.ObjectType) string {
	if n, ok := objectTypeNames[t]; ok {
		return n
	}
	return strings.TrimPrefix(t.String(), "OBJECT_")
}

// Parse splits and classifies a SQL script.
func Parse(sql string) ([]*Stmt, error) {
	tree, err := pg.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse SQL: %w", err)
	}
	var out []*Stmt
	for _, raw := range tree.Stmts {
		start := int(raw.StmtLocation)
		end := len(sql)
		if raw.StmtLen > 0 {
			end = start + int(raw.StmtLen)
		}
		text := strings.TrimSpace(sql[start:end])
		s := &Stmt{Text: text, Node: raw.Stmt, Transactional: true}
		classify(s)
		out = append(out, s)
	}
	// A non-concurrent index on a table created in the same script is fine.
	created := map[string]bool{}
	for _, s := range out {
		if s.Kind == "CREATE TABLE" && s.Rel != nil {
			created[RelName(s.Rel)] = true
		}
		if s.Kind == "CREATE INDEX" && s.Rel != nil && created[RelName(s.Rel)] {
			s.Flags = removeFlag(s.Flags, FlagNonConcurrentIndex)
		}
	}
	return out, nil
}

func removeFlag(fs []string, f string) []string {
	var out []string
	for _, x := range fs {
		if x != f {
			out = append(out, x)
		}
	}
	return out
}

func classify(s *Stmt) {
	n := s.Node
	switch {
	case n.GetSelectStmt() != nil:
		sel := n.GetSelectStmt()
		s.Kind, s.Category, s.Access = "SELECT", CatRead, adapter.Read
		if sel.IntoClause != nil {
			s.Kind, s.Category, s.Access = "SELECT INTO", CatDDL, adapter.Write
			s.obj(RelName(sel.IntoClause.Rel))
		}
		if len(sel.LockingClause) > 0 {
			s.flag("row-locks")
		}
		walkReads(s, n)
	case n.GetExplainStmt() != nil:
		s.Kind, s.Category, s.Access = "EXPLAIN", CatRead, adapter.Read
		inner := &Stmt{Node: n.GetExplainStmt().Query, Transactional: true}
		classify(inner)
		for _, o := range n.GetExplainStmt().Options {
			if d := o.GetDefElem(); d != nil && d.Defname == "analyze" && inner.Access != adapter.Read {
				s.Access = inner.Access // EXPLAIN ANALYZE executes the statement
				s.Category = inner.Category
				s.Flags = append(s.Flags, inner.Flags...)
			}
		}
		s.Objects = inner.Objects
	case n.GetInsertStmt() != nil:
		ins := n.GetInsertStmt()
		s.Kind, s.Category, s.Access, s.Rel = "INSERT", CatDML, adapter.Write, ins.Relation
		s.obj(RelName(ins.Relation))
		if ins.OnConflictClause != nil {
			s.flag(FlagUpsert)
		}
		if ins.WithClause != nil {
			s.flag(FlagWithClause)
		}
		walkFuncs(s, n)
	case n.GetUpdateStmt() != nil:
		up := n.GetUpdateStmt()
		s.Kind, s.Category, s.Access, s.Rel = "UPDATE", CatDML, adapter.Write, up.Relation
		s.obj(RelName(up.Relation))
		if up.WhereClause == nil {
			s.flag(FlagUpdateWithoutWhere)
		}
		if len(up.FromClause) > 0 {
			s.flag(FlagMultiTable)
		}
		if up.WithClause != nil {
			s.flag(FlagWithClause)
		}
		walkFuncs(s, n)
	case n.GetDeleteStmt() != nil:
		del := n.GetDeleteStmt()
		s.Kind, s.Category, s.Access, s.Rel = "DELETE", CatDML, adapter.Destructive, del.Relation
		s.obj(RelName(del.Relation))
		if del.WhereClause == nil {
			s.flag(FlagDeleteWithoutWhere)
		}
		if len(del.UsingClause) > 0 {
			s.flag(FlagMultiTable)
		}
		if del.WithClause != nil {
			s.flag(FlagWithClause)
		}
		walkFuncs(s, n)
	case n.GetMergeStmt() != nil:
		m := n.GetMergeStmt()
		s.Kind, s.Category, s.Access, s.Rel = "MERGE", CatDML, adapter.Destructive, m.Relation
		s.obj(RelName(m.Relation))
		walkFuncs(s, n)
	case n.GetCreateStmt() != nil:
		c := n.GetCreateStmt()
		s.Kind, s.Category, s.Access, s.Rel = "CREATE TABLE", CatDDL, adapter.Write, c.Relation
		s.obj(RelName(c.Relation))
	case n.GetCreateTableAsStmt() != nil:
		c := n.GetCreateTableAsStmt()
		s.Kind, s.Category, s.Access = "CREATE TABLE AS", CatDDL, adapter.Write
		if c.Into != nil {
			s.Rel = c.Into.Rel
			s.obj(RelName(c.Into.Rel))
		}
		if c.Objtype == pg.ObjectType_OBJECT_MATVIEW {
			s.Kind = "CREATE MATERIALIZED VIEW"
		}
	case n.GetIndexStmt() != nil:
		ix := n.GetIndexStmt()
		s.Kind, s.Category, s.Access, s.Rel = "CREATE INDEX", CatDDL, adapter.Write, ix.Relation
		s.obj(RelName(ix.Relation))
		if ix.Idxname != "" {
			if ix.Relation.Schemaname != "" {
				s.obj(ix.Relation.Schemaname + "." + ix.Idxname)
			} else {
				s.obj(ix.Idxname)
			}
		}
		if ix.Concurrent {
			s.Transactional = false
			s.flag(FlagConcurrent)
		} else {
			s.flag(FlagNonConcurrentIndex)
		}
	case n.GetAlterTableStmt() != nil:
		classifyAlter(s, n.GetAlterTableStmt())
	case n.GetDropStmt() != nil:
		d := n.GetDropStmt()
		s.Kind, s.Category, s.Access = "DROP "+objTypeName(d.RemoveType), CatDDL, adapter.Destructive
		for _, o := range d.Objects {
			s.obj(nameFromNode(o))
		}
		if len(d.Objects) == 1 && d.RemoveType == pg.ObjectType_OBJECT_TABLE {
			if parts := strings.Split(nameFromNode(d.Objects[0]), "."); len(parts) <= 2 {
				s.Rel = &pg.RangeVar{Relname: parts[len(parts)-1]}
				if len(parts) == 2 {
					s.Rel.Schemaname = parts[0]
				}
			}
		}
		switch d.RemoveType {
		case pg.ObjectType_OBJECT_TABLE:
			s.flag(FlagDropTable)
		case pg.ObjectType_OBJECT_SCHEMA:
			s.flag(FlagDropSchema)
		case pg.ObjectType_OBJECT_DATABASE:
			s.flag(FlagDropDatabase)
			s.Transactional = false
		case pg.ObjectType_OBJECT_INDEX:
			s.flag(FlagDropIndex)
		default:
			s.flag(FlagDropOther)
		}
		if d.Concurrent {
			s.Transactional = false
			s.flag(FlagConcurrent)
		}
		if d.Behavior == pg.DropBehavior_DROP_CASCADE {
			s.flag(FlagCascade)
		}
	case n.GetDropdbStmt() != nil:
		s.Kind, s.Category, s.Access, s.Transactional = "DROP DATABASE", CatDDL, adapter.Destructive, false
		s.obj(n.GetDropdbStmt().Dbname)
		s.flag(FlagDropDatabase)
	case n.GetTruncateStmt() != nil:
		t := n.GetTruncateStmt()
		s.Kind, s.Category, s.Access = "TRUNCATE", CatDDL, adapter.Destructive
		for _, r := range t.Relations {
			s.obj(RelName(r.GetRangeVar()))
		}
		if len(t.Relations) == 1 {
			s.Rel = t.Relations[0].GetRangeVar()
		}
		s.flag(FlagTruncate)
		if t.Behavior == pg.DropBehavior_DROP_CASCADE {
			s.flag(FlagCascade)
		}
	case n.GetRenameStmt() != nil:
		r := n.GetRenameStmt()
		s.Kind, s.Category, s.Access, s.Rel = "RENAME "+objTypeName(r.RenameType), CatDDL, adapter.Write, r.Relation
		if r.Relation != nil {
			s.obj(RelName(r.Relation))
		} else if r.Object != nil {
			s.obj(nameFromNode(r.Object))
		}
		switch r.RenameType {
		case pg.ObjectType_OBJECT_TABLE:
			s.flag(FlagRenameTable)
		case pg.ObjectType_OBJECT_COLUMN:
			s.flag(FlagRenameColumn)
		}
	case n.GetViewStmt() != nil:
		v := n.GetViewStmt()
		s.Kind, s.Category, s.Access, s.Rel = "CREATE VIEW", CatDDL, adapter.Write, v.View
		if v.Replace {
			s.Kind = "CREATE OR REPLACE VIEW"
		}
		s.obj(RelName(v.View))
	case n.GetCreateSchemaStmt() != nil:
		s.Kind, s.Category, s.Access = "CREATE SCHEMA", CatDDL, adapter.Write
		s.obj(n.GetCreateSchemaStmt().Schemaname)
	case n.GetCreateSeqStmt() != nil:
		s.Kind, s.Category, s.Access, s.Rel = "CREATE SEQUENCE", CatDDL, adapter.Write, n.GetCreateSeqStmt().Sequence
		s.obj(RelName(n.GetCreateSeqStmt().Sequence))
	case n.GetCreateFunctionStmt() != nil:
		f := n.GetCreateFunctionStmt()
		s.Kind, s.Category, s.Access = "CREATE FUNCTION", CatDDL, adapter.Write
		if f.IsProcedure {
			s.Kind = "CREATE PROCEDURE"
		}
		var parts []string
		for _, x := range f.Funcname {
			parts = append(parts, x.GetString_().GetSval())
		}
		s.obj(strings.Join(parts, "."))
	case n.GetCreateExtensionStmt() != nil:
		s.Kind, s.Category, s.Access = "CREATE EXTENSION", CatDDL, adapter.Write
		s.obj(n.GetCreateExtensionStmt().Extname)
	case n.GetCreateEnumStmt() != nil:
		s.Kind, s.Category, s.Access = "CREATE TYPE", CatDDL, adapter.Write
	case n.GetCompositeTypeStmt() != nil:
		s.Kind, s.Category, s.Access = "CREATE TYPE", CatDDL, adapter.Write
	case n.GetAlterEnumStmt() != nil:
		s.Kind, s.Category, s.Access = "ALTER TYPE", CatDDL, adapter.Write
	case n.GetCreateTrigStmt() != nil:
		t := n.GetCreateTrigStmt()
		s.Kind, s.Category, s.Access, s.Rel = "CREATE TRIGGER", CatDDL, adapter.Write, t.Relation
		s.obj(RelName(t.Relation))
	case n.GetCommentStmt() != nil:
		s.Kind, s.Category, s.Access = "COMMENT", CatDDL, adapter.Write
	case n.GetGrantStmt() != nil:
		g := n.GetGrantStmt()
		s.Kind, s.Category, s.Access = "GRANT", CatDDL, adapter.Write
		if !g.IsGrant {
			s.Kind = "REVOKE"
		}
		for _, o := range g.Objects {
			if rv := o.GetRangeVar(); rv != nil {
				s.obj(RelName(rv))
			} else {
				s.obj(nameFromNode(o))
			}
		}
		s.flag(FlagPrivilege)
	case n.GetGrantRoleStmt() != nil, n.GetCreateRoleStmt() != nil, n.GetAlterRoleStmt() != nil, n.GetDropRoleStmt() != nil:
		s.Kind, s.Category, s.Access = "ROLE", CatDDL, adapter.Destructive
		s.flag(FlagPrivilege)
	case n.GetTransactionStmt() != nil:
		s.Kind, s.Category, s.Access = "TRANSACTION", CatTx, adapter.Write
		s.flag(FlagTxControl)
	case n.GetVariableSetStmt() != nil:
		v := n.GetVariableSetStmt()
		s.Kind, s.Category, s.Access = "SET", CatUtility, adapter.Write
		s.flag(FlagSessionSetting)
		if v.Name == "session_replication_role" {
			s.flag(FlagDisablesTriggers)
		}
	case n.GetVariableShowStmt() != nil:
		s.Kind, s.Category, s.Access = "SHOW", CatRead, adapter.Read
	case n.GetCopyStmt() != nil:
		c := n.GetCopyStmt()
		s.Kind, s.Category, s.Access, s.Rel = "COPY", CatDML, adapter.Write, c.Relation
		if c.Relation != nil {
			s.obj(RelName(c.Relation))
		}
		if !c.IsFrom {
			s.Category, s.Access = CatRead, adapter.Read
		}
		if c.Filename != "" || c.IsProgram {
			s.flag(FlagServerFile)
		}
	case n.GetDoStmt() != nil:
		s.Kind, s.Category, s.Access = "DO", CatUtility, adapter.Destructive
		s.flag(FlagCodeBlock)
	case n.GetCallStmt() != nil:
		s.Kind, s.Category, s.Access = "CALL", CatUtility, adapter.Destructive
		s.flag(FlagProcedureCall)
	case n.GetVacuumStmt() != nil:
		s.Kind, s.Category, s.Access, s.Transactional = "VACUUM", CatUtility, adapter.Write, false
		for _, o := range n.GetVacuumStmt().Options {
			if d := o.GetDefElem(); d != nil && d.Defname == "full" {
				s.flag(FlagHeavyLock)
			}
		}
	case n.GetClusterStmt() != nil:
		s.Kind, s.Category, s.Access = "CLUSTER", CatUtility, adapter.Write
		s.flag(FlagHeavyLock)
	case n.GetReindexStmt() != nil:
		s.Kind, s.Category, s.Access = "REINDEX", CatUtility, adapter.Write
		s.flag(FlagHeavyLock)
	case n.GetLockStmt() != nil:
		s.Kind, s.Category, s.Access = "LOCK", CatUtility, adapter.Write
		s.flag(FlagHeavyLock)
	case n.GetCreatedbStmt() != nil:
		s.Kind, s.Category, s.Access, s.Transactional = "CREATE DATABASE", CatDDL, adapter.Write, false
	case n.GetAlterSystemStmt() != nil:
		s.Kind, s.Category, s.Access, s.Transactional = "ALTER SYSTEM", CatUtility, adapter.Destructive, false
		s.flag(FlagServerFile)
	default:
		s.Kind, s.Category, s.Access = strings.ToUpper(nodeTypeName(n)), CatDDL, adapter.Destructive
		s.flag(FlagUnparsed)
	}
	sort.Strings(s.Flags)
}

func nodeTypeName(n *pg.Node) string {
	m := n.ProtoReflect()
	var name string
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		name = string(fd.Message().Name())
		return false
	})
	return strings.TrimSuffix(name, "Stmt")
}

func classifyAlter(s *Stmt, a *pg.AlterTableStmt) {
	s.Kind, s.Category, s.Access, s.Rel = "ALTER TABLE", CatDDL, adapter.Write, a.Relation
	switch a.Objtype {
	case pg.ObjectType_OBJECT_INDEX:
		s.Kind = "ALTER INDEX"
	case pg.ObjectType_OBJECT_VIEW:
		s.Kind = "ALTER VIEW"
	case pg.ObjectType_OBJECT_SEQUENCE:
		s.Kind = "ALTER SEQUENCE"
	}
	s.obj(RelName(a.Relation))
	for _, c := range a.Cmds {
		cmd := c.GetAlterTableCmd()
		if cmd == nil {
			continue
		}
		switch cmd.Subtype {
		case pg.AlterTableType_AT_DropColumn:
			s.Access = adapter.Destructive
			s.flag(FlagDropColumn)
			if cmd.Behavior == pg.DropBehavior_DROP_CASCADE {
				s.flag(FlagCascade)
			}
		case pg.AlterTableType_AT_AlterColumnType:
			s.flag(FlagAlterColumnType)
			s.flag(FlagHeavyLock)
		case pg.AlterTableType_AT_SetNotNull:
			s.flag(FlagSetNotNull)
		case pg.AlterTableType_AT_AddColumn:
			cd := cmd.Def.GetColumnDef()
			if cd == nil {
				continue
			}
			hasDefault, notNull := cd.RawDefault != nil, cd.IsNotNull
			for _, cn := range cd.Constraints {
				con := cn.GetConstraint()
				if con == nil {
					continue
				}
				switch con.Contype {
				case pg.ConstrType_CONSTR_DEFAULT:
					hasDefault = true
					if hasVolatileCall(con.RawExpr) {
						s.flag(FlagVolatileDefault)
					}
				case pg.ConstrType_CONSTR_NOTNULL:
					notNull = true
				case pg.ConstrType_CONSTR_PRIMARY:
					s.flag(FlagAddPrimaryKey)
				case pg.ConstrType_CONSTR_UNIQUE:
					s.flag(FlagAddUnique)
				case pg.ConstrType_CONSTR_FOREIGN:
					s.flag(FlagFKWithoutNotValid)
				}
			}
			if notNull && !hasDefault {
				s.flag(FlagAddColumnNotNull)
			}
		case pg.AlterTableType_AT_AddConstraint:
			con := cmd.Def.GetConstraint()
			if con == nil {
				continue
			}
			switch con.Contype {
			case pg.ConstrType_CONSTR_FOREIGN:
				if !con.SkipValidation {
					s.flag(FlagFKWithoutNotValid)
				}
			case pg.ConstrType_CONSTR_CHECK:
				if !con.SkipValidation {
					s.flag(FlagCheckWithoutNotValid)
				}
			case pg.ConstrType_CONSTR_UNIQUE:
				if con.Indexname == "" {
					s.flag(FlagAddUnique)
				}
			case pg.ConstrType_CONSTR_PRIMARY:
				if con.Indexname == "" {
					s.flag(FlagAddPrimaryKey)
				}
			}
		case pg.AlterTableType_AT_DisableTrig, pg.AlterTableType_AT_DisableTrigAll, pg.AlterTableType_AT_DisableTrigUser:
			s.flag(FlagDisablesTriggers)
		case pg.AlterTableType_AT_SetTableSpace, pg.AlterTableType_AT_SetAccessMethod, pg.AlterTableType_AT_SetLogged, pg.AlterTableType_AT_SetUnLogged:
			s.flag(FlagHeavyLock)
		}
	}
}

// volatileFuncs are common volatile functions that force a table rewrite
// when used as a column default.
var volatileFuncs = map[string]bool{"random": true, "gen_random_uuid": true, "uuid_generate_v4": true, "clock_timestamp": true, "timeofday": true, "nextval": true}

func hasVolatileCall(n *pg.Node) bool {
	found := false
	walk(n.ProtoReflect(), func(m protoreflect.Message) {
		if fc, ok := m.Interface().(*pg.FuncCall); ok {
			if volatileFuncs[funcName(fc)] {
				found = true
			}
		}
	})
	return found
}

func funcName(fc *pg.FuncCall) string {
	if len(fc.Funcname) == 0 {
		return ""
	}
	return fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval()
}

// walk visits every message in a pg_query tree.
func walk(m protoreflect.Message, fn func(protoreflect.Message)) {
	if !m.IsValid() {
		return
	}
	fn(m)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				walk(l.Get(i).Message(), fn)
			}
		case fd.Message() != nil && !fd.IsMap():
			walk(v.Message(), fn)
		}
		return true
	})
}

func walkFuncs(s *Stmt, n *pg.Node) {
	walk(n.ProtoReflect(), func(m protoreflect.Message) {
		if fc, ok := m.Interface().(*pg.FuncCall); ok {
			if sideEffectFuncs[funcName(fc)] && funcName(fc) != "nextval" {
				s.flag(FlagSideEffectFunction)
			}
		}
	})
}

func walkReads(s *Stmt, n *pg.Node) {
	ctes := map[string]bool{}
	walk(n.ProtoReflect(), func(m protoreflect.Message) {
		if c, ok := m.Interface().(*pg.CommonTableExpr); ok {
			ctes[c.Ctename] = true
		}
	})
	walk(n.ProtoReflect(), func(m protoreflect.Message) {
		switch x := m.Interface().(type) {
		case *pg.RangeVar:
			if x.Schemaname == "" && ctes[x.Relname] {
				return
			}
			s.obj(RelName(x))
		case *pg.FuncCall:
			name := funcName(x)
			if sideEffectFuncs[name] {
				s.flag(FlagSideEffectFunction)
				if name != "pg_sleep" && name != "pg_advisory_lock" && name != "pg_advisory_xact_lock" {
					s.Access = adapter.Write
				}
			}
		case *pg.InsertStmt, *pg.UpdateStmt, *pg.DeleteStmt, *pg.MergeStmt:
			// Data-modifying CTE inside a SELECT.
			s.Access = adapter.Destructive
			s.Category = CatDML
			s.flag(FlagWithClause)
		}
	})
}

// Classify parses and classifies sql into the adapter's wire classification.
func Classify(sql string) (adapter.Classification, []*Stmt, error) {
	stmts, err := Parse(sql)
	if err != nil {
		return adapter.Classification{Access: adapter.Destructive, Flags: []string{FlagUnparsed}}, nil, err
	}
	c := adapter.Classification{Access: adapter.Read}
	objs, flags := map[string]bool{}, map[string]bool{}
	for _, s := range stmts {
		c.Access = adapter.MaxAccess(c.Access, s.Access)
		c.Statements = append(c.Statements, adapter.StatementClass{Text: s.Text, Kind: s.Kind, Access: s.Access, Objects: s.Objects, Flags: s.Flags})
		for _, o := range s.Objects {
			if !objs[o] {
				objs[o] = true
				c.Objects = append(c.Objects, o)
			}
		}
		for _, f := range s.Flags {
			flags[f] = true
		}
	}
	for f := range flags {
		c.Flags = append(c.Flags, f)
	}
	sort.Strings(c.Flags)
	return c, stmts, nil
}
