package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/internal/signals"
	"github.com/srawlin/changeward/pkg/adapter"
)

func sensitiveCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sensitive", Short: "Manage sensitive-column redaction"}
	var target string
	var threshold float64
	var apply bool
	suggest := &cobra.Command{
		Use:   "suggest",
		Short: "Suggest columns to redact, using Jev on table/column names and types only (never values)",
		Long: `Ask TypeSafe's Jev, for every column of the target, whether its name and type suggest
personal or secret data. Only schema names and types are sent; row values are never read.
Suggestions can only add redaction. With --apply they are merged into the target's
sensitive list in changeward.yaml. Requires $TYPESAFE_API_KEY.`,
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			key := os.Getenv("TYPESAFE_API_KEY")
			if j := a.cfg.Signals.Jev; key == "" && j != nil {
				key = os.ExpandEnv(j.APIKey)
			}
			if key == "" {
				return errors.New("set TYPESAFE_API_KEY")
			}
			t, err := a.cfg.Resolve(target)
			if err != nil {
				return err
			}
			ad, err := a.registry().Adapter(t.Adapter)
			if err != nil {
				return err
			}
			q, ok := ad.(adapter.Querier)
			if !ok {
				return fmt.Errorf("target %s is not a SQL target", t.Name)
			}
			sql := `SELECT table_schema || '.' || table_name, column_name, data_type FROM information_schema.columns
				WHERE table_schema NOT IN ('pg_catalog','information_schema','_changeward_trash') AND table_schema NOT LIKE 'pg_%' ORDER BY 1, ordinal_position`
			if t.Adapter == "mysql" {
				sql = `SELECT CONCAT(table_schema, '.', table_name), column_name, data_type FROM information_schema.columns
					WHERE table_schema = DATABASE() ORDER BY 1, ordinal_position`
			}
			res, err := q.Query(bg(), t, sql, 5000)
			if err != nil {
				return err
			}
			jev := &signals.Jev{APIKey: key}
			type col struct{ table, name, typ string }
			var cols []col
			for _, r := range res.Rows {
				cols = append(cols, col{r[0], r[1], r[2]})
			}
			scores := map[string]float64{}
			for i := 0; i < len(cols); i += 25 {
				batch := cols[i:min(i+25, len(cols))]
				qs := map[string]string{}
				var schema []string
				for j, c := range batch {
					id := fmt.Sprintf("c%d", j)
					qs[id] = fmt.Sprintf("Column %s.%s (%s) likely holds personal data (names, emails, phone numbers, addresses, birth dates, government IDs, IP addresses) or secrets (passwords, tokens, keys)", c.table, c.name, c.typ)
					schema = append(schema, fmt.Sprintf("%s.%s %s", c.table, c.name, c.typ))
				}
				ans, err := jev.Nouls(bg(), map[string]any{"database_columns": schema}, qs)
				if err != nil {
					return err
				}
				for j, c := range batch {
					scores[c.table+"."+c.name] = ans[fmt.Sprintf("c%d", j)]
				}
			}
			short := func(tc string) string { // schema.table.col → table.col
				p := strings.Split(tc, ".")
				return strings.Join(p[len(p)-2:], ".")
			}
			var picks []string
			for k, v := range scores {
				if v >= threshold {
					picks = append(picks, short(k))
				}
			}
			sort.Strings(picks)
			if jsonOut {
				return printJSON(map[string]any{"scores": scores, "suggested": picks})
			}
			if len(picks) == 0 {
				fmt.Println("no columns look sensitive")
				return nil
			}
			fmt.Printf("suggested sensitive columns for %s (score ≥ %.2f):\n", t.Name, threshold)
			for _, p := range picks {
				fmt.Printf("  %-40s %.2f\n", p, scores[findFull(scores, p)])
			}
			if !apply {
				fmt.Println("re-run with --apply to add them to changeward.yaml")
				return nil
			}
			ct := a.cfg.Targets[t.Name]
			have := map[string]bool{}
			for _, s := range ct.Sensitive {
				have[s] = true
			}
			for _, p := range picks {
				if !have[p] {
					ct.Sensitive = append(ct.Sensitive, p)
				}
			}
			a.cfg.Targets[t.Name] = ct
			if err := a.cfg.Save(); err != nil {
				return err
			}
			fmt.Println("updated", a.cfg.Path)
			return nil
		},
	}
	suggest.Flags().StringVarP(&target, "target", "t", "", "SQL target")
	suggest.Flags().Float64Var(&threshold, "threshold", 0.7, "minimum score to suggest a column")
	suggest.Flags().BoolVar(&apply, "apply", false, "add the suggestions to the target's sensitive list")
	cmd.AddCommand(suggest)
	return cmd
}

func findFull(scores map[string]float64, short string) string {
	for k := range scores {
		if strings.HasSuffix(k, "."+short) || k == short {
			return k
		}
	}
	return short
}
