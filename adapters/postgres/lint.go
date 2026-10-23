package postgres

import (
	"fmt"
	"sort"
	"strings"
)

// Finding is a human-readable lint finding for one statement.
type Finding struct {
	Flag       string `json:"flag"`
	Severity   string `json:"severity"` // "error", "warning", "info"
	Statement  int    `json:"statement"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

type rule struct {
	severity, message, suggestion string
}

var lintRules = map[string]rule{
	FlagDeleteWithoutWhere: {"error", "DELETE without WHERE removes every row", "Add a WHERE clause, or use TRUNCATE deliberately in a reviewed migration."},
	FlagUpdateWithoutWhere: {"error", "UPDATE without WHERE rewrites every row", "Add a WHERE clause; for backfills, batch by primary key range."},
	FlagDropTable:          {"error", "DROP TABLE destroys the table and its data", "Expand/contract: stop reading and writing the table in one release, drop it in a later one. In the direct lane Changeward soft-drops into _changeward_trash."},
	FlagDropColumn:         {"error", "DROP COLUMN destroys the column's data and breaks code still reading it", "Expand/contract: stop using the column, deploy, then drop it in a later release."},
	FlagDropSchema:         {"error", "DROP SCHEMA destroys every object in the schema", ""},
	FlagDropDatabase:       {"error", "DROP DATABASE destroys the database", ""},
	FlagDropIndex:          {"warning", "DROP INDEX may slow queries that relied on it, and takes an ACCESS EXCLUSIVE lock", "Use DROP INDEX CONCURRENTLY on large tables."},
	FlagDropOther:          {"warning", "DROP removes a database object", ""},
	FlagTruncate:           {"error", "TRUNCATE removes every row and takes an ACCESS EXCLUSIVE lock", ""},
	FlagCascade:            {"error", "CASCADE also drops or truncates dependent objects that are not listed", "Drop dependents explicitly so the effect is reviewable."},
	FlagNonConcurrentIndex: {"warning", "CREATE INDEX without CONCURRENTLY blocks writes to the table for the whole build", "Use CREATE INDEX CONCURRENTLY (outside a transaction) on existing tables."},
	FlagConcurrent:         {"info", "CONCURRENTLY cannot run inside a transaction, so it cannot be dry-run on the live target", "Rehearse on a copy."},
	FlagAlterColumnType:    {"error", "ALTER COLUMN TYPE usually rewrites the table under an ACCESS EXCLUSIVE lock", "Expand/contract: add a new column, backfill, switch reads, then drop the old column."},
	FlagSetNotNull:         {"warning", "SET NOT NULL scans the whole table under an ACCESS EXCLUSIVE lock", "Add a CHECK (col IS NOT NULL) NOT VALID constraint, VALIDATE it, then SET NOT NULL (PG12+ uses the constraint)."},
	FlagVolatileDefault:    {"error", "ADD COLUMN with a volatile default rewrites the whole table", "Add the column without a default, set the default, then backfill in batches."},
	FlagAddColumnNotNull:   {"error", "ADD COLUMN NOT NULL without a default fails on any non-empty table", "Add a DEFAULT, or add the column nullable, backfill, then SET NOT NULL."},
	FlagFKWithoutNotValid:  {"warning", "Adding a FOREIGN KEY validates every row while holding locks on both tables", "Add it NOT VALID, then VALIDATE CONSTRAINT separately."},
	FlagCheckWithoutNotValid: {"warning", "Adding a CHECK constraint scans the table under an ACCESS EXCLUSIVE lock",
		"Add it NOT VALID, then VALIDATE CONSTRAINT separately."},
	FlagAddUnique:          {"warning", "Adding a UNIQUE constraint builds an index while blocking writes", "CREATE UNIQUE INDEX CONCURRENTLY, then ADD CONSTRAINT ... USING INDEX."},
	FlagAddPrimaryKey:      {"warning", "Adding a PRIMARY KEY builds an index while blocking writes", "CREATE UNIQUE INDEX CONCURRENTLY, then ADD PRIMARY KEY USING INDEX."},
	FlagRenameTable:        {"warning", "Renaming a table breaks code that still uses the old name", "Expand/contract: create a view with the old name during the transition."},
	FlagRenameColumn:       {"warning", "Renaming a column breaks code that still uses the old name", "Expand/contract: add the new column, dual-write, migrate reads, drop the old one."},
	FlagPrivilege:          {"warning", "Changes database privileges", ""},
	FlagMultiTable:         {"info", "Multi-table DML (FROM/USING) is harder to preview exactly on older Postgres", ""},
	FlagWithClause:         {"info", "Data-modifying statement inside a WITH clause", ""},
	FlagUpsert:             {"info", "INSERT ... ON CONFLICT may update existing rows", ""},
	FlagTxControl:          {"error", "Transaction control statements are managed by Changeward and not allowed in a change", "Submit the statements without BEGIN/COMMIT; each change runs in its own transaction."},
	FlagSessionSetting:     {"info", "SET changes session settings", ""},
	FlagCodeBlock:          {"error", "DO blocks run arbitrary code that cannot be previewed or undone", "Write the change as plain SQL statements."},
	FlagProcedureCall:      {"error", "CALL runs a procedure whose effects cannot be previewed or undone", ""},
	FlagSideEffectFunction: {"error", "Calls a function with effects outside the transaction (cannot be rolled back)", ""},
	FlagHeavyLock:          {"warning", "Takes a heavy lock or rewrites the table", "Schedule in a maintenance window."},
	FlagServerFile:         {"error", "Reads or writes server files or runs programs", ""},
	FlagDisablesTriggers:   {"error", "Disables triggers (and with session_replication_role, foreign keys)", ""},
	FlagUnparsed:           {"warning", "Statement type is not analyzed by Changeward", ""},
}

// Lint returns human-readable findings for parsed statements.
func Lint(stmts []*Stmt) []Finding {
	var out []Finding
	for i, s := range stmts {
		for _, f := range s.Flags {
			r, ok := lintRules[f]
			if !ok {
				continue
			}
			out = append(out, Finding{Flag: f, Severity: r.severity, Statement: i + 1, Message: r.message, Suggestion: r.suggestion})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return sevRank(out[i].Severity) > sevRank(out[j].Severity) })
	return out
}

func sevRank(s string) int {
	switch s {
	case "error":
		return 2
	case "warning":
		return 1
	}
	return 0
}

// FormatFindings renders findings for terminals.
func FormatFindings(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "  [%s] #%d %s (%s)\n", f.Severity, f.Statement, f.Message, f.Flag)
		if f.Suggestion != "" {
			fmt.Fprintf(&b, "         → %s\n", f.Suggestion)
		}
	}
	return b.String()
}
