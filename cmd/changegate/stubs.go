package main

import (
	"errors"

	"github.com/changegate/changegate/internal/engine"

	"github.com/spf13/cobra"
)

func stub(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, RunE: func(*cobra.Command, []string) error { return errors.New("not implemented yet") }}
}

func serveCmd() *cobra.Command      { return stub("serve", "serve") }

func (a *app) notifier() engine.Notifier { return nil }
