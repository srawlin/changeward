// Package hooks implements coding-agent pre-tool hooks. They intercept
// shell commands that would change registered targets directly (psql,
// kubectl, terraform, helm, nginx reloads) and redirect them through the
// gate, and they stop agents from approving their own changes.
package hooks

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/pkg/adapter"
)

// Decision is a hook verdict.
type Decision struct {
	// Permission is "allow", "deny" or "ask".
	Permission string
	Reason     string
}

// Target is what the hook knows about a registered target.
type Target struct {
	Name     string
	Adapter  string
	Env      string
	Database string
	Host     string
}

// Evaluate inspects one shell command.
func Evaluate(command string, targets []Target) Decision {
	cmds := splitCommands(command)
	worst := Decision{Permission: "allow"}
	for _, c := range cmds {
		d := evaluateOne(c, command, targets)
		if rank(d.Permission) > rank(worst.Permission) {
			worst = d
		}
	}
	return worst
}

func rank(p string) int {
	switch p {
	case "deny":
		return 2
	case "ask":
		return 1
	}
	return 0
}

var separators = regexp.MustCompile(`\s*(?:&&|\|\||;|\|)\s*`)

// splitCommands splits a shell line into simple commands (best effort; quoted
// separators are kept by re-joining on quote balance).
func splitCommands(s string) []string {
	parts := separators.Split(s, -1)
	var out []string
	cur := ""
	for _, p := range parts {
		if cur != "" {
			cur += " ; " + p
		} else {
			cur = p
		}
		if balanced(cur) {
			out = append(out, strings.TrimSpace(cur))
			cur = ""
		}
	}
	if cur != "" {
		out = append(out, strings.TrimSpace(cur))
	}
	return out
}

func balanced(s string) bool {
	return strings.Count(s, `"`)%2 == 0 && strings.Count(s, `'`)%2 == 0
}

// fields splits a command into words, honoring single and double quotes.
func fields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func base(w string) string {
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		return w[i+1:]
	}
	return w
}

func evaluateOne(c, line string, targets []Target) Decision {
	w := fields(c)
	// Skip env assignments and sudo/env prefixes.
	for len(w) > 0 && (strings.Contains(w[0], "=") || w[0] == "sudo" || w[0] == "env" || w[0] == "command" || w[0] == "exec") {
		w = w[1:]
	}
	if len(w) == 0 {
		return Decision{Permission: "allow"}
	}
	switch base(w[0]) {
	case "changegate":
		return evaluateChangegate(w[1:])
	case "psql":
		return evaluatePsql(c+" "+pipedPsql(line), w[1:], targets)
	case "kubectl", "oc":
		if len(w) > 1 && oneOf(w[1], "apply", "delete", "patch", "replace", "scale", "edit", "create", "rollout", "set", "label", "annotate", "drain", "cordon", "taint") {
			if t, ok := firstOf(targets, "kubernetes"); ok {
				return Decision{"deny", fmt.Sprintf("kubectl %s changes the cluster without a preview or undo. Use Changegate instead: "+
					"`changegate exec --target %s -f manifest.yaml` for apply, or `changegate exec --target %s \"delete deploy/NAME\"` / \"scale deploy/NAME --replicas=N\"", w[1], t.Name, t.Name)}
			}
			return Decision{"ask", "kubectl " + w[1] + " changes a cluster directly; no Kubernetes target is registered with Changegate, so confirm manually"}
		}
	case "terraform", "tofu":
		if len(w) > 1 && oneOf(w[1], "apply", "destroy", "import", "state") {
			if t, ok := firstOf(targets, "terraform"); ok && oneOf(w[1], "apply", "destroy") {
				return Decision{"deny", fmt.Sprintf("%s %s bypasses Changegate's plan review and undo. Run `changegate exec --target %s %s` instead.", base(w[0]), w[1], t.Name, w[1])}
			}
			return Decision{"ask", base(w[0]) + " " + w[1] + " changes infrastructure directly; confirm manually (run plan first)"}
		}
	case "helm":
		if len(w) > 1 && oneOf(w[1], "install", "upgrade", "uninstall", "rollback", "delete") {
			return Decision{"ask", "helm " + w[1] + " changes a cluster directly; confirm manually"}
		}
	case "nginx":
		if strings.Contains(c, "-s reload") || strings.Contains(c, "-s stop") {
			return Decision{"ask", "reloading nginx changes live traffic; confirm manually (run nginx -t first)"}
		}
	case "systemctl":
		if len(w) > 2 && oneOf(w[1], "reload", "restart", "stop") && strings.Contains(w[2], "nginx") {
			return Decision{"ask", "restarting nginx changes live traffic; confirm manually"}
		}
	}
	return Decision{Permission: "allow"}
}

func firstOf(targets []Target, adapterName string) (Target, bool) {
	for _, t := range targets {
		if t.Adapter == adapterName {
			return t, true
		}
	}
	return Target{}, false
}

func oneOf(s string, xs ...string) bool {
	for _, x := range xs {
		if s == x {
			return true
		}
	}
	return false
}

var pipeIntoPsql = regexp.MustCompile(`\|\s*(\S*/)?psql\b`)

// pipedPsql marks lines that feed psql from a pipe.
func pipedPsql(line string) string {
	if pipeIntoPsql.MatchString(line) {
		return "| psql"
	}
	return ""
}

// Agents may plan, apply and undo through Changegate, but not approve, edit
// policy, rebind targets, restore checkpoints or purge trash.
func evaluateChangegate(args []string) Decision {
	if len(args) == 0 {
		return Decision{Permission: "allow"}
	}
	switch args[0] {
	case "approve", "reject", "connect", "restore":
		return Decision{"deny", fmt.Sprintf("`changegate %s` must be run by a human, not the agent. Ask the user to run it.", args[0])}
	case "policy":
		if len(args) > 1 && args[1] != "check" {
			return Decision{"deny", "agents cannot change Changegate policy"}
		}
	case "trash":
		if len(args) > 1 && args[1] == "purge" {
			return Decision{"deny", "purging soft-dropped objects is permanent; ask the user"}
		}
	case "install":
		return Decision{"ask", "installing integrations changes agent configuration"}
	}
	return Decision{Permission: "allow"}
}

// evaluatePsql extracts SQL from psql -c/--command (and notes -f / stdin
// use). Reads are allowed; anything that writes to a registered target is
// denied with instructions to go through Changegate.
func evaluatePsql(raw string, args []string, targets []Target) Decision {
	var sqls []string
	fileOrStdin := false
	conn := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--command":
			if i+1 < len(args) {
				sqls = append(sqls, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--command="):
			sqls = append(sqls, strings.TrimPrefix(a, "--command="))
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			sqls = append(sqls, a[2:])
		case a == "-f" || a == "--file" || strings.HasPrefix(a, "--file="):
			fileOrStdin = true
			i++
		case a == "-d" || a == "--dbname" || a == "-h" || a == "--host":
			if i+1 < len(args) {
				conn += " " + args[i+1]
				i++
			}
		case strings.HasPrefix(a, "postgres://") || strings.HasPrefix(a, "postgresql://") || (!strings.HasPrefix(a, "-") && conn == ""):
			conn += " " + a
		}
	}
	if strings.Contains(raw, "<<") || strings.Contains(raw, "| psql") || strings.Contains(raw, "|psql") {
		fileOrStdin = true
	}
	t, registered := matchTarget(conn, targets)
	if !registered {
		return Decision{Permission: "allow"}
	}
	if len(sqls) == 0 {
		if fileOrStdin {
			return Decision{"deny", fmt.Sprintf("psql with a script or stdin against target %q can change it without a preview. "+
				"Use the changegate MCP tools (propose_change, then apply_change), or `changegate exec -f FILE --target %s`.", t.Name, t.Name)}
		}
		return Decision{"ask", fmt.Sprintf("interactive psql session against target %q bypasses Changegate", t.Name)}
	}
	for _, sql := range sqls {
		if strings.HasPrefix(strings.TrimSpace(sql), `\`) {
			if !psqlMetaReadOnly(sql) {
				return Decision{"deny", fmt.Sprintf("psql meta-command %q against target %q can change it or run scripts outside Changegate. "+
					"Use the changegate MCP tools (propose_change, then apply_change) or `changegate exec`.", strings.Fields(sql)[0], t.Name)}
			}
			continue
		}
		_, stmts, err := postgres.Classify(sql)
		if err != nil {
			return Decision{"deny", fmt.Sprintf("Changegate could not parse this SQL (%v); fix it and use `changegate exec` or the propose_change tool", err)}
		}
		for _, s := range stmts {
			if s.Access != adapter.Read {
				return Decision{"deny", fmt.Sprintf("This psql command writes to %q (%s), so it must go through Changegate for a preview, policy check and undo. "+
					"Use the changegate MCP tools (propose_change → apply_change), or run: changegate exec --target %s %q",
					t.Name, s.Kind, t.Name, strings.Join(sqls, "; "))}
			}
		}
	}
	return Decision{Permission: "allow"}
}

// psqlMetaReadOnly reports whether a -c string made only of psql
// meta-commands (\d customers, \dt, \l ...) is read-only. Scripts (\i),
// shell escapes (\!), \copy, \gexec and large-object writes are not; nor is
// anything unknown.
func psqlMetaReadOnly(s string) bool {
	for _, part := range strings.Split(s, `\\`) { // "\\" separates commands
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, `\`) {
			return false // SQL mixed in after a separator: not just meta-commands
		}
		cmd := strings.TrimSuffix(strings.Fields(part)[0][1:], "+")
		switch {
		case strings.HasPrefix(cmd, "d") && cmd != "do", cmd == "l", cmd == "list", cmd == "z", cmd == "conninfo", cmd == "x",
			cmd == "timing", cmd == "echo", cmd == "q", cmd == "?", cmd == "h", cmd == "help", cmd == "sf", cmd == "sv", cmd == "encoding",
			cmd == "pset", cmd == "a", cmd == "t", cmd == "C", cmd == "f", cmd == "H":
		default:
			return false
		}
	}
	return true
}

// matchTarget decides whether a psql connection refers to a registered
// Postgres target. With no explicit connection info psql uses PG* defaults,
// which we conservatively treat as the default target.
func matchTarget(conn string, targets []Target) (Target, bool) {
	var pgs []Target
	for _, t := range targets {
		if t.Adapter == "postgres" {
			pgs = append(pgs, t)
		}
	}
	if len(pgs) == 0 {
		return Target{}, false
	}
	conn = strings.TrimSpace(conn)
	if conn == "" {
		return pgs[0], true
	}
	for _, t := range pgs {
		if t.Database != "" && strings.Contains(conn, t.Database) {
			return t, true
		}
	}
	return Target{}, false
}
