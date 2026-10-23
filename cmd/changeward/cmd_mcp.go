package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/adapters/postgres"
	"github.com/srawlin/changeward/internal/executor"
	"github.com/srawlin/changeward/internal/hooks"
	"github.com/srawlin/changeward/internal/mcpproxy"
	"github.com/srawlin/changeward/internal/mcpserver"
	"github.com/srawlin/changeward/internal/model"
)

func (a *app) mcpServer(target string, checkpoint, branch bool, sessionID string) (*mcpserver.Server, error) {
	return a.mcpServerFor("postgres", target, checkpoint, branch, sessionID)
}

func (a *app) mcpServerFor(adapterName, target string, checkpoint, branch bool, sessionID string) (*mcpserver.Server, error) {
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	svc, err := a.sessions()
	if err != nil {
		return nil, err
	}
	ad, err := a.registry().Adapter(adapterName)
	if err != nil {
		return nil, err
	}
	q, ok := ad.(mcpserver.Querier)
	if !ok {
		return nil, fmt.Errorf("adapter %s has no MCP server", adapterName)
	}
	if target != "" {
		if _, err := a.cfg.Resolve(target); err != nil {
			return nil, err
		}
	} else if t, err := a.cfg.Resolve(""); err == nil {
		target = t.Name
	}
	return &mcpserver.Server{Engine: eng, Sessions: svc, Querier: q, Target: target, Human: humanIdentity(),
		Checkpoint: checkpoint, Branch: branch, SessionID: sessionID, Version: version}, nil
}

func mcpCmd() *cobra.Command {
	var target, sessionID, executorURL string
	var checkpoint, branch bool
	cmd := &cobra.Command{
		Use:   "mcp ADAPTER",
		Short: "Run Changeward's MCP server for an adapter (stdio), e.g. `changeward mcp postgres`",
		Long: `Run a local MCP server that gives an agent plan → approve → apply → undo tools
for a target instead of raw execution. Credentials stay in this process.

Add to an MCP client, e.g. Claude Code's .mcp.json:
  {"mcpServers": {"changeward": {"command": "changeward", "args": ["mcp", "postgres", "--target", "app"]}}}`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if args[0] != "postgres" && args[0] != "mysql" {
				return fmt.Errorf("adapter %q has no MCP server yet (available: postgres, mysql)", args[0])
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if executorURL == "" {
				executorURL = os.Getenv("CHANGEWARD_EXECUTOR_URL")
			}
			var s *mcpserver.Server
			if executorURL != "" {
				tok := os.Getenv("CHANGEWARD_EXECUTOR_TOKEN")
				if tok == "" {
					return errors.New("set CHANGEWARD_EXECUTOR_TOKEN to use a shared executor")
				}
				if target == "" {
					return errors.New("--target names the executor's target")
				}
				s = &mcpserver.Server{Backend: executor.NewClient(executorURL, tok), Target: target, Checkpoint: checkpoint, Branch: branch, SessionID: sessionID, Version: version}
			} else if s, err = a.mcpServerFor(args[0], target, checkpoint, branch, sessionID); err != nil {
				return err
			}
			if s.Target == "" {
				return errors.New("no target: pass --target or configure one with `changeward connect`")
			}
			ctx, stop := signal.NotifyContext(bg(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return s.Run(ctx)
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target (default: the default target)")
	cmd.Flags().StringVar(&sessionID, "session", "", "attach to an existing session instead of starting one")
	cmd.Flags().StringVar(&executorURL, "executor", "", "use a shared executor at this URL ($CHANGEWARD_EXECUTOR_URL); token from $CHANGEWARD_EXECUTOR_TOKEN")
	cmd.Flags().BoolVar(&checkpoint, "checkpoint", false, "checkpoint the target when the session starts")
	cmd.Flags().BoolVar(&branch, "branch", false, "work on a private branch of the target")
	return cmd
}

func proxyCmd() *cobra.Command {
	var target, env, name string
	var sqlTools []string
	cmd := &cobra.Command{
		Use:   "proxy [flags] -- COMMAND [ARGS...]",
		Short: "Wrap any MCP server: SQL writes become plan → approve → apply; other writes need approval",
		Long: `Wrap an existing stdio MCP server. Known SQL tools (execute_sql, query, ...)
are rewritten when --target names a Postgres target: reads pass through to the
upstream server, writes are planned, approved and applied by Changeward.
Other non-read-only tools get approval-only handling under --env policy.

  changeward proxy --target app -- npx -y @modelcontextprotocol/server-postgres $DATABASE_URL`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() < 0 && len(args) == 0 {
				return errors.New("usage: changeward proxy [--target T] -- COMMAND [ARGS...]")
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			inner, err := a.mcpServer(target, false, false, "")
			if err != nil {
				return err
			}
			if target == "" {
				inner.Target = ""
			}
			p := &mcpproxy.Proxy{Inner: inner, Command: args, SQLTools: sqlTools, Env: env, Name: name, Version: version}
			if p.Name == "" {
				p.Name = filepath.Base(args[len(args)-1])
			}
			a.registry().builtin[p.AdapterName()] = p.Adapter()
			ctx, stop := signal.NotifyContext(bg(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return p.Run(ctx)
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "Postgres target for SQL tools (enables rewriting)")
	cmd.Flags().StringVar(&env, "env", "personal", "environment class for other tools")
	cmd.Flags().StringVar(&name, "name", "", "upstream name used in change records")
	cmd.Flags().StringSliceVar(&sqlTools, "sql-tool", nil, "tool names whose argument is SQL (default: common names)")
	return cmd
}

// claudeHookInput is Claude Code's PreToolUse hook payload.
type claudeHookInput struct {
	SessionID     string          `json:"session_id"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Cwd           string          `json:"cwd"`
}

func hookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "Coding-agent hooks (called by the agent, not by people)"}
	// Claude Code and Codex share the PreToolUse wire format.
	preToolUse := func(agent string) func(*cobra.Command, []string) error {
		return func(*cobra.Command, []string) error {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			var in claudeHookInput
			if err := json.Unmarshal(b, &in); err != nil {
				return nil //nolint:nilerr // never break the agent on malformed input
			}
			if in.ToolName != "Bash" {
				return nil
			}
			var ti struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(in.ToolInput, &ti)
			if in.Cwd != "" {
				_ = os.Chdir(in.Cwd)
			}
			d := evaluateHook(ti.Command, agent, in.SessionID)
			if d.Permission == "allow" {
				return nil
			}
			return printJSON(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": d.Permission,
				"permissionDecisionReason": "Changeward: " + d.Reason,
			}})
		}
	}
	claude := &cobra.Command{Use: "claude-code", Short: "Claude Code PreToolUse hook: route direct database changes through Changeward", RunE: preToolUse("claude-code")}
	codex := &cobra.Command{Use: "codex", Short: "Codex PreToolUse hook (same wire format as Claude Code)", RunE: preToolUse("codex")}
	generic := &cobra.Command{
		Use:   "check COMMAND",
		Short: "Evaluate a shell command for any agent; exit 2 means blocked",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			d := evaluateHook(args[0], os.Getenv("CHANGEWARD_AGENT"), "")
			if jsonOut {
				_ = printJSON(d)
			} else if d.Permission != "allow" {
				fmt.Fprintln(os.Stderr, d.Reason)
			}
			if d.Permission == "deny" {
				return exitError{code: 2}
			}
			return nil
		},
	}
	cursor := &cobra.Command{
		Use:   "cursor",
		Short: "Cursor beforeShellExecution hook",
		RunE: func(*cobra.Command, []string) error {
			b, _ := io.ReadAll(os.Stdin)
			var in struct {
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
			}
			if json.Unmarshal(b, &in) != nil {
				return printJSON(map[string]string{"permission": "allow"})
			}
			if in.Cwd != "" {
				_ = os.Chdir(in.Cwd)
			}
			d := evaluateHook(in.Command, "cursor", "")
			out := map[string]string{"permission": d.Permission}
			if d.Permission != "allow" {
				out["userMessage"] = "Changeward: " + d.Reason
				out["agentMessage"] = "Changeward: " + d.Reason
			}
			return printJSON(out)
		},
	}
	gemini := &cobra.Command{
		Use:   "gemini",
		Short: "Gemini CLI BeforeTool hook (shell commands); prints {decision: deny}",
		RunE: func(*cobra.Command, []string) error {
			b, _ := io.ReadAll(os.Stdin)
			var in struct {
				ToolName  string          `json:"tool_name"`
				ToolInput json.RawMessage `json:"tool_input"`
				Cwd       string          `json:"cwd"`
			}
			if err := json.Unmarshal(b, &in); err != nil {
				return nil //nolint:nilerr // never break the agent on malformed hook input
			}
			var ti struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(in.ToolInput, &ti)
			if ti.Command == "" {
				return nil
			}
			if in.Cwd != "" {
				_ = os.Chdir(in.Cwd)
			}
			d := evaluateHook(ti.Command, "gemini-cli", "")
			if d.Permission == "allow" {
				return nil
			}
			// Exit 0 with a JSON decision (Gemini reports exit 2 as a failed hook).
			return printJSON(map[string]string{"decision": map[string]string{"deny": "deny", "ask": "ask"}[d.Permission], "reason": "Changeward: " + d.Reason})
		},
	}
	cmd.AddCommand(claude, codex, generic, cursor, gemini)
	return cmd
}

// evaluateHook evaluates a command against registered targets and records
// blocked attempts in the audit log.
func evaluateHook(command, agent, agentSession string) hooks.Decision {
	a, err := newApp()
	if err != nil {
		return hooks.Decision{Permission: "allow"}
	}
	defer a.close()
	var targets []hooks.Target
	for _, n := range a.cfg.TargetNames() {
		t, err := a.cfg.Resolve(n)
		if err != nil {
			continue
		}
		ht := hooks.Target{Name: n, Adapter: t.Adapter, Env: t.Env}
		if t.Adapter == "postgres" {
			ht.Database, _ = postgres.DatabaseName(bg(), t.Config["dsn"])
		}
		targets = append(targets, ht)
	}
	d := hooks.Evaluate(command, targets)
	if d.Permission != "allow" {
		if st, err := a.store(); err == nil {
			data, _ := json.Marshal(map[string]string{"command": command, "decision": d.Permission, "agent_session": agentSession})
			_ = st.AppendEvent(bg(), &model.Event{Type: "hook." + d.Permission, Actor: "agent:" + agent, Reason: d.Reason, Data: data})
		}
	}
	return d
}

func installCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "install AGENT",
		Short: "Install Changeward into a coding agent (claude-code, codex, cursor, gemini, opencode, agents-md)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if target == "" {
				t, err := a.cfg.Resolve("")
				if err != nil {
					return fmt.Errorf("%w (install wires the MCP server to a target)", err)
				}
				target = t.Name
			}
			exe := "changeward"
			if p, err := os.Executable(); err == nil {
				exe = p
			}
			adapterName := "postgres"
			if t, err := a.cfg.Resolve(target); err == nil && t.Adapter == "mysql" {
				adapterName = "mysql"
			}
			serverArgs := []string{"mcp", adapterName, "--target", target}
			switch args[0] {
			case "claude-code":
				mcpPath := filepath.Join(a.cfg.Dir, ".mcp.json")
				if err := mergeJSON(mcpPath, func(m map[string]any) {
					servers := sub(m, "mcpServers")
					servers["changeward"] = map[string]any{"command": exe, "args": serverArgs, "env": map[string]any{"CHANGEWARD_AGENT": "claude-code"}}
				}); err != nil {
					return err
				}
				settingsPath := filepath.Join(a.cfg.Dir, ".claude", "settings.json")
				if err := mergeJSON(settingsPath, func(m map[string]any) {
					h := sub(m, "hooks")
					entry := map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": exe + " hook claude-code"}}}
					var kept []any
					if list, ok := h["PreToolUse"].([]any); ok {
						for _, e := range list {
							if !isChangewardHook(e) {
								kept = append(kept, e)
							}
						}
					}
					h["PreToolUse"] = append(kept, entry)
					env := sub(m, "env")
					env["CHANGEWARD_AGENT"] = "claude-code"
				}); err != nil {
					return err
				}
				fmt.Printf("wrote %s (MCP server \"changeward\" for target %s)\nwrote %s (PreToolUse hook)\n", mcpPath, target, settingsPath)
				fmt.Println("restart Claude Code, then ask it to change the database: it will use propose_change / apply_change, and `changeward log` shows the session")
			case "cursor":
				p := filepath.Join(a.cfg.Dir, ".cursor", "mcp.json")
				if err := mergeJSON(p, func(m map[string]any) {
					sub(m, "mcpServers")["changeward"] = map[string]any{"command": exe, "args": serverArgs, "env": map[string]any{"CHANGEWARD_AGENT": "cursor"}}
				}); err != nil {
					return err
				}
				hp := filepath.Join(a.cfg.Dir, ".cursor", "hooks.json")
				if err := mergeJSON(hp, func(m map[string]any) {
					m["version"] = 1
					h := sub(m, "hooks")
					var kept []any
					if list, ok := h["beforeShellExecution"].([]any); ok {
						for _, e := range list {
							if em, ok := e.(map[string]any); !ok || filepath.Base(firstWord(fmt.Sprint(em["command"]))) != "changeward" {
								kept = append(kept, e)
							}
						}
					}
					h["beforeShellExecution"] = append(kept, map[string]any{"command": exe + " hook cursor"})
				}); err != nil {
					return err
				}
				fmt.Println("wrote", p, "and", hp)
			case "gemini":
				p := filepath.Join(a.cfg.Dir, ".gemini", "settings.json")
				if err := mergeJSON(p, func(m map[string]any) {
					sub(m, "mcpServers")["changeward"] = map[string]any{"command": exe, "args": serverArgs, "env": map[string]any{"CHANGEWARD_AGENT": "gemini-cli"}}
					h := sub(m, "hooks")
					var kept []any
					if list, ok := h["BeforeTool"].([]any); ok {
						for _, e := range list {
							if !isChangewardHook(e) {
								kept = append(kept, e)
							}
						}
					}
					h["BeforeTool"] = append(kept, map[string]any{"matcher": "run_shell_command", "hooks": []any{map[string]any{"type": "command", "command": exe + " hook gemini"}}})
				}); err != nil {
					return err
				}
				fmt.Println("wrote", p)
			case "agents-md":
				p := filepath.Join(a.cfg.Dir, "AGENTS.md")
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "<!-- changeward -->") {
					fmt.Println(p, "already has the Changeward section")
					return nil
				}
				section := "\n<!-- changeward -->\n## Database and infrastructure changes\n\n" +
					"This project guards `" + target + "` with Changeward. Never change it with raw `psql`, `kubectl` or `terraform`.\n" +
					"Use the `changeward` MCP tools: `query` to read, `propose_change` to preview a change (it shows exact rows,\n" +
					"locks and reversibility without changing anything), then `apply_change`. Every change can be undone with\n" +
					"`undo_change`. If a change needs approval, tell the user and wait; do not look for another way. From a shell,\n" +
					"use `changeward exec --intent \"<what the user asked>\" \"<SQL>\"`. Never run `changeward approve`.\n"
				f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return err
				}
				_, err = f.WriteString(section)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return err
				}
				fmt.Println("added the Changeward section to", p)
			case "opencode":
				p := filepath.Join(a.cfg.Dir, "opencode.json")
				if err := mergeJSON(p, func(m map[string]any) {
					if _, ok := m["$schema"]; !ok {
						m["$schema"] = "https://opencode.ai/config.json"
					}
					sub(m, "mcp")["changeward"] = map[string]any{"type": "local", "command": append([]string{exe}, serverArgs...),
						"environment": map[string]any{"CHANGEWARD_AGENT": "opencode"}, "enabled": true}
				}); err != nil {
					return err
				}
				pp := filepath.Join(a.cfg.Dir, ".opencode", "plugins", "changeward.ts")
				if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
					return err
				}
				src := strings.Replace(opencodePlugin, "__CHANGEWARD_BIN__", strings.ReplaceAll(exe, `"`, `\"`), 1)
				if err := os.WriteFile(pp, []byte(src), 0o644); err != nil {
					return err
				}
				fmt.Println("wrote", p, "(MCP server \"changeward\") and", pp, "(blocks direct shell changes)")
			case "codex":
				p := filepath.Join(a.cfg.Dir, ".codex", "config.toml")
				block := fmt.Sprintf("%s\n[mcp_servers.changeward]\ncommand = %q\nargs = %s\nenv = { CHANGEWARD_AGENT = \"codex\" }\n\n"+
					"[[hooks.PreToolUse]]\nmatcher = \"^Bash$\"\n\n[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = %q\ntimeout = 30\n%s\n",
					codexBegin, exe, tomlList(serverArgs), exe+" hook codex", codexEnd)
				if err := writeMarkedBlock(p, block); err != nil {
					return err
				}
				fmt.Println("wrote", p, "(MCP server \"changeward\" and a PreToolUse hook)")
				fmt.Println("Codex loads project config only for trusted projects; open Codex here, trust the project and approve the hook in /hooks")
			default:
				return fmt.Errorf("unknown agent %q (claude-code, codex, cursor, gemini, opencode, agents-md)", args[0])
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target the MCP server guards (default: the default target)")
	return cmd
}

func isChangewardHook(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		if hm, ok := h.(map[string]any); ok {
			if c, _ := hm["command"].(string); c != "" && filepath.Base(firstWord(c)) == "changeward" {
				return true
			}
		}
	}
	return false
}

func firstWord(s string) string {
	for i, r := range s {
		if r == ' ' {
			return s[:i]
		}
	}
	return s
}

//go:embed opencode_plugin.ts
var opencodePlugin string

const (
	codexBegin = "# >>> changeward (managed by `changeward install codex`)"
	codexEnd   = "# <<< changeward"
)

// writeMarkedBlock replaces the block between codexBegin and codexEnd in p,
// or appends it, keeping the rest of the file.
func writeMarkedBlock(p, block string) error {
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s := string(b)
	if i := strings.Index(s, codexBegin); i >= 0 {
		end := len(s)
		if j := strings.Index(s[i:], codexEnd); j >= 0 {
			end = i + j + len(codexEnd)
			if end < len(s) && s[end] == '\n' {
				end++
			}
		}
		s = s[:i] + block + s[end:]
	} else {
		if s != "" && !strings.HasSuffix(s, "\n\n") {
			s += "\n"
		}
		s += block
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(s), 0o644)
}

func tomlList(xs []string) string {
	b, _ := json.Marshal(xs)
	return string(b)
}

func sub(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	v := map[string]any{}
	m[k] = v
	return v
}

// mergeJSON edits a JSON object file in place, creating it if needed.
func mergeJSON(path string, edit func(map[string]any)) error {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	edit(m)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
