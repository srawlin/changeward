package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/internal/config"
	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/executor"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/session"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

func (a *app) sessions() (*session.Service, error) {
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	return &session.Service{Engine: eng, Store: eng.Store, Resolve: a.cfg.Resolve, StateDir: a.cfg.StateDir()}, nil
}

// currentSession returns the current session if it is open and (when
// target is given) for that target.
func (a *app) currentSession(ctx context.Context, target string) *model.Session {
	svc, err := a.sessions()
	if err != nil {
		return nil
	}
	s, err := svc.Get(ctx, "")
	if err != nil || s.EndedAt != "" {
		return nil
	}
	if target != "" && s.Target != target {
		return nil
	}
	return s
}

// targetForChange resolves the connection a change was planned against.
func (a *app) targetForChange(ctx context.Context, c *model.Change) (adapter.Target, error) {
	if c.SessionID != "" {
		svc, err := a.sessions()
		if err != nil {
			return adapter.Target{}, err
		}
		if s, err := svc.Store.GetSession(ctx, c.SessionID); err == nil && s.Branch != nil && c.Target.Env == s.Branch.Target.Env {
			return s.Branch.Target, nil
		}
	}
	return a.cfg.Resolve(c.Target.Name)
}

func connectCmd() *cobra.Command {
	var adapterName, dsn, env string
	var sensitive []string
	var makeDefault bool
	var options []string
	cmd := &cobra.Command{
		Use:   "connect NAME",
		Short: "Register a target connection and bind it to an environment class",
		Long: `Register a target in changegate.yaml. The environment class is bound to the
connection, never chosen by the agent: personal, shared-dev, staging, prod, prod-restricted.

Use ${VAR} in --dsn to keep secrets out of the file, e.g. --dsn '${DATABASE_URL}'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if !adapter.ValidEnv(env) {
				return fmt.Errorf("--env must be one of personal, shared-dev, staging, prod, prod-restricted")
			}
			opts := map[string]string{}
			for _, o := range options {
				k, v, ok := strings.Cut(o, "=")
				if !ok {
					return fmt.Errorf("--option must be key=value, got %q", o)
				}
				opts[k] = v
			}
			if len(opts) == 0 {
				opts = nil
			}
			a.cfg.Targets[args[0]] = config.Target{Adapter: adapterName, Env: env, DSN: dsn, Sensitive: sensitive, Options: opts}
			if makeDefault || len(a.cfg.Targets) == 1 {
				a.cfg.DefaultTarget = args[0]
			}
			if err := a.cfg.Validate(); err != nil {
				return err
			}
			if err := a.cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("target %s (%s, %s) saved to %s\n", args[0], adapterName, env, a.cfg.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&adapterName, "adapter", "postgres", "adapter")
	cmd.Flags().StringVar(&dsn, "dsn", "", "connection string (supports ${VAR})")
	cmd.Flags().StringVar(&env, "env", "personal", "environment class")
	cmd.Flags().StringSliceVar(&sensitive, "sensitive", nil, "sensitive columns to redact (table.column, *.column)")
	cmd.Flags().StringArrayVar(&options, "option", nil, "adapter option key=value (repeatable)")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "make this the default target")
	return cmd
}

func targetsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "targets",
		Short: "List configured targets",
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if jsonOut {
				return printJSON(a.cfg.Targets)
			}
			if len(a.cfg.Targets) == 0 {
				fmt.Println("no targets; add one with `changegate connect NAME --dsn ... --env personal`")
			}
			for _, n := range a.cfg.TargetNames() {
				t := a.cfg.Targets[n]
				def := ""
				if n == a.cfg.DefaultTarget {
					def = " (default)"
				}
				fmt.Printf("%-16s %-10s %-16s%s\n", n, t.Adapter, t.Env, def)
			}
			return nil
		},
	}
}

type planFlags struct {
	file, target, intent, sessionID string
	static, noSession               bool
}

func (f *planFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&f.file, "file", "f", "", "read SQL from FILE ('-' for stdin)")
	cmd.Flags().StringVarP(&f.target, "target", "t", "", "target name")
	cmd.Flags().StringVar(&f.intent, "intent", "", "what the change is for")
	cmd.Flags().StringVar(&f.sessionID, "session", "", "session (default: current)")
	cmd.Flags().BoolVar(&f.noSession, "no-session", false, "do not attach to the current session")
}

// propose plans SQL against a target via the engine.
func (a *app) propose(ctx context.Context, f *planFlags, sql string) (*model.Change, error) {
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	t, err := a.cfg.Resolve(f.target)
	if err != nil {
		return nil, err
	}
	sessID := f.sessionID
	if sessID == "" && !f.noSession {
		if s := a.currentSession(ctx, t.Name); s != nil {
			sessID = s.ID
		}
	}
	if sessID != "" {
		svc, _ := a.sessions()
		s, err := svc.Get(ctx, sessID)
		if err != nil {
			return nil, err
		}
		sessID = s.ID
		if t, err = svc.TargetFor(ctx, s); err != nil {
			return nil, err
		}
	}
	return eng.Propose(ctx, engine.ProposeRequest{
		Target: t, Operation: adapter.Operation{Kind: operationKind(t.Adapter), Payload: sql, Intent: f.intent},
		Principal: principal(), SessionID: sessID, Provenance: model.Provenance{Host: hostInfo()},
	})
}

// operationKind maps an adapter to the operation kind the CLI submits.
func operationKind(adapterName string) string {
	switch adapterName {
	case "postgres", "mysql":
		return "sql"
	case "toykv":
		return "kv"
	case "kubernetes":
		return "k8s"
	case "configfile":
		return "file"
	case "httpapi":
		return "http"
	}
	return adapterName
}

func hostInfo() string {
	if v := os.Getenv("CHANGEGATE_HOST"); v != "" {
		return v
	}
	return "cli"
}

func decisionExit(c *model.Change) error {
	switch c.State {
	case model.AwaitingApproval:
		return exitError{code: 2}
	case model.Rejected:
		return exitError{code: 3}
	}
	return nil
}

func planCmd() *cobra.Command {
	f := &planFlags{}
	cmd := &cobra.Command{
		Use:   "plan [SQL]",
		Short: "Preview a change: static analysis, or a live dry run against a target",
		Long: `Without a target (or with --static), plan analyzes SQL statically: statement
classification and risk findings. With a target, it runs a live dry run in a
rolled-back transaction, records the proposal, and applies policy.

Exit codes: 0 auto-approved (or static), 2 approval required, 3 denied.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			sql, err := readSQL(args, f.file)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if f.static || (f.target == "" && len(a.cfg.Targets) == 0) {
				return staticPlan(sql)
			}
			c, err := a.propose(bg(), f, sql)
			if err != nil {
				return err
			}
			if jsonOut {
				_ = printJSON(c)
			} else {
				renderChange(os.Stdout, c)
			}
			return decisionExit(c)
		},
	}
	f.register(cmd)
	cmd.Flags().BoolVar(&f.static, "static", false, "static analysis only (no database)")
	return cmd
}

func staticPlan(sql string) error {
	cls, stmts, err := postgres.Classify(sql)
	if err != nil {
		return err
	}
	findings := postgres.Lint(stmts)
	if jsonOut {
		return printJSON(map[string]any{"classification": cls, "findings": findings})
	}
	fmt.Printf("static plan · %d statements · access: %s\n", len(stmts), cls.Access)
	for i, s := range stmts {
		objs := ""
		if len(s.Objects) > 0 {
			objs = " → " + strings.Join(s.Objects, ", ")
		}
		fmt.Printf("  #%d %-16s %-11s%s\n", i+1, s.Kind, s.Access, objs)
	}
	if len(findings) > 0 {
		fmt.Println("findings:")
		fmt.Print(postgres.FormatFindings(findings))
	} else {
		fmt.Println("findings: none")
	}
	return nil
}

func execCmd() *cobra.Command {
	f := &planFlags{}
	var yes bool
	cmd := &cobra.Command{
		Use:   "exec [SQL]",
		Short: "Plan and, if policy allows, apply a change in one step",
		RunE: func(cmd *cobra.Command, args []string) error {
			sql, err := readSQL(args, f.file)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			c, err := a.propose(bg(), f, sql)
			if err != nil {
				return err
			}
			if !jsonOut {
				renderChangeOpts(os.Stderr, c, false)
			}
			return a.applyFlow(bg(), c, yes)
		},
	}
	f.register(cmd)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve inline-approvable changes without prompting")
	return cmd
}

// applyFlow approves inline (terminal) when policy allows, then applies.
func (a *app) applyFlow(ctx context.Context, c *model.Change, yes bool) error {
	eng, err := a.engine()
	if err != nil {
		return err
	}
	if c.State == model.AwaitingApproval {
		if c.Decision == nil || c.Decision.Channel != "inline" {
			fmt.Fprintf(os.Stderr, "out-of-band approval required: changegate approve %s (by someone other than the proposer)\n", c.Plan.ID)
			return exitError{code: 2}
		}
		if !yes {
			if !isTerminal(os.Stdin) {
				fmt.Fprintf(os.Stderr, "approval required: re-run with --yes or run `changegate approve %s`\n", c.Plan.ID)
				return exitError{code: 2}
			}
			fmt.Fprintf(os.Stderr, "approve and apply %s? [y/N] ", c.Plan.ID)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				return exitError{code: 2, msg: "not approved"}
			}
		}
		if c, err = eng.Approve(ctx, c.ID, engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: humanIdentity(), Channel: "terminal", Approved: true}); err != nil {
			return err
		}
	}
	if c.State == model.Rejected {
		return exitError{code: 3, msg: "change was denied by policy"}
	}
	t, err := a.targetForChange(ctx, c)
	if err != nil {
		return err
	}
	c, err = eng.Apply(ctx, c.ID, t, humanIdentity())
	if jsonOut {
		_ = printJSON(c)
	} else if c != nil {
		fmt.Printf("change %s — %s", c.ID, c.State)
		if c.Receipt != nil {
			fmt.Printf(" · %d rows · %s", c.Receipt.RowsAffected, c.Receipt.Output)
		}
		fmt.Println()
		if c.Receipt != nil {
			for _, w := range c.Receipt.Warnings {
				fmt.Println("  note:", w)
			}
		}
	}
	return err
}

func applyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "apply REF",
		Short: "Apply an approved change (REF: plan ID, change ID, or #seq)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			eng, err := a.engine()
			if err != nil {
				return err
			}
			c, err := eng.Get(bg(), args[0])
			if err != nil {
				return err
			}
			return a.applyFlow(bg(), c, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve inline-approvable changes without prompting")
	return cmd
}

func approveCmd(approve bool) *cobra.Command {
	var reason, as, channel, executorURL string
	use, short := "approve REF", "Approve a pending change (binds to its exact plan hash)"
	if !approve {
		use, short = "reject REF", "Reject a pending change"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("CHANGEGATE_AGENT") != "" {
				return errors.New("approvals cannot be issued from an agent session")
			}
			if executorURL == "" {
				executorURL = os.Getenv("CHANGEGATE_EXECUTOR_URL")
			}
			if executorURL != "" {
				// On a shared executor the approver is the token's identity.
				cl := executor.NewClient(executorURL, os.Getenv("CHANGEGATE_EXECUTOR_TOKEN"))
				c, err := cl.Get(bg(), args[0])
				if err != nil {
					return err
				}
				if c.Plan == nil {
					return fmt.Errorf("change %s has no plan", c.ID)
				}
				if !jsonOut && approve {
					fmt.Print(c.Plan.Diff)
				}
				if c, err = cl.Approve(bg(), c.ID, c.Plan.Hash, approve, reason); err != nil {
					return err
				}
				if jsonOut {
					return printJSON(c)
				}
				fmt.Printf("change %s — %s\n", c.ID, c.State)
				return nil
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			eng, err := a.engine()
			if err != nil {
				return err
			}
			c, err := eng.Get(bg(), args[0])
			if err != nil {
				return err
			}
			who := humanIdentity()
			if as != "" {
				who = "user:" + strings.TrimPrefix(as, "user:")
			}
			c, err = eng.Approve(bg(), c.ID, engine.ApproveRequest{PlanHash: c.Plan.Hash, Approver: who, Channel: channel, Approved: approve, Reason: reason})
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(c)
			}
			fmt.Printf("change %s — %s\n", c.ID, c.State)
			if c.State == model.Approved {
				fmt.Printf("next: changegate apply %s\n", c.Plan.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "reason")
	cmd.Flags().StringVar(&as, "as", "", "approver identity (local mode; server mode uses SSO)")
	cmd.Flags().StringVar(&channel, "channel", "terminal", "approval channel")
	cmd.Flags().StringVar(&executorURL, "executor", "", "approve on a shared executor (default $CHANGEGATE_EXECUTOR_URL; token from $CHANGEGATE_EXECUTOR_TOKEN, approve role)")
	return cmd
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show REF",
		Short: "Show a change record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			st, err := a.store()
			if err != nil {
				return err
			}
			c, err := st.GetChange(bg(), args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(c)
			}
			renderChange(os.Stdout, c)
			evs, err := st.ListEvents(bg(), store.EventFilter{ChangeID: c.ID})
			if err != nil {
				return err
			}
			fmt.Println("events:")
			for _, e := range evs {
				fmt.Printf("  %s  %-22s %-20s %s\n", e.At[:19], e.Type, e.Actor, e.Reason)
			}
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status REF",
		Short: "Print a change's state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			st, err := a.store()
			if err != nil {
				return err
			}
			c, err := st.GetChange(bg(), args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(map[string]any{"id": c.ID, "plan_id": c.Plan.ID, "state": c.State})
			}
			fmt.Println(c.State)
			return nil
		},
	}
}

// resolveStep turns "4" into the 4th change of the current (or given) session.
func (a *app) resolveStep(ctx context.Context, ref, sessionRef string) (*model.Change, error) {
	if n, err := strconv.Atoi(ref); err == nil && n > 0 {
		svc, err := a.sessions()
		if err != nil {
			return nil, err
		}
		s, err := svc.Get(ctx, sessionRef)
		if err != nil {
			return nil, err
		}
		cs, err := svc.Changes(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if n > len(cs) {
			return nil, fmt.Errorf("session %s has %d changes", s.ID, len(cs))
		}
		return cs[n-1], nil
	}
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	return st.GetChange(ctx, ref)
}

func undoCmd() *cobra.Command {
	var yes, only bool
	var sessionRef string
	cmd := &cobra.Command{
		Use:   "undo STEP|REF",
		Short: "Undo a change (STEP is its number in the session; dependents are undone first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := bg()
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			c, err := a.resolveStep(ctx, args[0], sessionRef)
			if err != nil {
				return err
			}
			if !session.Applied(c) {
				return fmt.Errorf("change %s is %s; nothing to undo", c.ID, c.State)
			}
			all, err := svc.Store.ListChanges(ctx, store.ChangeFilter{Target: c.Target.Name})
			if err != nil {
				return err
			}
			deps := session.Dependents(all, c)
			if len(deps) > 0 && !only {
				fmt.Fprintf(os.Stderr, "undoing %s first requires undoing %d later change(s) that touch the same objects:\n", c.ID, len(deps))
				for _, d := range deps {
					fmt.Fprintf(os.Stderr, "  %s  %s\n", d.ID, shortSQL(d))
				}
				if !yes {
					if !isTerminal(os.Stdin) {
						return exitError{code: 2, msg: "re-run with --yes to undo the chain, or --only to try just this change"}
					}
					fmt.Fprint(os.Stderr, "undo the chain? [y/N] ")
					line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
					if strings.ToLower(strings.TrimSpace(line)) != "y" {
						return exitError{code: 2, msg: "cancelled"}
					}
				}
			}
			res := svc.UndoChain(ctx, c, !only, humanIdentity())
			for _, u := range res.Undone {
				fmt.Printf("undone %s  %s\n", u.ID, shortSQL(u))
			}
			if res.Err != nil {
				var ce *adapter.ConflictError
				if errors.As(res.Err, &ce) {
					fmt.Fprintln(os.Stderr, "undo refused: the target changed since the change was applied; nothing was overwritten")
					for _, x := range ce.Conflicts {
						fmt.Fprintln(os.Stderr, "  conflict:", x)
					}
					return exitError{code: 4}
				}
				return res.Err
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "undo dependent changes without asking")
	cmd.Flags().BoolVar(&only, "only", false, "undo only this change (guards still apply)")
	cmd.Flags().StringVar(&sessionRef, "session", "", "session for STEP numbers (default: current)")
	return cmd
}

func logCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "log [SESSION]",
		Short: "Show a session's timeline (default: current session)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := bg()
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			var sessions []*model.Session
			if all {
				if sessions, err = svc.Store.ListSessions(ctx, 20); err != nil {
					return err
				}
			} else {
				ref := ""
				if len(args) == 1 {
					ref = args[0]
				}
				s, err := svc.Get(ctx, ref)
				if err != nil {
					return err
				}
				sessions = []*model.Session{s}
			}
			type out struct {
				Session *model.Session  `json:"session"`
				Changes []*model.Change `json:"changes"`
			}
			var outs []out
			for _, s := range sessions {
				cs, err := svc.Changes(ctx, s.ID)
				if err != nil {
					return err
				}
				outs = append(outs, out{s, cs})
			}
			if jsonOut {
				return printJSON(outs)
			}
			for i, o := range outs {
				if i > 0 {
					fmt.Println()
				}
				renderTimeline(os.Stdout, o.Session, o.Changes)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show recent sessions")
	return cmd
}

func rewindCmd() *cobra.Command {
	var yes, useCheckpoint bool
	cmd := &cobra.Command{
		Use:   "rewind [SESSION]",
		Short: "Undo every change in a session, newest first (or restore its start checkpoint)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := bg()
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			s, err := svc.Get(ctx, ref)
			if err != nil {
				return err
			}
			cs, _ := svc.Changes(ctx, s.ID)
			n := 0
			for _, c := range cs {
				if session.Applied(c) {
					n++
				}
			}
			if !yes {
				if !isTerminal(os.Stdin) {
					return exitError{code: 2, msg: "re-run with --yes to rewind"}
				}
				fmt.Fprintf(os.Stderr, "rewind session %s (%d applied changes)? [y/N] ", s.ID, n)
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return exitError{code: 2, msg: "cancelled"}
				}
			}
			res := svc.Rewind(ctx, s.ID, useCheckpoint, humanIdentity())
			for _, u := range res.Undone {
				fmt.Printf("undone %s  %s\n", u.ID, shortSQL(u))
			}
			if res.Restored != "" {
				fmt.Printf("restored checkpoint %s\n", res.Restored)
			}
			if res.Err != nil {
				if s.CheckpointID != "" && !useCheckpoint {
					fmt.Fprintf(os.Stderr, "hint: `changegate rewind %s --checkpoint` restores the session's start checkpoint %s\n", s.ID, s.CheckpointID)
				}
				return res.Err
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.Flags().BoolVar(&useCheckpoint, "checkpoint", false, "restore the session checkpoint if an undo fails")
	return cmd
}
