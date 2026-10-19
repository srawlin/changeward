package postgres

import (
	"context"
	"testing"

	"github.com/changegate/changegate/conformance"
)

// TestConformance runs the adapter conformance suite (generated operation
// corpora; apply → undo → identical state) on every reachable server.
func TestConformance(t *testing.T) {
	eachServer(t, func(t *testing.T, admin string) {
		for _, seed := range []int64{1, 2, 3} {
			a := New(Options{StateDir: t.TempDir()})
			rep := conformance.Run(context.Background(), ConformanceDriver(a, admin, seed, 30))
			a.Close()
			t.Log(rep.String())
			if !rep.Passed {
				t.Fatalf("seed %d:\n%s", seed, rep)
			}
		}
	})
}
