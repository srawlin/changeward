package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/changegate/changegate/adapters/postgres"
	"github.com/changegate/changegate/internal/migrations"
	"github.com/changegate/changegate/internal/model"
	"github.com/changegate/changegate/internal/session"
	"github.com/spf13/cobra"
)

func promoteCmd() *cobra.Command {
	var framework, dir, name, base string
	var includeData, pr, dryRun bool
	cmd := &cobra.Command{
		Use:   "promote [SESSION]",
		Short: "Turn a session's changes into migration files (and optionally a PR)",
		Long: `Promote converts the applied changes of a direct-lane session into a migration
for the project's framework (detected from the repository; override with
--framework/--dir). Up is the SQL as written; down is generated from the
recorded undo handles, so the rollback path is one that was exercised.
With --pr it commits to a new branch and opens a pull request carrying the
session timeline as evidence; Lane A then rehearses and gates it.

Writable frameworks: ` + strings.Join(migrations.Writable, ", "),
		Args: cobra.MaximumNArgs(1),
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
			changes, err := svc.Changes(ctx, s.ID)
			if err != nil {
				return err
			}
			var applied []*model.Change
			for _, c := range changes {
				if session.Applied(c) && c.Plan != nil && c.Plan.Adapter == "postgres" {
					applied = append(applied, c)
				}
			}
			if len(applied) == 0 {
				return fmt.Errorf("session %s has no applied Postgres changes to promote", s.ID)
			}
			root := repoRoot(a.cfg.Dir)
			set, err := chooseSet(root, framework, dir)
			if err != nil {
				return err
			}
			m := migrations.NewMigration{Name: name, Header: []string{
				fmt.Sprintf("Promoted by Changegate from session %s (%s, agent %s, user %s).", s.ID, s.Target, orDash(s.Principal.Agent), strings.TrimPrefix(s.Principal.Human, "user:")),
				"Down was generated from the recorded undo handles of the live session.",
			}}
			var notes []string
			for _, c := range applied {
				up, _, n, err := postgres.MigrationSteps(c.Operation.Payload, c.Receipt.UndoHandle, includeData)
				if err != nil {
					return err
				}
				m.Up = append(m.Up, up...)
				notes = append(notes, n...)
				if m.Name == "" && c.Operation.Intent != "" {
					m.Name = c.Operation.Intent
				}
			}
			for i := len(applied) - 1; i >= 0; i-- {
				_, down, _, _ := postgres.MigrationSteps(applied[i].Operation.Payload, applied[i].Receipt.UndoHandle, includeData)
				m.Down = append(m.Down, down...)
			}
			if len(m.Up) == 0 {
				return errors.New("nothing to promote: the session's changes were data-only (use --include-data)")
			}
			if m.Name == "" {
				m.Name = "changegate_" + s.ID
			}
			if dryRun {
				fmt.Printf("framework %s in %s\n-- up\n%s\n-- down\n%s\n", set.Framework, set.Dir, strings.Join(m.Up, ";\n"), strings.Join(m.Down, ";\n"))
				for _, n := range notes {
					fmt.Println("note:", n)
				}
				return nil
			}
			branch := ""
			if pr {
				branch = "changegate/promote-" + strings.TrimPrefix(s.ID, "s_")
				if err := git(root, "checkout", "-b", branch); err != nil {
					return err
				}
			}
			files, err := migrations.Write(root, set.Framework, set.Dir, set, m, time.Now())
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Println("wrote", f)
			}
			for _, n := range notes {
				fmt.Println("note:", n)
			}
			_ = svc.Store.AppendEvent(ctx, &model.Event{Type: "session.promoted", SessionID: s.ID, Actor: humanIdentity(), Reason: strings.Join(files, ", ")})
			if !pr {
				fmt.Println("next: review the files, commit them, and open a PR (or re-run with --pr)")
				return nil
			}
			return openPR(root, branch, base, s, applied, files, notes, set.Framework)
		},
	}
	cmd.Flags().StringVar(&framework, "framework", "", "migration framework (default: detected)")
	cmd.Flags().StringVar(&dir, "dir", "", "migrations directory, relative to the repository root")
	cmd.Flags().StringVar(&name, "name", "", "migration name (default: the first change's intent)")
	cmd.Flags().StringVar(&base, "base", "", "base branch for the PR (default: the repository default)")
	cmd.Flags().BoolVar(&includeData, "include-data", false, "include data changes (DML) in the migration")
	cmd.Flags().BoolVar(&pr, "pr", false, "commit on a new branch and open a pull request (needs git and gh)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration instead of writing it")
	return cmd
}

func repoRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return dir
	}
	return strings.TrimSpace(string(out))
}

func chooseSet(root, framework, dir string) (*migrations.Set, error) {
	sets, err := migrations.DetectAll(root)
	if err != nil {
		return nil, err
	}
	var cands []migrations.Set
	for _, s := range sets {
		if (framework == "" || s.Framework == framework) && (dir == "" || filepath.Clean(s.Dir) == filepath.Clean(dir)) {
			cands = append(cands, s)
		}
	}
	switch {
	case len(cands) == 1:
		return &cands[0], nil
	case len(cands) > 1:
		var names []string
		for _, c := range cands {
			names = append(names, c.Framework+" ("+c.Dir+")")
		}
		return nil, fmt.Errorf("several migration sets found: %s; pass --framework and --dir", strings.Join(names, ", "))
	}
	if framework == "" || dir == "" {
		return nil, errors.New("no migrations detected; pass --framework and --dir to start a new migration directory")
	}
	return &migrations.Set{Framework: framework, Dir: dir}, nil
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func openPR(root, branch, base string, s *model.Session, changes []*model.Change, files, notes []string, framework string) error {
	if err := git(root, append([]string{"add"}, files...)...); err != nil {
		return err
	}
	msg := fmt.Sprintf("Promote Changegate session %s to a %s migration\n\nChangegate-Session: %s\nChangegate-Agent: %s", s.ID, framework, s.ID, orDash(s.Principal.Agent))
	if err := git(root, "commit", "-m", msg); err != nil {
		return err
	}
	if _, lookErr := exec.LookPath("gh"); lookErr != nil {
		fmt.Printf("committed on %s; push it and open a PR (gh CLI not found)\n", branch)
		return nil
	}
	if err := git(root, "push", "-u", "origin", branch); err != nil {
		return err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Promoted from Changegate session `%s` on `%s` (agent: %s, user: %s).\n\n", s.ID, s.Target, orDash(s.Principal.Agent), strings.TrimPrefix(s.Principal.Human, "user:"))
	body.WriteString("### Session timeline\n\n| # | change | reversibility | rows | state |\n|---|---|---|---|---|\n")
	for i, c := range changes {
		fmt.Fprintf(&body, "| %d | `%s` | %s | %d | %s |\n", i+1, strings.ReplaceAll(shortSQL(c), "|", "\\|"), c.Plan.Reversibility, c.Plan.BlastRows, c.State)
	}
	if len(notes) > 0 {
		body.WriteString("\n### Notes\n\n")
		for _, n := range notes {
			body.WriteString("- " + n + "\n")
		}
	}
	body.WriteString("\nThe down migration was generated from the undo handles recorded when these changes were applied and undone-tested in the session. Lane A rehearsal will run forward → rollback → compare on this PR.\n\n<!-- changegate-session: " + s.ID + " -->\n")
	args := []string{"pr", "create", "--title", "Promote Changegate session " + s.ID, "--body", body.String(), "--head", branch}
	if base != "" {
		args = append(args, "--base", base)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
