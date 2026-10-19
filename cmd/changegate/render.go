package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/srawlin/changegate/internal/model"
)

func decisionLine(c *model.Change) string {
	if c.Decision == nil {
		return string(c.State)
	}
	d := c.Decision
	switch d.Decision {
	case "approve":
		return "auto-approved (" + strings.Join(d.Rules, ", ") + ")"
	case "deny":
		return "denied: " + strings.Join(d.Reasons, "; ")
	}
	s := fmt.Sprintf("approval required (%s", d.Channel)
	if len(d.Approvers) > 0 {
		s += "; approvers: " + strings.Join(d.Approvers, ", ")
	}
	if d.Quorum > 1 {
		s += fmt.Sprintf("; quorum %d", d.Quorum)
	}
	s += ")"
	if len(d.Reasons) > 0 {
		s += ": " + strings.Join(d.Reasons, "; ")
	}
	return s
}

func renderChange(w io.Writer, c *model.Change) { renderChangeOpts(w, c, true) }

func renderChangeOpts(w io.Writer, c *model.Change, showNext bool) {
	if c.Plan != nil {
		fmt.Fprint(w, c.Plan.Diff)
	}
	fmt.Fprintf(w, "change %s (#%d) — %s\n", c.ID, c.Seq, c.State)
	fmt.Fprintf(w, "policy: %s\n", decisionLine(c))
	if c.Error != "" {
		fmt.Fprintf(w, "error: %s\n", c.Error)
	}
	if !showNext {
		return
	}
	switch c.State {
	case model.AutoApproved, model.Approved:
		fmt.Fprintf(w, "next: changegate apply %s\n", c.Plan.ID)
	case model.AwaitingApproval:
		fmt.Fprintf(w, "next: changegate approve %s   (or reject)\n", c.Plan.ID)
	case model.Verified, model.Applied:
		fmt.Fprintf(w, "undo: changegate undo %s\n", c.ID)
	}
}

func shortSQL(c *model.Change) string {
	s := strings.Join(strings.Fields(c.Operation.Payload), " ")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func rowsLabel(c *model.Change) string {
	if c.Plan == nil {
		return ""
	}
	if c.Plan.BlastRows > 0 {
		return fmt.Sprintf("(%d rows)", c.Plan.BlastRows)
	}
	return ""
}

func renderTimeline(w io.Writer, s *model.Session, changes []*model.Change) {
	fmt.Fprintf(w, "SESSION %s  agent=%s  user=%s  env=%s  target=%s", s.ID, orDash(s.Principal.Agent), strings.TrimPrefix(s.Principal.Human, "user:"), s.Env, s.Target)
	if s.CheckpointID != "" {
		fmt.Fprintf(w, "  checkpoint=%s", s.CheckpointID)
	}
	if s.Branch != nil {
		fmt.Fprint(w, "  branch=private-copy")
	}
	if s.IncidentRef != "" {
		fmt.Fprintf(w, "  incident=%s", s.IncidentRef)
	}
	if s.EndedAt != "" {
		fmt.Fprint(w, "  (ended)")
	}
	fmt.Fprintln(w)
	if len(changes) == 0 {
		fmt.Fprintln(w, "  (no changes)")
	}
	for i, c := range changes {
		at := c.CreatedAt
		if len(at) >= 16 {
			at = at[11:16]
		}
		rev := ""
		if c.Plan != nil {
			rev = string(c.Plan.Reversibility)
		}
		fmt.Fprintf(w, "  #%-2d %s  %-62s %-16s %s\n", i+1, at, strings.TrimSpace(shortSQL(c)+" "+rowsLabel(c)), rev, c.State)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return strings.TrimPrefix(s, "agent:")
}
