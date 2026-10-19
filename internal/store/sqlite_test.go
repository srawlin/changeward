package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

func open(t *testing.T) *store.SQLite {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestChangesAndChain(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sess := &model.Session{ID: "s_1", Target: "app", Env: "personal", StartedAt: "2026-01-01T00:00:00Z"}
	if err := s.CreateSession(ctx, sess, &model.Event{Type: "session.started", SessionID: sess.ID, Actor: "user:a"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c := &model.Change{ID: adapter.NewID("c"), SessionID: "s_1", State: model.Proposed, Target: adapter.Target{Name: "app"}}
		if err := s.CreateChange(ctx, c, &model.Event{Type: "change.proposed", ChangeID: c.ID, Actor: "agent"}); err != nil {
			t.Fatal(err)
		}
		if c.Seq != int64(i+1) {
			t.Fatalf("seq = %d", c.Seq)
		}
		c.State = model.Planned
		c.Plan = &adapter.Plan{ID: "p_plan" + string(rune('a'+i))}
		if err := s.UpdateChange(ctx, c, &model.Event{Type: "change.planned", ChangeID: c.ID, Actor: "engine"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetChange(ctx, "p_plana")
	if err != nil || got.Seq != 1 {
		t.Fatalf("by plan id: %v %+v", err, got)
	}
	if got, err = s.GetChange(ctx, "#3"); err != nil || got.Plan.ID != "p_planc" {
		t.Fatalf("by seq: %v", err)
	}
	if _, err := s.GetChange(ctx, "p_plan"); err == nil {
		t.Fatal("expected ambiguous prefix error")
	}
	list, err := s.ListChanges(ctx, store.ChangeFilter{SessionID: "s_1", Limit: 2})
	if err != nil || len(list) != 2 || list[0].Seq != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	n, err := store.VerifyChain(ctx, s)
	if err != nil || n != 7 {
		t.Fatalf("verify: n=%d err=%v", n, err)
	}

	// Events are append-only at the database level.
	if _, err := s.RawDB().Exec(`UPDATE events SET type = 'x' WHERE seq = 2`); err == nil {
		t.Fatal("update of events should be refused")
	}
	if _, err := s.RawDB().Exec(`DELETE FROM events WHERE seq = 2`); err == nil {
		t.Fatal("delete of events should be refused")
	}
	// Tampering that bypasses the trigger is detected by the chain.
	if _, err := s.RawDB().Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RawDB().Exec(`UPDATE events SET doc = replace(doc, 'change.planned', 'change.approved') WHERE seq = 3`); err != nil {
		t.Fatal(err)
	}
	_, err = store.VerifyChain(ctx, s)
	var ce *store.ChainError
	if !errors.As(err, &ce) || ce.Seq != 3 {
		t.Fatalf("expected chain error at 3, got %v", err)
	}
}
