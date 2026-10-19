package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/changegate/changegate/internal/approval"
	"github.com/changegate/changegate/internal/engine"
	"github.com/spf13/cobra"
)

// notifier returns the configured out-of-band approval notifier, if any.
func (a *app) notifier() engine.Notifier {
	s := a.cfg.Approvals.Slack
	if s == nil {
		return nil
	}
	sl := &approval.Slack{WebhookURL: os.ExpandEnv(s.WebhookURL), BotToken: os.ExpandEnv(s.BotToken), Channel: os.ExpandEnv(s.Channel),
		SigningSecret: os.ExpandEnv(s.SigningSecret), Users: s.Users}
	if sl.WebhookURL == "" && (sl.BotToken == "" || sl.Channel == "") {
		return nil
	}
	return sl
}

func serveCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve approval endpoints (Slack interactions) and a JSON change API",
		Long: `Serve HTTP endpoints for out-of-band approvals:

  POST /slack/interactions   Slack interactivity request URL (signed requests only)
  GET  /changes/{ref}        change record (JSON, plan config redacted)
  GET  /healthz

Configure approvals.slack in changegate.yaml (bot_token, channel, signing_secret,
users). Slack must reach this address, e.g. through your ingress or a tunnel.`,
		RunE: func(*cobra.Command, []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			defer a.close()
			eng, err := a.engine()
			if err != nil {
				return err
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
			mux.HandleFunc("/changes/", func(w http.ResponseWriter, r *http.Request) {
				ref := strings.TrimPrefix(r.URL.Path, "/changes/")
				c, err := eng.Get(r.Context(), ref)
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(c)
			})
			if n, ok := a.notifier().(*approval.Slack); ok {
				n.Engine = eng
				if n.SigningSecret == "" {
					return errors.New("approvals.slack.signing_secret is required to accept Slack interactions")
				}
				mux.Handle("/slack/interactions", n)
			}
			srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(bg(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			fmt.Fprintf(os.Stderr, "changegate serving on %s\n", addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8484", "listen address")
	return cmd
}
