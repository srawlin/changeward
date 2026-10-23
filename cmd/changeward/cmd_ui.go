package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srawlin/changeward/internal/webui"
)

func uiCmd() *cobra.Command {
	var addr string
	var open bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the local web view of sessions, changes, diffs and audit events",
		RunE: func(*cobra.Command, []string) error {
			if host, _, err := net.SplitHostPort(addr); err == nil && host != "127.0.0.1" && host != "localhost" && host != "::1" {
				return errors.New("the local UI only listens on localhost; use server mode for shared access")
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
			svc, err := a.sessions()
			if err != nil {
				return err
			}
			srv := &http.Server{Addr: addr, Handler: webui.New(eng, svc, humanIdentity()).Handler(), ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(bg(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = srv.Shutdown(c)
			}()
			u := "http://" + addr + "/"
			fmt.Fprintln(os.Stderr, "changeward ui on", u)
			if open {
				opener := map[string]string{"darwin": "open", "linux": "xdg-open"}[runtime.GOOS]
				if opener != "" {
					_ = exec.Command(opener, u).Start()
				}
			}
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8485", "listen address (localhost only)")
	cmd.Flags().BoolVar(&open, "open", false, "open a browser")
	return cmd
}
