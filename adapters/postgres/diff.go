package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/srawlin/changegate/pkg/adapter"
)

// redactor hides values in plans shown to agents. Columns matching a
// sensitive pattern ("table.column", "*.column" or "column") are masked. In
// shared and production environments no row values are shown at all: only
// counts and hashed keys (§9).
type redactor struct {
	patterns  []string
	keysOnly  bool
	hashedKey bool
}

func newRedactor(t adapter.Target) *redactor {
	r := &redactor{}
	for k, v := range t.Config {
		if strings.HasPrefix(k, "sensitive.") {
			r.patterns = append(r.patterns, strings.ToLower(v))
		}
	}
	switch t.Env {
	case adapter.EnvPersonal:
	case adapter.EnvSharedDev:
		r.keysOnly = true
	default:
		r.keysOnly, r.hashedKey = true, true
	}
	if t.Config["show_values"] == "true" && t.Env == adapter.EnvSharedDev {
		r.keysOnly = false
	}
	return r
}

func (r *redactor) sensitive(table, col string) bool {
	col = strings.ToLower(col)
	short := table
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		short = table[i+1:]
	}
	for _, p := range r.patterns {
		if p == col || p == "*."+col || p == strings.ToLower(short)+"."+col || p == strings.ToLower(table)+"."+col {
			return true
		}
	}
	return false
}

func (r *redactor) value(table, col string, v json.RawMessage) string {
	if r.sensitive(table, col) {
		return "•••"
	}
	s := string(v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func (r *redactor) key(table string, pk []string, img json.RawMessage) string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(img, &m)
	if len(pk) == 0 {
		sum := sha256.Sum256(img)
		return "row#" + hex.EncodeToString(sum[:4])
	}
	var parts []string
	for _, k := range pk {
		v := string(m[k])
		if r.hashedKey || r.sensitive(table, k) {
			sum := sha256.Sum256(m[k])
			v = "h:" + hex.EncodeToString(sum[:4])
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (b *planBuilder) renderDML(i int, s *Stmt, rel *relRef, pk []string, c *capture) {
	r := b.redact
	table := rel.display()
	fmt.Fprintf(&b.diff, "#%d %s %s — %d rows\n", i+1, s.Kind, table, c.Count)
	shown := 0
	limit := b.a.opts.DisplayRows
	for _, u := range c.Updated {
		if shown >= limit {
			break
		}
		var bm, am map[string]json.RawMessage
		_ = json.Unmarshal(u.B, &bm)
		_ = json.Unmarshal(u.A, &am)
		var changes []string
		var keys []string
		for k := range am {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if string(bm[k]) != string(am[k]) {
				if r.keysOnly {
					changes = append(changes, k)
				} else {
					changes = append(changes, fmt.Sprintf("%s: %s → %s", k, r.value(table, k, bm[k]), r.value(table, k, am[k])))
				}
			}
		}
		if len(changes) == 0 {
			changes = []string{"(no column values changed)"}
		}
		fmt.Fprintf(&b.diff, "   ~ %s  %s\n", r.key(table, pk, u.B), strings.Join(changes, "; "))
		shown++
	}
	for _, img := range c.Deleted {
		if shown >= limit {
			break
		}
		fmt.Fprintf(&b.diff, "   - %s%s\n", r.key(table, pk, img), r.row(table, img))
		shown++
	}
	for _, img := range c.Inserted {
		if shown >= limit {
			break
		}
		fmt.Fprintf(&b.diff, "   + %s%s\n", r.key(table, pk, img), r.row(table, img))
		shown++
	}
	if int64(shown) < c.Count && (len(c.Updated)+len(c.Deleted)+len(c.Inserted)) > 0 {
		fmt.Fprintf(&b.diff, "   … %d more\n", c.Count-int64(shown))
	}
}

func (r *redactor) row(table string, img json.RawMessage) string {
	if r.keysOnly {
		return ""
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(img, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+r.value(table, k, m[k]))
	}
	s := strings.Join(parts, " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return "  {" + s + "}"
}
