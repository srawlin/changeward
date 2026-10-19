package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/changegate/changegate/internal/model"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  started_at TEXT NOT NULL,
  doc TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS changes (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL UNIQUE,
  plan_id TEXT,
  session_id TEXT,
  target TEXT NOT NULL,
  state TEXT NOT NULL,
  doc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS changes_session ON changes(session_id, seq);
CREATE INDEX IF NOT EXISTS changes_plan ON changes(plan_id);
CREATE TABLE IF NOT EXISTS checkpoints (
  id TEXT PRIMARY KEY,
  session_id TEXT,
  doc TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  change_id TEXT,
  session_id TEXT,
  type TEXT NOT NULL,
  at TEXT NOT NULL,
  prev_hash TEXT NOT NULL,
  hash TEXT NOT NULL,
  doc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_change ON events(change_id, seq);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id, seq);
CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
`

// SQLite is the local, zero-setup store.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) the store at path.
func OpenSQLite(path string) (*SQLite, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	if path == ":memory:" {
		dsn = "file::memory:?_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init store schema: %w", err)
	}
	if path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	return &SQLite{db: db}, nil
}

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func appendEvent(ctx context.Context, tx *sql.Tx, e *model.Event) error {
	if e == nil {
		return nil
	}
	var prev sql.NullString
	var lastSeq sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT hash, seq FROM events ORDER BY seq DESC LIMIT 1`).Scan(&prev, &lastSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	e.PrevHash = prev.String
	e.Seq = lastSeq.Int64 + 1
	e.Hash = HashEvent(e)
	doc, _ := json.Marshal(e)
	_, err = tx.ExecContext(ctx, `INSERT INTO events(seq, change_id, session_id, type, at, prev_hash, hash, doc) VALUES(?,?,?,?,?,?,?,?)`,
		e.Seq, e.ChangeID, e.SessionID, e.Type, e.At, e.PrevHash, e.Hash, string(doc))
	return err
}

// AppendEvent implements Store.
func (s *SQLite) AppendEvent(ctx context.Context, e *model.Event) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return appendEvent(ctx, tx, e) })
}

// ListEvents implements Store.
func (s *SQLite) ListEvents(ctx context.Context, f EventFilter) ([]*model.Event, error) {
	q := `SELECT doc FROM events WHERE seq > ?`
	args := []any{f.AfterSeq}
	if f.ChangeID != "" {
		q += ` AND change_id = ?`
		args = append(args, f.ChangeID)
	}
	if f.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, f.SessionID)
	}
	q += ` ORDER BY seq`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*model.Event
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var e model.Event
		if err := json.Unmarshal([]byte(doc), &e); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CreateSession implements Store.
func (s *SQLite) CreateSession(ctx context.Context, sess *model.Session, e *model.Event) error {
	doc, _ := json.Marshal(sess)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, started_at, doc) VALUES(?,?,?)`, sess.ID, sess.StartedAt, string(doc)); err != nil {
			return err
		}
		return appendEvent(ctx, tx, e)
	})
}

// UpdateSession implements Store.
func (s *SQLite) UpdateSession(ctx context.Context, sess *model.Session, e *model.Event) error {
	doc, _ := json.Marshal(sess)
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET doc = ? WHERE id = ?`, string(doc), sess.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return appendEvent(ctx, tx, e)
	})
}

// GetSession implements Store. A unique ID prefix is accepted.
func (s *SQLite) GetSession(ctx context.Context, id string) (*model.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM sessions WHERE id = ? OR id LIKE ? ESCAPE '\' ORDER BY id = ? DESC LIMIT 2`, id, likePrefix(id), id)
	if err != nil {
		return nil, err
	}
	docs, err := scanDocs(rows)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, ErrNotFound
	}
	var out model.Session
	return &out, json.Unmarshal([]byte(docs[0]), &out)
}

// ListSessions implements Store, newest first.
func (s *SQLite) ListSessions(ctx context.Context, limit int) ([]*model.Session, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM sessions ORDER BY started_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	docs, err := scanDocs(rows)
	if err != nil {
		return nil, err
	}
	var out []*model.Session
	for _, d := range docs {
		var x model.Session
		if err := json.Unmarshal([]byte(d), &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, nil
}

func planID(c *model.Change) string {
	if c.Plan == nil {
		return ""
	}
	return c.Plan.ID
}

// CreateChange implements Store.
func (s *SQLite) CreateChange(ctx context.Context, c *model.Change, e *model.Event) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM changes`).Scan(&seq); err != nil {
			return err
		}
		c.Seq = seq.Int64 + 1
		doc, _ := json.Marshal(c)
		if _, err := tx.ExecContext(ctx, `INSERT INTO changes(id, seq, plan_id, session_id, target, state, doc) VALUES(?,?,?,?,?,?,?)`,
			c.ID, c.Seq, planID(c), c.SessionID, c.Target.Name, string(c.State), string(doc)); err != nil {
			return err
		}
		return appendEvent(ctx, tx, e)
	})
}

// UpdateChange implements Store.
func (s *SQLite) UpdateChange(ctx context.Context, c *model.Change, e *model.Event) error {
	doc, _ := json.Marshal(c)
	return s.tx(ctx, func(tx *sql.Tx) error {
		q := `UPDATE changes SET plan_id = ?, state = ?, doc = ? WHERE id = ?`
		args := []any{planID(c), string(c.State), string(doc), c.ID}
		if e != nil && e.FromState != "" {
			q += ` AND state = ?`
			args = append(args, string(e.FromState))
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if e != nil && e.FromState != "" {
				return ErrStateConflict
			}
			return ErrNotFound
		}
		return appendEvent(ctx, tx, e)
	})
}

func likePrefix(s string) string {
	s = strings.NewReplacer("%", "", "_", "\\_").Replace(s)
	return s + "%"
}

// GetChange implements Store. ref may be a change ID, plan ID, a unique
// prefix of either, or "#<seq>".
func (s *SQLite) GetChange(ctx context.Context, ref string) (*model.Change, error) {
	var rows *sql.Rows
	var err error
	if strings.HasPrefix(ref, "#") {
		rows, err = s.db.QueryContext(ctx, `SELECT doc FROM changes WHERE seq = ?`, strings.TrimPrefix(ref, "#"))
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT doc FROM changes WHERE id = ? OR plan_id = ? OR id LIKE ? ESCAPE '\' OR plan_id LIKE ? ESCAPE '\' LIMIT 2`,
			ref, ref, likePrefix(ref), likePrefix(ref))
	}
	if err != nil {
		return nil, err
	}
	docs, err := scanDocs(rows)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, ErrNotFound
	}
	if len(docs) > 1 {
		return nil, fmt.Errorf("change reference %q is ambiguous", ref)
	}
	var c model.Change
	return &c, json.Unmarshal([]byte(docs[0]), &c)
}

// ListChanges implements Store, in sequence order.
func (s *SQLite) ListChanges(ctx context.Context, f ChangeFilter) ([]*model.Change, error) {
	q := `SELECT doc FROM changes WHERE 1=1`
	var args []any
	if f.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, f.SessionID)
	}
	if f.Target != "" {
		q += ` AND target = ?`
		args = append(args, f.Target)
	}
	if len(f.States) > 0 {
		q += ` AND state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",") + `)`
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY seq`
	if f.Limit > 0 {
		q = `SELECT doc FROM (` + strings.Replace(q, "SELECT doc", "SELECT doc, seq", 1) + fmt.Sprintf(` DESC LIMIT %d) ORDER BY seq`, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	docs, err := scanDocs(rows)
	if err != nil {
		return nil, err
	}
	var out []*model.Change
	for _, d := range docs {
		var c model.Change
		if err := json.Unmarshal([]byte(d), &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, nil
}

// SaveCheckpoint implements Store.
func (s *SQLite) SaveCheckpoint(ctx context.Context, cp *model.Checkpoint, e *model.Event) error {
	doc, _ := json.Marshal(cp)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(id, session_id, doc) VALUES(?,?,?)`, cp.Ref.ID, cp.SessionID, string(doc)); err != nil {
			return err
		}
		return appendEvent(ctx, tx, e)
	})
}

// GetCheckpoint implements Store.
func (s *SQLite) GetCheckpoint(ctx context.Context, id string) (*model.Checkpoint, error) {
	var doc string
	err := s.db.QueryRowContext(ctx, `SELECT doc FROM checkpoints WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var cp model.Checkpoint
	return &cp, json.Unmarshal([]byte(doc), &cp)
}

func scanDocs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RawDB exposes the database for tests that simulate tampering.
func (s *SQLite) RawDB() *sql.DB { return s.db }
