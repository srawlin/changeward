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

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/adapters/kubernetes"
	"github.com/srawlin/changeward/adapters/postgres"
	"github.com/srawlin/changeward/internal/migrations"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/internal/session"
)

func promoteCmd() *cobra.Command { return promoteLike(false) }

func reconcileCmd() *cobra.Command { return promoteLike(true) }

func promoteLike(reconcile bool) *cobra.Command {
	var framework, dir, name, base, k8sDir string
	var includeData, pr, dryRun bool
	use, short := "promote [SESSION]", "Turn a session's changes into migration files and manifests (and optionally a PR)"
	if reconcile {
		use, short = "reconcile [SESSION]", "Open a reconciliation PR for direct changes made to a Git-managed system (keeps Git the source of truth)"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
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
			var applied, k8sChanges []*model.Change
			for _, c := range changes {
				if !session.Applied(c) || c.Plan == nil {
					continue
				}
				switch c.Plan.Adapter {
				case "postgres":
					applied = append(applied, c)
				case "kubernetes":
					k8sChanges = append(k8sChanges, c)
				}
			}
			if len(applied) == 0 && len(k8sChanges) == 0 {
				return fmt.Errorf("session %s has no applied Postgres or Kubernetes changes to promote", s.ID)
			}
			root := repoRoot(a.cfg.Dir)
			if reconcile && s.IncidentRef == "" && s.Env != "personal" {
				fmt.Fprintln(os.Stderr, "note: this session has no incident reference")
			}
			var set *migrations.Set
			if len(applied) > 0 {
				if set, err = chooseSet(root, framework, dir); err != nil {
					return err
				}
			}
			if len(k8sChanges) > 0 && k8sDir == "" {
				return errors.New("the session changed Kubernetes objects: pass --k8s-dir (the manifests directory in this repository)")
			}
			m := migrations.NewMigration{Name: name, Header: []string{
				fmt.Sprintf("Promoted by Changeward from session %s (%s, agent %s, user %s).", s.ID, s.Target, orDash(s.Principal.Agent), strings.TrimPrefix(s.Principal.Human, "user:")),
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
			var manifests []kubernetes.ManifestFile
			for _, c := range k8sChanges {
				mf, err := kubernetes.ManifestsForPromotion(c.Plan.AdapterData)
				if err != nil {
					return err
				}
				manifests = append(manifests, mf...)
			}
			if len(m.Up) == 0 && len(manifests) == 0 {
				return errors.New("nothing to promote: the session's changes were data-only (use --include-data)")
			}
			if m.Name == "" {
				m.Name = "changeward_" + s.ID
			}
			if dryRun {
				if set != nil {
					fmt.Printf("framework %s in %s\n-- up\n%s\n-- down\n%s\n", set.Framework, set.Dir, strings.Join(m.Up, ";\n"), strings.Join(m.Down, ";\n"))
				}
				for _, mf := range manifests {
					if mf.Delete {
						fmt.Printf("remove %s\n", filepath.Join(k8sDir, mf.Path))
					} else {
						fmt.Printf("write %s:\n%s", filepath.Join(k8sDir, mf.Path), mf.YAML)
					}
				}
				for _, n := range notes {
					fmt.Println("note:", n)
				}
				return nil
			}
			branch := ""
			if pr {
				branch = "changeward/promote-" + strings.TrimPrefix(s.ID, "s_")
				if reconcile {
					branch = "changeward/reconcile-" + strings.TrimPrefix(s.ID, "s_")
				}
				if err := git(root, "checkout", "-b", branch); err != nil {
					return err
				}
			}
			var files []string
			if set != nil && len(m.Up) > 0 {
				if files, err = migrations.Write(root, set.Framework, set.Dir, set, m, time.Now()); err != nil {
					return err
				}
			}
			for _, mf := range manifests {
				rel := filepath.Join(k8sDir, mf.Path)
				full := filepath.Join(root, rel)
				if mf.Delete {
					if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
						return err
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(full, []byte(mf.YAML), 0o644); err != nil {
						return err
					}
				}
				files = append(files, rel)
			}
			for _, f := range files {
				fmt.Println("wrote", f)
			}
			for _, n := range notes {
				fmt.Println("note:", n)
			}
			evType := "session.promoted"
			if reconcile {
				evType = "session.reconciled"
			}
			_ = svc.Store.AppendEvent(ctx, &model.Event{Type: evType, SessionID: s.ID, Actor: humanIdentity(), Reason: strings.Join(files, ", ")})
			if !pr {
				fmt.Println("next: review the files, commit them, and open a PR (or re-run with --pr)")
				return nil
			}
			fw := "Kubernetes"
			if set != nil {
				fw = set.Framework
			}
			return openPR(root, branch, base, s, append(applied, k8sChanges...), files, notes, fw, reconcile)
		},
	}
	cmd.Flags().StringVar(&framework, "framework", "", "migration framework (default: detected)")
	cmd.Flags().StringVar(&dir, "dir", "", "migrations directory, relative to the repository root")
	cmd.Flags().StringVar(&name, "name", "", "migration name (default: the first change's intent)")
	cmd.Flags().StringVar(&base, "base", "", "base branch for the PR (default: the repository default)")
	cmd.Flags().BoolVar(&includeData, "include-data", false, "include data changes (DML) in the migration")
	cmd.Flags().BoolVar(&pr, "pr", false, "commit on a new branch and open a pull request (needs git and gh)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration instead of writing it")
	cmd.Flags().StringVar(&k8sDir, "k8s-dir", "", "manifests directory for Kubernetes changes, relative to the repository root")
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

func openPR(root, branch, base string, s *model.Session, changes []*model.Change, files, notes []string, framework string, reconcile bool) error {
	if err := git(root, append([]string{"add", "-A", "--"}, files...)...); err != nil {
		return err
	}
	title := fmt.Sprintf("Promote Changeward session %s (%s)", s.ID, framework)
	if reconcile {
		title = fmt.Sprintf("Reconcile direct changes from %s on %s", s.ID, s.Target)
		if s.IncidentRef != "" {
			title += " (" + s.IncidentRef + ")"
		}
	}
	msg := fmt.Sprintf("%s\n\nChangeward-Session: %s\nChangeward-Agent: %s", title, s.ID, orDash(s.Principal.Agent))
	if s.IncidentRef != "" {
		msg += "\nChangeward-Incident: " + s.IncidentRef
	}
	if err := git(root, "commit", "-m", msg); err != nil {
		return err
	}
	if !onPath("gh") {
		fmt.Printf("committed on %s; push it and open a PR (gh CLI not found)\n", branch)
		return nil
	}
	if err := git(root, "push", "-u", "origin", branch); err != nil {
		return err
	}
	var body strings.Builder
	if reconcile {
		fmt.Fprintf(&body, "**Reconciliation:** these changes were applied directly to `%s` (%s) in session `%s`%s. This PR brings Git back in line with the live system; until it merges, the system has drifted from Git.\n\n",
			s.Target, s.Env, s.ID, map[bool]string{true: " for incident " + s.IncidentRef, false: ""}[s.IncidentRef != ""])
	} else {
		fmt.Fprintf(&body, "Promoted from Changeward session `%s` on `%s` (agent: %s, user: %s).\n\n", s.ID, s.Target, orDash(s.Principal.Agent), strings.TrimPrefix(s.Principal.Human, "user:"))
	}
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
	body.WriteString("\nThe down migration was generated from the undo handles recorded when these changes were applied and undone-tested in the session. Lane A rehearsal will run forward → rollback → compare on this PR.\n\n<!-- changeward-session: " + s.ID + " -->\n")
	args := []string{"pr", "create", "--title", title, "--body", body.String(), "--head", branch}
	if base != "" {
		args = append(args, "--base", base)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func onPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
