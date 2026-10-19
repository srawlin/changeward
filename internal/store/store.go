// Package store persists Change Records, sessions, approvals, checkpoints and
// the append-only, hash-chained event log.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/srawlin/changegate/internal/model"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// ErrStateConflict is returned when a change was moved to another state
// concurrently (UpdateChange checks the event's from_state).
var ErrStateConflict = errors.New("change state changed concurrently; reload and retry")

// ChangeFilter selects changes.
type ChangeFilter struct {
	SessionID string
	Target    string
	States    []model.State
	Limit     int
}

// EventFilter selects events.
type EventFilter struct {
	ChangeID  string
	SessionID string
	AfterSeq  int64
	Limit     int
}

// Store is implemented by the SQLite store (local/CLI) and, in server mode,
// a Postgres store.
type Store interface {
	CreateSession(ctx context.Context, s *model.Session, e *model.Event) error
	UpdateSession(ctx context.Context, s *model.Session, e *model.Event) error
	GetSession(ctx context.Context, id string) (*model.Session, error)
	ListSessions(ctx context.Context, limit int) ([]*model.Session, error)

	// CreateChange inserts c (assigning c.Seq) and appends e atomically.
	CreateChange(ctx context.Context, c *model.Change, e *model.Event) error
	// UpdateChange saves c and appends e atomically. If e.FromState is set,
	// the update applies only if the stored state still equals it.
	UpdateChange(ctx context.Context, c *model.Change, e *model.Event) error
	// GetChange finds a change by change ID or plan ID.
	GetChange(ctx context.Context, ref string) (*model.Change, error)
	ListChanges(ctx context.Context, f ChangeFilter) ([]*model.Change, error)

	SaveChangeset(ctx context.Context, cs *model.Changeset, e *model.Event) error
	GetChangeset(ctx context.Context, id string) (*model.Changeset, error)

	SaveCheckpoint(ctx context.Context, cp *model.Checkpoint, e *model.Event) error
	GetCheckpoint(ctx context.Context, id string) (*model.Checkpoint, error)

	AppendEvent(ctx context.Context, e *model.Event) error
	ListEvents(ctx context.Context, f EventFilter) ([]*model.Event, error)

	Close() error
}

// HashEvent computes an event's chain hash: sha256 over prev_hash and the
// canonical JSON of the event with its hash field cleared.
func HashEvent(e *model.Event) string {
	c := *e
	c.Hash = ""
	b, _ := json.Marshal(c)
	h := sha256.New()
	h.Write([]byte(e.PrevHash))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// ChainError describes the first break found in the event chain.
type ChainError struct {
	Seq    int64
	Reason string
}

func (e *ChainError) Error() string {
	return "audit chain broken at event " + itoa(e.Seq) + ": " + e.Reason
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// VerifyChain walks every event in order and checks hashes and links.
// It returns the number of events verified.
func VerifyChain(ctx context.Context, s Store) (int, error) {
	prev := ""
	var after int64
	n := 0
	for {
		evs, err := s.ListEvents(ctx, EventFilter{AfterSeq: after, Limit: 1000})
		if err != nil {
			return n, err
		}
		if len(evs) == 0 {
			return n, nil
		}
		for _, e := range evs {
			if e.PrevHash != prev {
				return n, &ChainError{Seq: e.Seq, Reason: "prev_hash does not match the previous event"}
			}
			if HashEvent(e) != e.Hash {
				return n, &ChainError{Seq: e.Seq, Reason: "hash does not match content (event modified)"}
			}
			prev = e.Hash
			after = e.Seq
			n++
		}
	}
}
