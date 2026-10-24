package lanea

import (
	"fmt"
	"sort"
	"strings"

	"github.com/srawlin/changeward/adapters/postgres"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/pkg/adapter"
)

// Marker identifies Changeward's PR/MR comment so it can be updated in place.
const Marker = "<!-- changeward-report -->"

// Evidence is everything the report and gate need. It is the JSON written
// by `changeward rehearse --out`.
type Evidence struct {
	Rehearsal  *postgres.RehearsalReport `json:"rehearsal"`
	Provenance Provenance                `json:"provenance"`
	Env        string                    `json:"env"`
	Framework  string                    `json:"framework"`
	Dir        string                    `json:"dir"`
	BaseRef    string                    `json:"base_ref,omitempty"`
	Decision   *model.Decision           `json:"decision,omitempty"`
	Skipped    []string                  `json:"skipped,omitempty"`
}

var tierNames = map[string]string{
	postgres.TierT0: "T0 · schema only",
	postgres.TierT1: "T1 · synthetic data",
	postgres.TierT2: "T2 · masked copy",
	postgres.TierT3: "T3 · production clone",
}

func revIcon(r adapter.Reversibility, passed bool) string {
	if !passed {
		return "❌"
	}
	switch r {
	case adapter.Reversible:
		return "✅"
	case adapter.Compensatable, adapter.CheckpointOnly:
		return "⚠️"
	}
	return "❌"
}

func esc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// Markdown renders the PR/MR comment. It contains counts, names, lock modes
// and timings, never row values (§5.3.3).
func Markdown(e *Evidence) string {
	var b strings.Builder
	b.WriteString(Marker + "\n")
	r := e.Rehearsal
	if r == nil || len(r.Results) == 0 {
		b.WriteString("## Changeward: no new migrations in this change\n")
		for _, s := range e.Skipped {
			b.WriteString("- " + s + "\n")
		}
		return b.String()
	}
	headline := "reversible"
	switch {
	case !r.Passed:
		headline = "rehearsal failed"
	case r.Reversibility != adapter.Reversible:
		headline = "not proven reversible (" + string(r.Reversibility) + ")"
	}
	fmt.Fprintf(&b, "## %s Changeward rehearsal: %d migration(s) · %s\n\n", revIcon(r.Reversibility, r.Passed), len(r.Results), headline)
	tier := tierNames[r.Tier]
	if tier == "" {
		tier = r.Tier
	}
	fmt.Fprintf(&b, "Rehearsed on **%s** · Postgres %d · %s %s · %d base migration(s) replayed · %.1fs\n\n",
		tier, r.ServerVersion/10000, e.Framework, "`"+e.Dir+"`", r.BaseCount, float64(r.DurationMs)/1000)
	if e.Provenance.Agent {
		fmt.Fprintf(&b, "**Provenance:** agent-authored (%s): %s\n\n", e.Provenance.Name, strings.Join(e.Provenance.Signals, "; "))
	} else if e.Provenance.Human != "" {
		fmt.Fprintf(&b, "**Provenance:** human-authored (%s); no agent signals found\n\n", e.Provenance.Human)
	}
	if d := e.Decision; d != nil {
		switch d.Decision {
		case "approve":
			fmt.Fprintf(&b, "**Policy (%s):** ✅ auto-approved: eligible as a standard change (%s)\n\n", e.Env, strings.Join(d.Rules, ", "))
		case "deny":
			fmt.Fprintf(&b, "**Policy (%s):** ⛔ blocked: %s\n\n", e.Env, strings.Join(d.Reasons, "; "))
		default:
			fmt.Fprintf(&b, "**Policy (%s):** 👀 review required by %s", e.Env, strings.Join(d.Approvers, ", "))
			if d.Quorum > 1 {
				fmt.Fprintf(&b, " (quorum %d)", d.Quorum)
			}
			if len(d.Reasons) > 0 {
				fmt.Fprintf(&b, ": %s", strings.Join(d.Reasons, "; "))
			}
			b.WriteString("\n\n")
		}
	}
	b.WriteString("| | Migration | Reversibility | Locks (existing tables) | Rows touched | Up / down | Findings |\n|---|---|---|---|---|---|---|\n")
	for _, m := range r.Results {
		rows := int64(0)
		for _, n := range m.RowsTouched {
			rows += n
		}
		locks := "—"
		if len(m.Locks) > 0 {
			var parts []string
			for rel, mode := range m.Locks {
				n := m.TableRows[rel]
				parts = append(parts, fmt.Sprintf("%s on `%s` (%d rows)", strings.TrimSuffix(mode, "Lock"), rel, n))
			}
			sort.Strings(parts)
			locks = strings.Join(parts, "<br>")
		}
		errs, warns := 0, 0
		for _, f := range m.Findings {
			switch f.Severity {
			case "error":
				errs++
			case "warning":
				warns++
			}
		}
		find := "—"
		if errs+warns > 0 {
			find = fmt.Sprintf("%d error(s), %d warning(s)", errs, warns)
		}
		timing := fmt.Sprintf("%dms", m.UpMs)
		if m.HasDown {
			timing += fmt.Sprintf(" / %dms", m.DownMs)
		} else {
			timing += " / no down"
		}
		rev := string(m.Reversibility)
		if m.Reversibility != adapter.Reversible {
			rev = "**" + rev + "**"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %d | %s | %s |\n", revIcon(m.Reversibility, m.Passed), esc(m.Path), rev, locks, rows, timing, find)
	}
	b.WriteString("\n")
	for _, m := range r.Results {
		fmt.Fprintf(&b, "<details><summary><code>%s</code>: %s</summary>\n\n", m.Path, summaryLine(m))
		if m.Error != "" {
			fmt.Fprintf(&b, "**Error:** `%s`\n\n", strings.ReplaceAll(m.Error, "`", "'"))
		}
		if len(m.Effects) > 0 {
			b.WriteString("**Effects** (catalog diff):\n```diff\n" + strings.Join(m.Effects, "\n") + "\n```\n")
		}
		if len(m.Rewrites) > 0 {
			fmt.Fprintf(&b, "**Table rewrites:** %s\n\n", strings.Join(m.Rewrites, ", "))
		}
		if len(m.RollbackDiff) > 0 {
			b.WriteString("**Rollback does not restore the previous state:**\n```diff\n" + strings.Join(m.RollbackDiff, "\n") + "\n```\n")
		} else if m.HasDown && m.Reversibility == adapter.Reversible {
			b.WriteString("**Rollback:** forward → down → compare restored the catalog and data checksums exactly")
			if m.Reapplied != nil && *m.Reapplied {
				b.WriteString("; re-applying forward succeeded")
			}
			b.WriteString(".\n\n")
		}
		for _, f := range m.Findings {
			fmt.Fprintf(&b, "- **%s** (#%d) %s", f.Severity, f.Statement, f.Message)
			if f.Suggestion != "" {
				fmt.Fprintf(&b, ". _%s_", f.Suggestion)
			}
			b.WriteString("\n")
		}
		for _, n := range m.Notes {
			b.WriteString("- " + n + "\n")
		}
		b.WriteString("\n</details>\n\n")
	}
	if len(r.Notes) > 0 || len(e.Skipped) > 0 {
		b.WriteString("**Notes:** ")
		b.WriteString(strings.Join(append(append([]string{}, r.Notes...), e.Skipped...), " · "))
		b.WriteString("\n\n")
	}
	b.WriteString("<sub>Changeward rehearses each migration on a throwaway copy: forward, observe locks/rewrites/rows, run the down migration, and compare catalog and data checksums with the state before. Row values never leave the copy.</sub>\n")
	return b.String()
}

func summaryLine(m postgres.MigrationResult) string {
	switch {
	case m.Error != "":
		return "failed"
	case m.Reversibility == adapter.Reversible:
		return "reversible"
	case !m.HasDown:
		return string(m.Reversibility) + " (no down migration)"
	}
	return string(m.Reversibility)
}
