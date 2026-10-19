// Command changegate is the Changegate CLI: plan, rehearse, gate, apply,
// verify and undo agent-originated changes, in CI (Lane A) and directly
// against live systems (Lane B).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

var jsonOut bool

func main() {
	root := &cobra.Command{
		Use:           "changegate",
		Short:         "Change control for AI agents: preview, approve, apply and undo agent changes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON")
	root.AddCommand(
		versionCmd(),
		connectCmd(), targetsCmd(),
		planCmd(), execCmd(), applyCmd(), approveCmd(true), approveCmd(false), showCmd(), statusCmd(),
		undoCmd(), logCmd(), rewindCmd(), sessionCmd(), checkpointCmd(), restoreCmd(), branchCmd(), trashCmd(),
		auditCmd(), policyCmd(), adapterCmd(),
		mcpCmd(), proxyCmd(), hookCmd(), installCmd(), promoteCmd(), reconcileCmd(), driftCmd(),
		migrationsCmd(), rehearseCmd(), reportCmd(), gateCmd(), detectCmd(), serveCmd(), uiCmd(), executorCmd(), changesetCmd(),
	)
	if err := root.Execute(); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "changegate:", ee.msg)
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "changegate:", err)
		os.Exit(1)
	}
}

// exitError carries a specific exit code (e.g. 2 = approval required, 3 = denied).
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(*cobra.Command, []string) { fmt.Println("changegate", version) },
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// readSQL returns SQL from args, -f file, or stdin ("-").
func readSQL(args []string, file string) (string, error) {
	if file != "" {
		var b []byte
		var err error
		if file == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(file)
		}
		return string(b), err
	}
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	if len(args) == 0 {
		return "", errors.New("provide SQL as an argument, with -f FILE, or on stdin with -")
	}
	return strings.Join(args, " "), nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
