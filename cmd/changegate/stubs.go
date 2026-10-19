package main

import (
	"errors"

	"github.com/changegate/changegate/internal/engine"

	"github.com/spf13/cobra"
)

func stub(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, RunE: func(*cobra.Command, []string) error { return errors.New("not implemented yet") }}
}

func promoteCmd() *cobra.Command    { return stub("promote", "promote a session") }
func migrationsCmd() *cobra.Command { return stub("migrations", "migrations") }
func rehearseCmd() *cobra.Command   { return stub("rehearse", "rehearse") }
func reportCmd() *cobra.Command     { return stub("report", "report") }
func gateCmd() *cobra.Command       { return stub("gate", "gate") }
func detectCmd() *cobra.Command     { return stub("detect", "detect") }
func serveCmd() *cobra.Command      { return stub("serve", "serve") }

func (a *app) notifier() engine.Notifier { return nil }
