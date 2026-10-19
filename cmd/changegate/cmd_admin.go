package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/changegate/changegate/adapters/postgres"
	"github.com/changegate/changegate/adapters/toykv"
	"github.com/changegate/changegate/conformance"
	"github.com/changegate/changegate/internal/policy"
	"github.com/changegate/changegate/internal/store"
	"github.com/changegate/changegate/pkg/adapter"
	"github.com/spf13/cobra"
)

func auditCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Inspect and verify the tamper-evident audit log"}
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Verify the event hash chain",
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
			n, err := store.VerifyChain(bg(), st)
			if err != nil {
				return exitError{code: 5, msg: err.Error()}
			}
			fmt.Printf("audit chain intact: %d events verified\n", n)
			return nil
		},
	}
	var change, sess string
	var limit int
	logc := &cobra.Command{
		Use:   "log",
		Short: "Print audit events",
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
			evs, err := st.ListEvents(bg(), store.EventFilter{ChangeID: change, SessionID: sess})
			if err != nil {
				return err
			}
			if limit > 0 && len(evs) > limit {
				evs = evs[len(evs)-limit:]
			}
			if jsonOut {
				return printJSON(evs)
			}
			for _, e := range evs {
				fmt.Printf("%5d %s %-22s %-14s %-28s %s\n", e.Seq, e.At[:19], e.Type, e.ChangeID, e.Actor, e.Reason)
			}
			return nil
		},
	}
	logc.Flags().StringVar(&change, "change", "", "filter by change ID")
	logc.Flags().StringVar(&sess, "session", "", "filter by session ID")
	logc.Flags().IntVar(&limit, "limit", 50, "show the last N events (0 = all)")
	cmd.AddCommand(verify, logc)
	return cmd
}

func policyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Manage CEL change-control policy"}
	init := &cobra.Command{
		Use:   "init",
		Short: "Write changegate.policy.yaml with the built-in defaults as a starting point",
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			p := filepath.Join(a.cfg.Dir, "changegate.policy.yaml")
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists", p)
			}
			b := strings.ReplaceAll(string(policy.BuiltinYAML()), "name: builtin/", "name: ")
			if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", p)
			return nil
		},
	}
	check := &cobra.Command{
		Use:   "check [FILE]",
		Short: "Compile a policy file and report errors",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			path := a.cfg.PolicyPath()
			if len(args) == 1 {
				path = args[0]
			}
			if path == "" {
				fmt.Println("no policy file; built-in defaults apply")
				_, err := policy.New(nil)
				return err
			}
			f, err := policy.LoadFile(path)
			if err != nil {
				return err
			}
			if _, err := policy.New(f); err != nil {
				return err
			}
			fmt.Printf("%s: %d rules OK\n", path, len(f.Rules))
			return nil
		},
	}
	cmd.AddCommand(init, check)
	return cmd
}

func adapterCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "adapter", Short: "Adapter tools"}
	var plugin, dsn string
	var seed int64
	var n int
	var configs []string
	test := &cobra.Command{
		Use:   "test [postgres|toykv]",
		Short: "Run the adapter conformance suite",
		Long: `Run the conformance suite against a built-in adapter or a plugin binary.

  changegate adapter test postgres --dsn postgres://admin@localhost/postgres
  changegate adapter test toykv
  changegate adapter test --plugin ./changegate-adapter-foo --config path=/tmp/x

The Postgres driver creates and drops scratch databases on the server; the
DSN must be able to CREATE DATABASE. Plugins take part through the
changegate.fingerprint and changegate.corpus Plan conventions.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			var d conformance.Driver
			switch {
			case plugin != "":
				p, err := adapter.Open(plugin)
				if err != nil {
					return err
				}
				defer p.Close()
				cfg := map[string]string{}
				for _, kv := range configs {
					k, v, _ := strings.Cut(kv, "=")
					cfg[k] = v
				}
				dir, err := os.MkdirTemp("", "cg-conformance-")
				if err != nil {
					return err
				}
				defer func() { _ = os.RemoveAll(dir) }()
				i := 0
				d = conformance.Driver{Adapter: p, Seed: seed, N: n, NewTarget: func(context.Context) (adapter.Target, func(), error) {
					i++
					c := map[string]string{}
					for k, v := range cfg {
						c[k] = strings.ReplaceAll(v, "{tmp}", filepath.Join(dir, fmt.Sprint(i)))
					}
					if _, ok := c["path"]; !ok {
						c["path"] = filepath.Join(dir, fmt.Sprintf("target-%d.json", i))
					}
					return adapter.Target{Name: "conformance", Env: adapter.EnvPersonal, Config: c}, func() {}, nil
				}}
			case firstArg(args) == "toykv":
				dir, err := os.MkdirTemp("", "cg-conformance-")
				if err != nil {
					return err
				}
				defer func() { _ = os.RemoveAll(dir) }()
				i := 0
				d = conformance.Driver{Adapter: toykv.New(), Seed: seed, N: n, NewTarget: func(context.Context) (adapter.Target, func(), error) {
					i++
					return adapter.Target{Name: "kv", Adapter: "toykv", Env: adapter.EnvPersonal,
						Config: map[string]string{"path": filepath.Join(dir, fmt.Sprintf("kv-%d.json", i))}}, func() {}, nil
				}}
			case firstArg(args) == "postgres":
				if dsn == "" {
					dsn = os.Getenv("CHANGEGATE_ADMIN_DSN")
				}
				if dsn == "" {
					return errors.New("--dsn (an admin connection that can CREATE DATABASE) is required")
				}
				a := postgres.New(postgres.Options{StateDir: os.TempDir()})
				defer a.Close()
				d = postgres.ConformanceDriver(a, dsn, seed, n)
			default:
				return errors.New("name a built-in adapter (postgres, toykv) or pass --plugin")
			}
			rep := conformance.Run(ctx, d)
			if jsonOut {
				_ = printJSON(rep)
			} else {
				fmt.Print(rep.String())
			}
			if !rep.Passed {
				return exitError{code: 1}
			}
			return nil
		},
	}
	test.Flags().StringVar(&plugin, "plugin", "", "adapter plugin binary")
	test.Flags().StringVar(&dsn, "dsn", "", "admin DSN for the postgres driver")
	test.Flags().StringArrayVar(&configs, "config", nil, "plugin target config key=value ({tmp} expands to a scratch path)")
	test.Flags().Int64Var(&seed, "seed", 1, "corpus seed")
	test.Flags().IntVar(&n, "n", 30, "corpus size")
	cmd.AddCommand(test)
	return cmd
}
