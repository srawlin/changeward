package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srawlin/changegate/internal/executor"
	"gopkg.in/yaml.v3"
)

func executorCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "executor", Short: "Run or configure a shared executor (holds target credentials; enforces policy next to the target)"}
	var addr, cert, key string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the executor API",
		Long: `Serve the executor API for MCP servers, CLIs and SDKs (changegate mcp postgres --executor URL).
Tokens are configured under executor.tokens in changegate.yaml; the token's name is the delegating human.
Serve TLS directly (--tls-cert/--tls-key) or behind a TLS-terminating proxy.`,
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			if len(a.cfg.Executor.Tokens) == 0 {
				return errors.New("no executor.tokens configured; create one with `changegate executor token NAME`")
			}
			eng, err := a.engine()
			if err != nil {
				return err
			}
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			var toks []executor.Token
			for _, t := range a.cfg.Executor.Tokens {
				toks = append(toks, executor.Token{Name: t.Name, SHA256: t.SHA256, Roles: t.Roles})
			}
			srv := &http.Server{Addr: addr, Handler: (&executor.Server{Engine: eng, Sessions: svc, Tokens: toks}).Handler(), ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(bg(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(c)
			}()
			fmt.Fprintf(os.Stderr, "changegate executor on %s (%d targets, %d tokens)\n", addr, len(a.cfg.Targets), len(toks))
			if cert != "" {
				err = srv.ListenAndServeTLS(cert, key)
			} else {
				err = srv.ListenAndServe()
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	serve.Flags().StringVar(&addr, "addr", "127.0.0.1:8443", "listen address")
	serve.Flags().StringVar(&cert, "tls-cert", "", "TLS certificate")
	serve.Flags().StringVar(&key, "tls-key", "", "TLS key")
	var roles string
	token := &cobra.Command{
		Use:   "token NAME",
		Short: "Create an API token (prints the token once, and the config entry to add)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tok, entry := executor.NewToken(args[0], strings.Split(roles, ","))
			b, _ := yaml.Marshal(map[string]any{"executor": map[string]any{"tokens": []any{map[string]any{"name": entry.Name, "sha256": entry.SHA256, "roles": entry.Roles}}}})
			fmt.Printf("token (shown once): %s\n\nadd to changegate.yaml on the executor:\n%s", tok, b)
			return nil
		},
	}
	token.Flags().StringVar(&roles, "roles", "propose", "comma-separated roles: propose, approve, admin")
	cmd.AddCommand(serve, token)
	return cmd
}
