// Package lanea implements the CI/CD lane: agent provenance detection,
// rehearsal reports for pull/merge requests, the policy gate, and posting
// results to GitHub and GitLab.
package lanea

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Provenance describes who (or what) authored a change set.
type Provenance struct {
	Agent   bool     `json:"agent"`
	Name    string   `json:"name,omitempty"`
	Human   string   `json:"human,omitempty"`
	Signals []string `json:"signals,omitempty"`
	Commits int      `json:"commits"`
}

// knownAgents maps lower-case markers to agent names.
var knownAgents = []struct{ marker, name string }{
	{"claude", "Claude Code"}, {"anthropic", "Claude Code"},
	{"copilot", "GitHub Copilot"}, {"codex", "OpenAI Codex"}, {"openai", "OpenAI Codex"},
	{"cursor", "Cursor"}, {"devin", "Devin"}, {"gemini", "Gemini CLI"}, {"jules", "Jules"},
	{"sweep", "Sweep"}, {"aider", "Aider"}, {"coder agent", "Coder Agents"}, {"amazon q", "Amazon Q"},
	{"changegate", "Changegate promotion"},
}

var reTrailer = regexp.MustCompile(`(?im)^(co-authored-by|changegate-agent|assisted-by|generated-by):\s*(.+)$`)

func matchAgent(s string) string {
	ls := strings.ToLower(s)
	for _, k := range knownAgents {
		if strings.Contains(ls, k.marker) {
			return k.name
		}
	}
	return ""
}

// DetectProvenance inspects commits in base..head (git), the branch name and
// CI environment for signs of agent authorship.
func DetectProvenance(dir, base, head string) Provenance {
	var p Provenance
	add := func(name, signal string) {
		p.Agent = true
		if p.Name == "" {
			p.Name = name
		}
		for _, s := range p.Signals {
			if s == signal {
				return
			}
		}
		p.Signals = append(p.Signals, signal)
	}
	if head == "" {
		head = "HEAD"
	}
	rng := head
	if base != "" {
		rng = base + ".." + head
	}
	out, err := exec.Command("git", "-C", dir, "log", "--format=%H%x1f%an%x1f%ae%x1f%B%x1e", rng).Output()
	if err == nil {
		for _, rec := range bytes.Split(out, []byte{0x1e}) {
			f := strings.SplitN(strings.TrimSpace(string(rec)), "\x1f", 4)
			if len(f) < 4 {
				continue
			}
			p.Commits++
			sha, name, email, body := f[0], f[1], f[2], f[3]
			short := sha
			if len(short) > 8 {
				short = short[:8]
			}
			if strings.HasSuffix(name, "[bot]") || strings.Contains(email, "[bot]") {
				if a := matchAgent(name + " " + email); a != "" {
					add(a, "commit "+short+" authored by bot "+name)
				} else {
					add("bot", "commit "+short+" authored by bot "+name)
				}
			} else if a := matchAgent(email); a != "" {
				add(a, "commit "+short+" author email "+email)
			} else if p.Human == "" {
				p.Human = name
			}
			for _, m := range reTrailer.FindAllStringSubmatch(body, -1) {
				if a := matchAgent(m[2]); a != "" {
					add(a, "commit "+short+" trailer "+m[1]+": "+strings.TrimSpace(m[2]))
				}
			}
		}
	}
	for _, env := range []string{"GITHUB_HEAD_REF", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME", "CI_COMMIT_REF_NAME"} {
		if b := os.Getenv(env); b != "" {
			for _, prefix := range []string{"claude/", "copilot/", "codex/", "cursor/", "devin/", "changegate/"} {
				if strings.HasPrefix(b, prefix) {
					add(matchAgent(prefix), "branch "+b)
				}
			}
		}
	}
	if actor := os.Getenv("GITHUB_ACTOR"); strings.HasSuffix(actor, "[bot]") {
		if a := matchAgent(actor); a != "" {
			add(a, "GitHub actor "+actor)
		}
	}
	if ev := os.Getenv("GITHUB_EVENT_PATH"); ev != "" {
		if b, err := os.ReadFile(ev); err == nil {
			var e struct {
				PullRequest struct {
					User struct {
						Login string `json:"login"`
						Type  string `json:"type"`
					} `json:"user"`
				} `json:"pull_request"`
			}
			if json.Unmarshal(b, &e) == nil && e.PullRequest.User.Type == "Bot" {
				name := matchAgent(e.PullRequest.User.Login)
				if name == "" {
					name = "bot"
				}
				add(name, "PR opened by bot "+e.PullRequest.User.Login)
			}
		}
	}
	return p
}
