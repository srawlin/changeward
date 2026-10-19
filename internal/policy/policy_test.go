package policy

import (
	"strings"
	"testing"
	"time"
)

func change(env, lane, rev string, destructive bool, rows int64) map[string]any {
	return map[string]any{
		"env": env, "lane": lane, "reversibility": rev, "destructive": destructive,
		"blast": map[string]any{"rows": rows}, "rehearsal": map[string]any{"passed": true, "tier": "live-dry-run"},
	}
}

func sess(incident string, cp bool) map[string]any {
	return map[string]any{"incident_ref": incident, "has_checkpoint": cp, "spend_usd": 0.0}
}

func TestBuiltinDefaults(t *testing.T) {
	e, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		change  map[string]any
		session map[string]any
		want    string
		channel string
	}{
		{"personal reversible", change("personal", "direct", "reversible", true, 5000), sess("", false), Approve, ""},
		{"personal irreversible", change("personal", "direct", "irreversible", true, 1), sess("", false), Require, ChannelInline},
		{"personal checkpoint-only with checkpoint", change("personal", "direct", "checkpoint-only", true, 1), sess("", true), Approve, ""},
		{"personal checkpoint-only without", change("personal", "direct", "checkpoint-only", true, 1), sess("", false), Require, ChannelInline},
		{"shared-dev small", change("shared-dev", "direct", "reversible", false, 10), sess("", false), Approve, ""},
		{"shared-dev large", change("shared-dev", "direct", "reversible", false, 5000), sess("", false), Require, ChannelInline},
		{"staging destructive", change("staging", "direct", "reversible", true, 1), sess("", false), Require, ChannelOutOfBand},
		{"prod no incident", change("prod", "direct", "reversible", false, 1), sess("", false), Deny, ""},
		{"prod with incident", change("prod", "direct", "reversible", false, 1), sess("INC-1", false), Require, ChannelOutOfBand},
		{"prod-restricted", change("prod-restricted", "direct", "reversible", false, 1), sess("INC-1", false), Deny, ""},
		{"ci reversible", change("prod", "ci", "reversible", false, 1), sess("", false), Approve, ""},
		{"ci irreversible", change("prod", "ci", "irreversible", true, 1), sess("", false), Require, ChannelOutOfBand},
	}
	for _, c := range cases {
		d := e.Decide(Input{Change: c.change, Session: c.session, Principal: map[string]any{"kind": "agent"}})
		if d.Decision != c.want {
			t.Errorf("%s: decision %s (rules %v), want %s", c.name, d.Decision, d.Rules, c.want)
		}
		if c.channel != "" && d.Channel != c.channel {
			t.Errorf("%s: channel %s, want %s", c.name, d.Channel, c.channel)
		}
	}
}

const userPolicy = `
version: 1
freezes:
  - name: q4-release
    start: 2026-12-01T00:00:00Z
    end: 2026-12-15T00:00:00Z
rules:
  - name: auto-approve-reversible-low-risk
    when: >
      change.reversibility == "reversible" && change.rehearsal.passed &&
      change.blast.rows < 1000 && change.env != "prod-pci"
    decide: approve
  - name: prod-destructive-needs-dba
    when: change.env.startsWith("prod") && change.destructive
    decide: require
    approvers: ["group:dba", "group:sre"]
    quorum: 2
  - name: freeze
    when: calendar.freeze("q4-release")
    decide: deny
  - name: agent-stricter-than-human
    when: principal.kind == "agent" && change.reversibility in ["checkpoint-only","irreversible"]
    decide: require
    approvers: ["owner:codeowners"]
  - name: continuous-agent-budget
    when: change.lane == "direct" && principal.agent == "refund-agent" && session.spend_usd + change.spend_usd > 500.0
    decide: require
    approvers: ["group:finance-ops"]
  - name: broken
    when: change.nonexistent.field == 1
    decide: approve
`

func TestUserPolicy(t *testing.T) {
	f, err := Parse([]byte(userPolicy))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	e.SetClock(func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) })

	d := e.Decide(Input{Change: change("prod", "ci", "reversible", true, 10), Principal: map[string]any{"kind": "agent"}})
	if d.Decision != Require || d.Quorum != 2 || strings.Join(d.Approvers, ",") != "group:dba,group:sre" {
		t.Fatalf("prod destructive: %+v", d)
	}
	// A rule that errors at evaluation never silently approves.
	if !contains(d.Rules, "broken") {
		t.Fatalf("broken rule should be reported: %+v", d)
	}

	e.SetClock(func() time.Time { return time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC) })
	d = e.Decide(Input{Change: change("staging", "ci", "reversible", false, 10), Principal: map[string]any{"kind": "human"}})
	if d.Decision != Deny {
		t.Fatalf("freeze: %+v", d)
	}

	e.SetClock(time.Now)
	c := change("personal", "direct", "reversible", false, 1)
	c["spend_usd"] = 100.0
	d = e.Decide(Input{Change: c, Principal: map[string]any{"kind": "agent", "agent": "refund-agent"}, Session: map[string]any{"spend_usd": 450.0}})
	if d.Decision != Require || !contains(d.Approvers, "group:finance-ops") {
		t.Fatalf("budget: %+v", d)
	}
}

func TestInvalidPolicy(t *testing.T) {
	for _, src := range []string{
		"rules: [{name: a, when: 'change.env ==', decide: approve}]",
		"rules: [{name: a, when: 'true', decide: maybe}]",
		"rules: [{name: a, when: '1 + 1', decide: approve}]",
		"rules: [{name: a, when: 'true', decide: approve}, {name: a, when: 'true', decide: deny}]",
	} {
		f, err := Parse([]byte(src))
		if err != nil {
			continue
		}
		if _, err := New(f); err == nil {
			t.Errorf("expected error for %s", src)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
