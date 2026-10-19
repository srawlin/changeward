package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srawlin/changegate/adapters/terraform"
	"github.com/srawlin/changegate/internal/lanea"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/pkg/adapter"
)

const tfMarker = "<!-- changegate-terraform -->"

func tfCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tf", Short: "Terraform/OpenTofu in CI (Lane A)"}
	var planJSON, env, stateful string
	var github bool
	review := &cobra.Command{
		Use:   "review --plan-json FILE",
		Short: "Classify a plan (terraform show -json) for review and gate it with policy",
		Long: `Review a plan produced by your pipeline (terraform plan -out p && terraform show -json p > plan.json).
Every resource change is classified; destroying or replacing a stateful resource
(databases, buckets, volumes, keys, DNS zones) is irreversible. Policy decides in the
CI lane. Exit codes: 0 approved, 2 review required, 3 denied.`,
		RunE: func(*cobra.Command, []string) error {
			b, err := os.ReadFile(planJSON)
			if err != nil {
				return err
			}
			extra := map[string]bool{}
			for _, s := range strings.Split(stateful, ",") {
				if s = strings.TrimSpace(s); s != "" {
					extra[s] = true
				}
			}
			changes, err := terraform.ClassifyPlanJSON(b, extra)
			if err != nil {
				return err
			}
			rev, destructive, effects, diff, warnings := terraform.Summarize(changes)
			if len(changes) == 0 {
				rev = adapter.Reversible
			}
			plan := &adapter.Plan{Adapter: "terraform", Env: env, Reversibility: rev, Destructive: destructive, BlastRows: int64(len(changes)),
				Effects: effects, Warnings: warnings, RehearsalTier: "plan", Operation: adapter.Operation{Kind: "terraform"}}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			pol, err := a.policy()
			if err != nil {
				return err
			}
			wd, _ := os.Getwd()
			prov := lanea.DetectProvenance(repoRoot(wd), os.Getenv("CHANGEGATE_BASE_REF"), "")
			principal := map[string]any{"kind": "human", "agent": "", "human": prov.Human}
			if prov.Agent {
				principal["kind"], principal["agent"] = "agent", prov.Name
			}
			d := pol.Decide(policy.Input{Change: policy.ChangeInput(plan, model.LaneCI, true, nil), Principal: principal,
				Session: map[string]any{"incident_ref": "", "has_checkpoint": false, "spend_usd": 0.0}})
			md := tfMarkdown(diff, warnings, rev, d, prov, env)
			if jsonOut {
				_ = printJSON(map[string]any{"changes": changes, "reversibility": rev, "decision": d})
			} else {
				fmt.Print(md)
			}
			if github {
				g, err := lanea.GitHubFromEnv()
				if err != nil {
					return err
				}
				if _, err := g.CommentWithMarker(md, tfMarker); err != nil {
					return err
				}
			}
			switch d.Decision {
			case policy.Deny:
				return exitError{code: 3}
			case policy.Require:
				return exitError{code: 2}
			}
			return nil
		},
	}
	review.Flags().StringVar(&planJSON, "plan-json", "", "output of terraform show -json PLANFILE")
	review.Flags().StringVar(&env, "env", "prod", "environment class the plan applies to")
	review.Flags().StringVar(&stateful, "stateful-types", "", "extra resource types to treat as stateful")
	review.Flags().BoolVar(&github, "github", false, "post the review as a PR comment")
	cmd.AddCommand(review)
	return cmd
}

func tfMarkdown(diff string, warnings []string, rev adapter.Reversibility, d model.Decision, prov lanea.Provenance, env string) string {
	var b strings.Builder
	b.WriteString(tfMarker + "\n")
	icon := map[adapter.Reversibility]string{adapter.Irreversible: "❌", adapter.Compensatable: "⚠️", adapter.Reversible: "✅"}[rev]
	fmt.Fprintf(&b, "## %s Changegate: Terraform plan · %s\n\n", icon, rev)
	if prov.Agent {
		fmt.Fprintf(&b, "**Provenance:** agent-authored (%s)\n\n", prov.Name)
	}
	switch d.Decision {
	case policy.Approve:
		fmt.Fprintf(&b, "**Policy (%s):** ✅ auto-approved (%s)\n\n", env, strings.Join(d.Rules, ", "))
	case policy.Deny:
		fmt.Fprintf(&b, "**Policy (%s):** ⛔ blocked: %s\n\n", env, strings.Join(d.Reasons, "; "))
	default:
		fmt.Fprintf(&b, "**Policy (%s):** 👀 review required by %s: %s\n\n", env, strings.Join(d.Approvers, ", "), strings.Join(d.Reasons, "; "))
	}
	b.WriteString("```diff\n" + diff + "```\n")
	for _, w := range warnings {
		b.WriteString("- ⚠️ " + w + "\n")
	}
	b.WriteString("\n<sub>Terraform changes are compensatable at best: undo re-applies the previous configuration, and destroyed stateful resources need a snapshot.</sub>\n")
	return b.String()
}
