package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/internal/engine"
	"github.com/srawlin/changeward/internal/model"
	"github.com/srawlin/changeward/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// changesetFile is the YAML format for `changeward changeset plan`.
type changesetFile struct {
	Name  string `yaml:"name"`
	Steps []struct {
		Target  string `yaml:"target"`
		Payload string `yaml:"payload,omitempty"` // SQL, "apply", a kubectl-style command, a JSON request
		SQL     string `yaml:"sql,omitempty"`
		File    string `yaml:"file,omitempty"` // read the payload from a file (manifests, config files, SQL)
		Intent  string `yaml:"intent,omitempty"`
	} `yaml:"steps"`
}

func changesetCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "changeset", Short: "Plan and apply ordered changes across systems as a saga (failed step → earlier steps undone)"}
	var file string
	plan := &cobra.Command{
		Use:   "plan -f CHANGESET.yaml",
		Short: "Plan every step and decide policy for the group",
		Long: `Plan a changeset. Example:

  name: add-region
  steps:
    - target: app            # postgres
      sql: ALTER TABLE orders ADD COLUMN region text
    - target: cluster        # kubernetes
      file: k8s/web.yaml
    - target: edge           # nginx (configfile)
      file: nginx/default.conf

Every step is planned against current state, so steps that depend on each
other's effects on the same target belong in one multi-statement step.`,
		RunE: func(*cobra.Command, []string) error {
			if file == "" {
				return errors.New("-f CHANGESET.yaml is required")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var cf changesetFile
			if err := yaml.Unmarshal(b, &cf); err != nil {
				return err
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
			var reqs []engine.ProposeRequest
			for i, st := range cf.Steps {
				t, err := a.cfg.Resolve(st.Target)
				if err != nil {
					return fmt.Errorf("step %d: %w", i+1, err)
				}
				payload := st.Payload
				if st.SQL != "" {
					payload = st.SQL
				}
				if st.File != "" {
					p := st.File
					if !filepath.IsAbs(p) {
						p = filepath.Join(filepath.Dir(file), p)
					}
					fb, err := os.ReadFile(p)
					if err != nil {
						return fmt.Errorf("step %d: %w", i+1, err)
					}
					payload = string(fb)
				}
				reqs = append(reqs, engine.ProposeRequest{Target: t, Operation: adapter.Operation{Kind: operationKind(t.Adapter), Payload: payload, Intent: st.Intent},
					Principal: principal()})
			}
			cs, changes, err := eng.ProposeChangeset(bg(), cf.Name, reqs)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(map[string]any{"changeset": cs, "changes": changes})
			}
			for i, c := range changes {
				fmt.Printf("── step %d · %s\n", i+1, c.Target.Name)
				renderChangeOpts(os.Stdout, c, false)
			}
			fmt.Printf("\nchangeset %s — %s\n", cs.ID, cs.State)
			if cs.State == model.CSApproved {
				fmt.Printf("next: changeward changeset apply %s\n", cs.ID)
			}
			return nil
		},
	}
	plan.Flags().StringVarP(&file, "file", "f", "", "changeset file")
	targetsFn := func(a *app) engine.TargetFunc {
		return func(c *model.Change) (adapter.Target, error) { return a.targetForChange(bg(), c) }
	}
	apply := &cobra.Command{
		Use:   "apply ID",
		Short: "Apply every step in order; on failure undo the steps already applied",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			eng, err := a.engine()
			if err != nil {
				return err
			}
			cs, err := eng.ApplyChangeset(bg(), args[0], targetsFn(a), humanIdentity())
			if cs != nil {
				fmt.Printf("changeset %s — %s\n", cs.ID, cs.State)
			}
			return err
		},
	}
	undo := &cobra.Command{
		Use:   "undo ID",
		Short: "Undo an applied changeset, last step first",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			eng, err := a.engine()
			if err != nil {
				return err
			}
			cs, err := eng.UndoChangeset(bg(), args[0], targetsFn(a), humanIdentity())
			if cs != nil {
				fmt.Printf("changeset %s — %s\n", cs.ID, cs.State)
			}
			return err
		},
	}
	cmd.AddCommand(plan, apply, undo)
	return cmd
}
