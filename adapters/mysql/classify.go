package mysql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver" // value expressions for the parser
	"github.com/srawlin/changeward/pkg/adapter"
)

// Statement categories.
const (
	CatRead    = "read"
	CatDML     = "dml"
	CatDDL     = "ddl"
	CatUtility = "utility"
)

// Stmt is one parsed, classified MySQL statement.
type Stmt struct {
	Text     string
	Kind     string
	Category string
	Access   adapter.Access
	Objects  []string
	Flags    []string
	Node     ast.StmtNode
	// Table is the single target table of simple DML/DDL ("" otherwise).
	Table  string
	Schema string
	Alias  string
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

func tableName(t *ast.TableName) string {
	if t.Schema.O != "" {
		return t.Schema.O + "." + t.Name.O
	}
	return t.Name.O
}

// singleTable returns the one table of a FROM/table-refs clause, if simple.
func singleTable(refs *ast.TableRefsClause) (*ast.TableName, string, bool) {
	if refs == nil || refs.TableRefs == nil || refs.TableRefs.Right != nil {
		return nil, "", false
	}
	ts, ok := refs.TableRefs.Left.(*ast.TableSource)
	if !ok {
		return nil, "", false
	}
	tn, ok := ts.Source.(*ast.TableName)
	if !ok {
		return nil, "", false
	}
	return tn, ts.AsName.O, true
}

// Restore renders an AST node back to SQL.
func Restore(n ast.Node) (string, error) {
	var sb strings.Builder
	if err := n.Restore(format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)); err != nil {
		return "", err
	}
	return sb.String(), nil
}

var sideEffectFuncs = map[string]bool{"sleep": true, "get_lock": true, "release_lock": true, "load_file": true, "sys_exec": true, "sys_eval": true}

type funcVisitor struct{ hit bool }

func (v *funcVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if f, ok := n.(*ast.FuncCallExpr); ok && sideEffectFuncs[f.FnName.L] {
		v.hit = true
	}
	return n, false
}
func (v *funcVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// Parse splits and classifies a MySQL script.
func Parse(sql string) ([]*Stmt, error) {
	p := parser.New()
	nodes, _, err := p.Parse(sql, "", "")
	if err != nil {
		return nil, fmt.Errorf("parse SQL: %w", err)
	}
	var out []*Stmt
	for _, n := range nodes {
		s := &Stmt{Text: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n.OriginalText()), ";")), Node: n}
		if s.Text == "" {
			s.Text, _ = Restore(n)
		}
		classify(s)
		fv := &funcVisitor{}
		n.Accept(fv)
		if fv.hit {
			s.flag("side-effect-function")
		}
		sort.Strings(s.Flags)
		out = append(out, s)
	}
	return out, nil
}

func classify(s *Stmt) {
	set := func(kind, cat string, acc adapter.Access) { s.Kind, s.Category, s.Access = kind, cat, acc }
	switch n := s.Node.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		set("SELECT", CatRead, adapter.Read)
		if sel, ok := n.(*ast.SelectStmt); ok && sel.SelectIntoOpt != nil {
			set("SELECT INTO", CatUtility, adapter.Write)
			s.flag("server-file-access")
		}
	case *ast.ShowStmt:
		set("SHOW", CatRead, adapter.Read)
	case *ast.ExplainStmt:
		set("EXPLAIN", CatRead, adapter.Read)
		if n.Analyze {
			set("EXPLAIN ANALYZE", CatRead, adapter.Write)
		}
	case *ast.UpdateStmt:
		set("UPDATE", CatDML, adapter.Write)
		if tn, as, ok := singleTable(n.TableRefs); ok {
			s.Table, s.Schema, s.Alias = tn.Name.O, tn.Schema.O, as
			s.Objects = []string{tableName(tn)}
		} else {
			s.flag("multi-table-dml")
		}
		if n.Where == nil {
			s.flag("update-without-where")
		}
	case *ast.DeleteStmt:
		set("DELETE", CatDML, adapter.Destructive)
		if tn, as, ok := singleTable(n.TableRefs); ok && !n.IsMultiTable {
			s.Table, s.Schema, s.Alias = tn.Name.O, tn.Schema.O, as
			s.Objects = []string{tableName(tn)}
		} else {
			s.flag("multi-table-dml")
		}
		if n.Where == nil {
			s.flag("delete-without-where")
		}
	case *ast.InsertStmt:
		set("INSERT", CatDML, adapter.Write)
		if n.IsReplace {
			set("REPLACE", CatDML, adapter.Destructive)
		}
		if tn, _, ok := singleTable(n.Table); ok {
			s.Table, s.Schema = tn.Name.O, tn.Schema.O
			s.Objects = []string{tableName(tn)}
		}
		if len(n.OnDuplicate) > 0 {
			s.flag("upsert")
		}
		if n.Select != nil {
			s.flag("insert-select")
		}
	case *ast.CreateTableStmt:
		set("CREATE TABLE", CatDDL, adapter.Write)
		s.Table, s.Schema = n.Table.Name.O, n.Table.Schema.O
		s.Objects = []string{tableName(n.Table)}
	case *ast.DropTableStmt:
		set("DROP TABLE", CatDDL, adapter.Destructive)
		if n.IsView {
			s.Kind = "DROP VIEW"
		}
		for _, t := range n.Tables {
			s.Objects = append(s.Objects, tableName(t))
		}
		if len(n.Tables) == 1 {
			s.Table, s.Schema = n.Tables[0].Name.O, n.Tables[0].Schema.O
		}
		s.flag("drop-table")
	case *ast.TruncateTableStmt:
		set("TRUNCATE", CatDDL, adapter.Destructive)
		s.Table, s.Schema = n.Table.Name.O, n.Table.Schema.O
		s.Objects = []string{tableName(n.Table)}
		s.flag("truncate")
	case *ast.AlterTableStmt:
		set("ALTER TABLE", CatDDL, adapter.Write)
		s.Table, s.Schema = n.Table.Name.O, n.Table.Schema.O
		s.Objects = []string{tableName(n.Table)}
		for _, sp := range n.Specs {
			switch sp.Tp {
			case ast.AlterTableDropColumn:
				s.Access = adapter.Destructive
				s.flag("drop-column")
			case ast.AlterTableModifyColumn, ast.AlterTableChangeColumn:
				s.flag("alter-column-type")
			case ast.AlterTableDropPrimaryKey, ast.AlterTableDropIndex, ast.AlterTableDropForeignKey:
				s.flag("drop-index")
			}
		}
	case *ast.CreateIndexStmt:
		set("CREATE INDEX", CatDDL, adapter.Write)
		s.Table, s.Schema = n.Table.Name.O, n.Table.Schema.O
		s.Objects = []string{tableName(n.Table)}
	case *ast.DropIndexStmt:
		set("DROP INDEX", CatDDL, adapter.Write)
		s.Table, s.Schema = n.Table.Name.O, n.Table.Schema.O
		s.Objects = []string{tableName(n.Table)}
		s.flag("drop-index")
	case *ast.RenameTableStmt:
		set("RENAME TABLE", CatDDL, adapter.Write)
		for _, t := range n.TableToTables {
			s.Objects = append(s.Objects, tableName(t.OldTable))
		}
		s.flag("rename-table")
	case *ast.DropDatabaseStmt:
		set("DROP DATABASE", CatDDL, adapter.Destructive)
		s.flag("drop-database")
	case *ast.CreateDatabaseStmt:
		set("CREATE DATABASE", CatDDL, adapter.Write)
	case *ast.GrantStmt, *ast.RevokeStmt, *ast.GrantRoleStmt, *ast.CreateUserStmt, *ast.DropUserStmt, *ast.AlterUserStmt:
		set("PRIVILEGE", CatDDL, adapter.Destructive)
		s.flag("privilege-change")
	case *ast.BeginStmt, *ast.CommitStmt, *ast.RollbackStmt:
		set("TRANSACTION", CatUtility, adapter.Write)
		s.flag("transaction-control")
	case *ast.SetStmt:
		set("SET", CatUtility, adapter.Write)
		s.flag("session-setting")
	case *ast.CallStmt:
		set("CALL", CatUtility, adapter.Destructive)
		s.flag("procedure-call")
	case *ast.LoadDataStmt:
		set("LOAD DATA", CatDML, adapter.Write)
		s.flag("server-file-access")
	default:
		set(strings.ToUpper(strings.TrimPrefix(fmt.Sprintf("%T", n), "*ast.")), CatDDL, adapter.Destructive)
		s.flag("unparsed")
	}
}

// Classify parses sql into the adapter's wire classification.
func Classify(sql string) (adapter.Classification, []*Stmt, error) {
	stmts, err := Parse(sql)
	if err != nil {
		return adapter.Classification{Access: adapter.Destructive, Flags: []string{"unparsed"}}, nil, err
	}
	c := adapter.Classification{Access: adapter.Read}
	seen, flags := map[string]bool{}, map[string]bool{}
	for _, s := range stmts {
		c.Access = adapter.MaxAccess(c.Access, s.Access)
		c.Statements = append(c.Statements, adapter.StatementClass{Text: s.Text, Kind: s.Kind, Access: s.Access, Objects: s.Objects, Flags: s.Flags})
		for _, o := range s.Objects {
			if !seen[o] {
				seen[o] = true
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
