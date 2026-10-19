package postgres

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MigrationSteps converts an applied change into migration SQL: up is the
// statements as the agent wrote them (a real DROP, not the soft-drop that
// kept the live change reversible); down comes from the recorded inverses,
// with soft-drops of created tables turned into plain DROPs. Steps that
// cannot be reversed in a migration (dropped data) become comments.
func MigrationSteps(payload string, undo json.RawMessage, includeData bool) (up, down []string, notes []string, err error) {
	_, stmts, err := Classify(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	var uh undoHandle
	if len(undo) > 0 {
		if err := json.Unmarshal(undo, &uh); err != nil {
			return nil, nil, nil, err
		}
	}
	steps := map[int]undoStep{}
	for _, s := range uh.Steps {
		steps[s.Index] = s
	}
	for i := len(stmts) - 1; i >= 0; i-- {
		s := stmts[i]
		st, ok := steps[i]
		switch {
		case s.Category == CatDML:
			if includeData {
				down = append(down, fmt.Sprintf("-- data change not reversed automatically: %s", firstLine(s.Text)))
			}
		case s.Category == CatDDL && ok && st.DDL != nil:
			soft := classifyDDL(s).Soft
			if soft != "" {
				down = append(down, fmt.Sprintf("-- IRREVERSIBLE in a migration: %s destroys data (Changegate soft-dropped it in the live session)", firstLine(s.Text)))
				notes = append(notes, fmt.Sprintf("%q cannot be reversed by a down migration; consider expand/contract", firstLine(s.Text)))
				continue
			}
			down = append(down, migrationInverse(st.DDL.Inverse)...)
		case s.Category == CatDDL:
			down = append(down, fmt.Sprintf("-- no recorded inverse for: %s", firstLine(s.Text)))
		}
	}
	for _, s := range stmts {
		if s.Category == CatDML && !includeData {
			notes = append(notes, fmt.Sprintf("data change left out of the migration: %s (use --include-data)", firstLine(s.Text)))
			continue
		}
		if s.Access == "read" {
			continue
		}
		up = append(up, s.Text)
	}
	return up, down, notes, nil
}

var (
	reSoftRename = regexp.MustCompile(`^ALTER (TABLE|VIEW|MATERIALIZED VIEW|SEQUENCE|FOREIGN TABLE) (\S+) RENAME TO "` + TrashPrefix)
	reSetTrash   = regexp.MustCompile(`SET SCHEMA "` + TrashSchema + `"$`)
)

// migrationInverse rewrites live-session inverses for a migration file.
func migrationInverse(inv []string) []string {
	var out []string
	for _, sql := range inv {
		t := strings.TrimSpace(sql)
		switch {
		case strings.HasPrefix(t, "CREATE SCHEMA IF NOT EXISTS \""+TrashSchema+"\""), strings.HasPrefix(t, "DO $cg$"), reSetTrash.MatchString(t):
			continue
		case reSoftRename.MatchString(t):
			m := reSoftRename.FindStringSubmatch(t)
			out = append(out, fmt.Sprintf("DROP %s %s", m[1], m[2]))
		default:
			out = append(out, t)
		}
	}
	return out
}
