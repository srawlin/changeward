package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/internal/session"
	"github.com/srawlin/changeward/internal/store"
)

func driftCmd() *cobra.Command {
	var maxAge time.Duration
	var envs string
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Report direct changes not yet reconciled into Git (exit 3 if any exceed --max-age)",
		Long: `List sessions in the given environment classes that applied direct changes
to Postgres or Kubernetes targets without a promotion or reconciliation PR
(changeward promote/reconcile). Run it from cron or CI to enforce that
break-glass changes reach Git within a time limit.`,
		RunE: func(*cobra.Command, []string) error {
			ctx := bg()
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			st, err := a.store()
			if err != nil {
				return err
			}
			want := map[string]bool{}
			for _, e := range strings.Split(envs, ",") {
				want[strings.TrimSpace(e)] = true
			}
			ss, err := st.ListSessions(ctx, 1000)
			if err != nil {
				return err
			}
			type row struct {
				ID, Target, Env, Incident string
				Changes                   int
				Age                       time.Duration
			}
			var rows []row
			overdue := 0
			for _, s := range ss {
				if !want[s.Env] {
					continue
				}
				cs, err := st.ListChanges(ctx, store.ChangeFilter{SessionID: s.ID})
				if err != nil {
					return err
				}
				var first time.Time
				n := 0
				for _, c := range cs {
					if session.Applied(c) && c.Plan != nil && (c.Plan.Adapter == "postgres" || c.Plan.Adapter == "kubernetes") {
						n++
						if t, err := time.Parse(time.RFC3339Nano, c.Receipt.AppliedAt); err == nil && (first.IsZero() || t.Before(first)) {
							first = t
						}
					}
				}
				if n == 0 {
					continue
				}
				evs, err := st.ListEvents(ctx, store.EventFilter{SessionID: s.ID})
				if err != nil {
					return err
				}
				done := false
				for _, e := range evs {
					if e.Type == "session.reconciled" || e.Type == "session.promoted" {
						done = true
					}
				}
				if done {
					continue
				}
				age := time.Since(first)
				r := row{s.ID, s.Target, s.Env, s.IncidentRef, n, age.Round(time.Second)}
				if maxAge > 0 && age > maxAge {
					overdue++
				}
				rows = append(rows, r)
			}
			if jsonOut {
				_ = printJSON(rows)
			} else if len(rows) == 0 {
				fmt.Println("no unreconciled direct changes")
			} else {
				for _, r := range rows {
					fmt.Printf("%s  %-12s %-10s %d change(s)  applied %s ago  %s\n", r.ID, r.Target, r.Env, r.Changes, r.Age, r.Incident)
				}
				fmt.Println("reconcile with: changeward reconcile SESSION --pr --framework ... --dir ... [--k8s-dir ...]")
			}
			if overdue > 0 {
				return exitError{code: 3, msg: fmt.Sprintf("%d session(s) unreconciled for longer than %s", overdue, maxAge)}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&maxAge, "max-age", 0, "fail when a direct change has been unreconciled for longer than this (e.g. 24h)")
	cmd.Flags().StringVar(&envs, "env", "staging,prod,prod-restricted", "environment classes to check")
	return cmd
}
