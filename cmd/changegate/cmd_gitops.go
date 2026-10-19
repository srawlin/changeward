package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srawlin/changegate/internal/executor"
	"github.com/srawlin/changegate/internal/gitops"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/pkg/adapter"
)

// readManifests concatenates YAML files from a file or directory (sorted).
func readManifests(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	var files []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) && filepath.Base(p) != "kustomization.yaml" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func k8sCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "k8s", Short: "Kubernetes and GitOps (Argo CD, Flux) integration"}
	var manifests, target, out, executorURL string
	gate := &cobra.Command{
		Use:   "gate --manifests DIR",
		Short: "GitOps gate: plan desired manifests against the live cluster; pass only if policy approves this exact plan",
		Long: `Plan the manifests a sync would apply (server-side dry run against live objects) and apply
policy in the CI lane. Exit 0 when auto-approved or when this exact plan hash was approved
(e.g. with changegate approve, Slack or the web UI); exit 2 while approval is pending; exit 3
when denied. Run it in CI on GitOps-repository pull requests, or as an Argo CD PreSync hook
Job (see integrations/argocd). Changegate never applies here: the GitOps controller does.`,
		RunE: func(*cobra.Command, []string) error {
			ctx := bg()
			if manifests == "" {
				return errors.New("--manifests is required")
			}
			payload, err := readManifests(manifests)
			if err == nil {
				var hooks int
				if payload, hooks, err = gitops.StripHooks(payload); err == nil && hooks > 0 {
					fmt.Fprintf(os.Stderr, "skipped %d hook resource(s) (Argo CD / Helm hooks are not desired state)\n", hooks)
				}
			}
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if executorURL == "" {
				executorURL = os.Getenv("CHANGEGATE_EXECUTOR_URL")
			}
			var t adapter.Target
			if executorURL == "" {
				if t, err = a.cfg.Resolve(target); err != nil {
					return err
				}
			} else if target == "" {
				return errors.New("--target names the executor's kubernetes target")
			}
			prov := model.Provenance{Commit: os.Getenv("ARGOCD_APP_REVISION")}
			if prov.Commit == "" {
				prov.Commit = os.Getenv("GITHUB_SHA")
			}
			var c *model.Change
			if executorURL != "" {
				c, err = executor.NewClient(executorURL, os.Getenv("CHANGEGATE_EXECUTOR_TOKEN")).GitOpsGate(ctx, target, payload, prov.Commit)
			} else {
				eng, eerr := a.engine()
				if eerr != nil {
					return eerr
				}
				c, err = gitops.Gate(ctx, eng, t, payload, principal(), prov)
			}
			if err != nil {
				return err
			}
			if jsonOut {
				_ = printJSON(c)
			} else {
				renderChangeOpts(os.Stdout, c, false)
			}
			if out != "" {
				if err := os.WriteFile(out, []byte(c.Plan.Diff), 0o644); err != nil {
					return err
				}
			}
			switch c.State {
			case model.AutoApproved, model.Approved:
				if !jsonOut {
					fmt.Println("gate: pass")
				}
				return nil
			case model.Rejected:
				return exitError{code: 3, msg: "gate: denied by policy"}
			}
			return exitError{code: 2, msg: fmt.Sprintf("gate: approval required; approve with `changegate approve %s` (or Slack/web), then retry the sync", c.Plan.ID)}
		},
	}
	gate.Flags().StringVar(&manifests, "manifests", "", "manifest file or directory (rendered YAML)")
	gate.Flags().StringVarP(&target, "target", "t", "", "kubernetes target")
	gate.Flags().StringVar(&out, "out", "", "write the plan diff here (for PR comments)")
	gate.Flags().StringVar(&executorURL, "executor", "", "run the gate on a shared executor ($CHANGEGATE_EXECUTOR_URL; token in $CHANGEGATE_EXECUTOR_TOKEN)")
	cmd.AddCommand(gate)
	return cmd
}
