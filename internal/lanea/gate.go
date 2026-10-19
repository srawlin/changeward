package lanea

import (
	"strings"

	"github.com/changegate/changegate/internal/model"
	"github.com/changegate/changegate/internal/policy"
	"github.com/changegate/changegate/pkg/adapter"
)

// Decide applies policy to rehearsal evidence (lane "ci"). The policy sees
// the aggregate of all new migrations: worst reversibility, any destructive
// statement, rows touched on the copy, the tier, and every lint flag.
func Decide(pol *policy.Engine, e *Evidence) model.Decision {
	r := e.Rehearsal
	if r == nil || len(r.Results) == 0 {
		return model.Decision{Decision: policy.Approve, Rules: []string{"no-migrations"}, Reasons: []string{"no new migrations"}}
	}
	flags := map[string]bool{}
	var flagList []any
	maxLock := ""
	rewrite := false
	for _, m := range r.Results {
		for _, f := range m.Flags {
			if !flags[f] {
				flags[f] = true
				flagList = append(flagList, f)
			}
		}
		if lockRank(m.MaxLock) > lockRank(maxLock) {
			maxLock = m.MaxLock
		}
		if len(m.Rewrites) > 0 {
			rewrite = true
		}
	}
	if flagList == nil {
		flagList = []any{}
	}
	plan := &adapter.Plan{Adapter: "postgres", Env: e.Env, Reversibility: r.Reversibility, Destructive: r.Destructive, BlastRows: r.BlastRows,
		RehearsalTier: r.Tier, Operation: adapter.Operation{Kind: "migration"}}
	ch := policy.ChangeInput(plan, model.LaneCI, r.Passed, map[string]any{
		"flags":      flagList,
		"migrations": int64(len(r.Results)),
		"framework":  e.Framework,
		"facts":      map[string]any{"max_lock": maxLock, "rewrite": rewrite},
	})
	principal := map[string]any{"kind": "human", "agent": "", "human": e.Provenance.Human}
	if e.Provenance.Agent {
		principal["kind"] = "agent"
		principal["agent"] = e.Provenance.Name
	}
	return pol.Decide(policy.Input{Change: ch, Principal: principal, Session: map[string]any{"incident_ref": "", "has_checkpoint": false, "spend_usd": 0.0}})
}

func lockRank(m string) int {
	order := []string{"", "AccessShareLock", "RowShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock", "ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock"}
	for i, x := range order {
		if strings.EqualFold(x, m) {
			return i
		}
	}
	return 0
}
