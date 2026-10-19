package main

import (
	"context"
	"fmt"
	"os"

	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/session"
	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "session", Short: "Manage direct-lane sessions"}
	var target, incident, agent string
	var checkpoint, branch bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Start a session (optionally with a checkpoint, or on a private branch)",
		RunE: func(*cobra.Command, []string) error {
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
			p := principal()
			if agent != "" {
				p = model.Principal{Kind: "agent", Agent: "agent:" + agent, Human: humanIdentity()}
			}
			s, err := svc.Start(ctx, session.StartOptions{Target: target, Principal: p, IncidentRef: incident, Checkpoint: checkpoint})
			if err != nil && s == nil {
				return err
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
			if branch {
				if s, err = svc.CreateBranch(ctx, s.ID, ""); err != nil {
					return fmt.Errorf("session %s started, but branching failed: %w", s.ID, err)
				}
			}
			if jsonOut {
				return printJSON(s)
			}
			fmt.Printf("session %s started on %s (%s)", s.ID, s.Target, s.Env)
			if s.CheckpointID != "" {
				fmt.Printf(" · checkpoint %s", s.CheckpointID)
			}
			if s.Branch != nil {
				fmt.Print(" · working on a private branch")
			}
			fmt.Println()
			return nil
		},
	}
	start.Flags().StringVarP(&target, "target", "t", "", "target")
	start.Flags().StringVar(&incident, "incident", "", "incident or ticket reference (required for direct prod writes)")
	start.Flags().StringVar(&agent, "agent", "", "agent name (e.g. claude-code)")
	start.Flags().BoolVar(&checkpoint, "checkpoint", false, "take a checkpoint at session start")
	start.Flags().BoolVar(&branch, "branch", false, "work on a private branch of the target")

	end := &cobra.Command{
		Use:   "end [SESSION]",
		Short: "End a session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
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
			s, err := svc.Get(ctx, firstArg(args))
			if err != nil {
				return err
			}
			if s, err = svc.End(ctx, s.ID); err != nil {
				return err
			}
			cs, _ := svc.Changes(ctx, s.ID)
			renderTimeline(os.Stdout, s, cs)
			if s.Branch != nil {
				fmt.Printf("this session works on a branch: `changegate promote %s` to turn it into a PR, or `changegate branch discard %s`\n", s.ID, s.ID)
			}
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List recent sessions",
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			st, err := a.store()
			if err != nil {
				return err
			}
			ss, err := st.ListSessions(bg(), 20)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(ss)
			}
			for _, s := range ss {
				state := "open"
				if s.EndedAt != "" {
					state = "ended"
				}
				fmt.Printf("%s  %s  %-12s %-10s %-6s %s\n", s.ID, s.StartedAt[:19], s.Target, s.Env, state, orDash(s.Principal.Agent))
			}
			return nil
		},
	}
	use := &cobra.Command{
		Use:   "use SESSION",
		Short: "Make SESSION the current session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := svc.Get(bg(), args[0])
			if err != nil {
				return err
			}
			return svc.SetCurrent(s.ID)
		},
	}
	cmd.AddCommand(start, end, list, use)
	return cmd
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func checkpointCmd() *cobra.Command {
	var label, sessionRef string
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Checkpoint the current session's target",
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := svc.Get(bg(), sessionRef)
			if err != nil {
				return err
			}
			cp, err := svc.Checkpoint(bg(), s.ID, label)
			if err != nil {
				return err
			}
			fmt.Printf("checkpoint %s of %s\n", cp.Ref.ID, s.Target)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "label")
	cmd.Flags().StringVar(&sessionRef, "session", "", "session (default: current)")
	return cmd
}

func restoreCmd() *cobra.Command {
	var sessionRef string
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore CHECKPOINT",
		Short: "Restore a checkpoint (discards everything after it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			cp, err := svc.Store.GetCheckpoint(bg(), args[0])
			if err != nil {
				return err
			}
			sid := cp.SessionID
			if sessionRef != "" {
				sid = sessionRef
			}
			if !yes {
				return exitError{code: 2, msg: "restore discards every change made after the checkpoint; re-run with --yes"}
			}
			if err := svc.Restore(bg(), sid, cp.Ref.ID, humanIdentity()); err != nil {
				return err
			}
			fmt.Printf("restored %s\n", cp.Ref.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionRef, "session", "", "session")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm")
	return cmd
}

func branchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Work on a private branch of a target (branch-as-working-target)"}
	var name string
	create := &cobra.Command{
		Use:   "create [SESSION]",
		Short: "Create a branch of the session's target and point the session at it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := svc.Get(bg(), firstArg(args))
			if err != nil {
				return err
			}
			if s, err = svc.CreateBranch(bg(), s.ID, name); err != nil {
				return err
			}
			fmt.Printf("session %s now works on a private branch of %s; changes there need no approval\n", s.ID, s.Target)
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "branch name")
	discard := &cobra.Command{
		Use:   "discard [SESSION]",
		Short: "Drop the session's branch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := svc.Get(bg(), firstArg(args))
			if err != nil {
				return err
			}
			if err := svc.DiscardBranch(bg(), s.ID); err != nil {
				return err
			}
			fmt.Println("branch discarded")
			return nil
		},
	}
	cmd.AddCommand(create, discard)
	return cmd
}

func trashCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "trash", Short: "List or purge soft-dropped Postgres objects"}
	var target string
	var yes bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List soft-dropped tables and columns",
		RunE: func(*cobra.Command, []string) error {
			return withPG(target, func(ctx context.Context, c *pgx.Conn) error {
				items, err := postgres.ListTrash(ctx, c)
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(items)
				}
				if len(items) == 0 {
					fmt.Println("trash is empty")
				}
				for _, it := range items {
					fmt.Printf("%-8s %s\n", it.Kind, it.Name)
				}
				return nil
			})
		},
	}
	purge := &cobra.Command{
		Use:   "purge",
		Short: "Permanently drop soft-dropped objects (they can no longer be undone)",
		RunE: func(*cobra.Command, []string) error {
			if !yes {
				return exitError{code: 2, msg: "purge is permanent; re-run with --yes"}
			}
			return withPG(target, func(ctx context.Context, c *pgx.Conn) error {
				n, err := postgres.PurgeTrash(ctx, c)
				if err != nil {
					return err
				}
				fmt.Printf("purged %d objects\n", n)
				return nil
			})
		},
	}
	for _, c := range []*cobra.Command{list, purge} {
		c.Flags().StringVarP(&target, "target", "t", "", "target")
	}
	purge.Flags().BoolVarP(&yes, "yes", "y", false, "confirm")
	cmd.AddCommand(list, purge)
	return cmd
}

func withPG(target string, fn func(context.Context, *pgx.Conn) error) error {
	a, err := newApp()
	if err != nil {
		return err
	}
	defer a.close()
	t, err := a.cfg.Resolve(target)
	if err != nil {
		return err
	}
	if t.Adapter != "postgres" {
		return fmt.Errorf("target %s is not a postgres target", t.Name)
	}
	ctx := bg()
	c, err := pgx.Connect(ctx, t.Config["dsn"])
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	return fn(ctx, c)
}
