// Package session implements Lane B sessions: the sequence of changes one
// agent run makes, with a timeline, dependency-aware undo, rewind and
// checkpoints. A session is to the direct lane what a PR is to Lane A.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

// TargetResolver resolves a target name to a connection (with config).
type TargetResolver func(name string) (adapter.Target, error)

// Service manages sessions.
type Service struct {
	Engine   *engine.Engine
	Store    store.Store
	Resolve  TargetResolver
	StateDir string
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// StartOptions configure a new session.
type StartOptions struct {
	Target      string
	Principal   model.Principal
	IncidentRef string
	Checkpoint  bool
	Meta        map[string]string
}

// Start begins a session and makes it current.
func (s *Service) Start(ctx context.Context, o StartOptions) (*model.Session, error) {
	t, err := s.Resolve(o.Target)
	if err != nil {
		return nil, err
	}
	sess := &model.Session{ID: adapter.NewID("s"), Principal: o.Principal, Target: t.Name, Env: t.Env,
		IncidentRef: o.IncidentRef, Meta: o.Meta, StartedAt: now()}
	if err := s.Store.CreateSession(ctx, sess, &model.Event{Type: "session.started", SessionID: sess.ID, Actor: o.Principal.String(),
		Reason: o.IncidentRef}); err != nil {
		return nil, err
	}
	if o.Checkpoint {
		if _, err := s.Checkpoint(ctx, sess.ID, "session-start"); err != nil {
			return sess, fmt.Errorf("session started, but the start checkpoint failed: %w", err)
		}
		sess, _ = s.Store.GetSession(ctx, sess.ID)
	}
	return sess, s.SetCurrent(sess.ID)
}

// End closes a session.
func (s *Service) End(ctx context.Context, id string) (*model.Session, error) {
	sess, err := s.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	sess.EndedAt = now()
	if err := s.Store.UpdateSession(ctx, sess, &model.Event{Type: "session.ended", SessionID: sess.ID, Actor: sess.Principal.String()}); err != nil {
		return nil, err
	}
	if cur, _ := s.Current(); cur == id {
		_ = os.Remove(s.currentFile())
	}
	return sess, nil
}

func (s *Service) currentFile() string { return filepath.Join(s.StateDir, "current-session") }

// SetCurrent records the current session for CLI commands.
func (s *Service) SetCurrent(id string) error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.currentFile(), []byte(id+"\n"), 0o600)
}

// Current returns the current session ID: $CHANGEGATE_SESSION or the last started.
func (s *Service) Current() (string, error) {
	if id := os.Getenv("CHANGEGATE_SESSION"); id != "" {
		return id, nil
	}
	b, err := os.ReadFile(s.currentFile())
	if err != nil {
		return "", errors.New("no current session (run `changegate session start`)")
	}
	return strings.TrimSpace(string(b)), nil
}

// Resolve a session reference ("" means current).
func (s *Service) Get(ctx context.Context, ref string) (*model.Session, error) {
	if ref == "" {
		var err error
		if ref, err = s.Current(); err != nil {
			return nil, err
		}
	}
	return s.Store.GetSession(ctx, ref)
}

// Changes returns the session's changes in order.
func (s *Service) Changes(ctx context.Context, id string) ([]*model.Change, error) {
	return s.Store.ListChanges(ctx, store.ChangeFilter{SessionID: id})
}

// Applied reports whether a change's effects are currently in place.
func Applied(c *model.Change) bool {
	switch c.State {
	case model.Applied, model.Verified:
		return true
	case model.Failed:
		return c.Receipt != nil
	}
	return false
}

func lastSegment(o string) string {
	if i := strings.LastIndexByte(o, '.'); i >= 0 {
		return o[i+1:]
	}
	return o
}

func touches(a, b *model.Change) bool {
	if a.Plan == nil || b.Plan == nil {
		return false
	}
	for _, x := range a.Plan.Classification.Objects {
		for _, y := range b.Plan.Classification.Objects {
			if lastSegment(x) == lastSegment(y) {
				return true
			}
		}
	}
	return false
}

// Dependents returns the applied changes after target (same target system)
// that touch any object target touched, transitively, newest first. They
// must be undone before target.
func Dependents(changes []*model.Change, target *model.Change) []*model.Change {
	deps := map[string]bool{target.ID: true}
	var chain []*model.Change
	for _, c := range changes {
		if c.Seq <= target.Seq || !Applied(c) || c.Target.Name != target.Target.Name {
			continue
		}
		for _, d := range append([]*model.Change{target}, chain...) {
			if deps[d.ID] && touches(c, d) {
				deps[c.ID] = true
				chain = append(chain, c)
				break
			}
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// UndoResult reports what an undo or rewind did.
type UndoResult struct {
	Undone   []*model.Change
	Failed   *model.Change
	Err      error
	Restored string // checkpoint ID if a restore happened
}

// UndoChain undoes target and (if withDependents) the changes that depend on
// it, newest first. It stops at the first failure.
func (s *Service) UndoChain(ctx context.Context, target *model.Change, withDependents bool, actor string) UndoResult {
	var res UndoResult
	order := []*model.Change{target}
	if withDependents {
		all, err := s.Store.ListChanges(ctx, store.ChangeFilter{Target: target.Target.Name})
		if err != nil {
			res.Err = err
			return res
		}
		order = append(Dependents(all, target), target)
	}
	for _, c := range order {
		t, err := s.Resolve(c.Target.Name)
		if err != nil {
			res.Err = err
			return res
		}
		if bt, ok := s.branchTarget(ctx, c); ok {
			t = bt
		}
		u, err := s.Engine.Undo(ctx, c.ID, t, actor)
		if err != nil {
			res.Failed, res.Err = u, err
			return res
		}
		res.Undone = append(res.Undone, u)
	}
	return res
}

// branchTarget returns the session's branch target if the change was made
// on a session branch.
func (s *Service) branchTarget(ctx context.Context, c *model.Change) (adapter.Target, bool) {
	if c.SessionID == "" {
		return adapter.Target{}, false
	}
	sess, err := s.Store.GetSession(ctx, c.SessionID)
	if err != nil || sess.Branch == nil || sess.Branch.Target.Name != c.Target.Name {
		return adapter.Target{}, false
	}
	return sess.Branch.Target, true
}

// TargetFor returns the connection a session's changes go to: its branch
// if one was created, otherwise the configured target.
func (s *Service) TargetFor(ctx context.Context, sess *model.Session) (adapter.Target, error) {
	if sess.Branch != nil {
		return sess.Branch.Target, nil
	}
	return s.Resolve(sess.Target)
}

// Rewind undoes every applied change in the session, newest first. If an
// undo fails and the session has a start checkpoint, useCheckpoint restores it.
func (s *Service) Rewind(ctx context.Context, id string, useCheckpoint bool, actor string) UndoResult {
	var res UndoResult
	sess, err := s.Store.GetSession(ctx, id)
	if err != nil {
		res.Err = err
		return res
	}
	changes, err := s.Changes(ctx, id)
	if err != nil {
		res.Err = err
		return res
	}
	t, err := s.TargetFor(ctx, sess)
	if err != nil {
		res.Err = err
		return res
	}
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if !Applied(c) {
			continue
		}
		u, err := s.Engine.Undo(ctx, c.ID, t, actor)
		if err != nil {
			res.Failed, res.Err = u, err
			break
		}
		res.Undone = append(res.Undone, u)
	}
	if res.Err != nil && useCheckpoint && sess.CheckpointID != "" {
		if err := s.Restore(ctx, id, sess.CheckpointID, actor); err != nil {
			res.Err = fmt.Errorf("undo failed (%v) and checkpoint restore failed: %w", res.Err, err)
			return res
		}
		res.Restored, res.Err = sess.CheckpointID, nil
	}
	return res
}

// Checkpoint takes a checkpoint of the session's target and attaches it.
func (s *Service) Checkpoint(ctx context.Context, sessionID, label string) (*model.Checkpoint, error) {
	sess, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	t, err := s.TargetFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	ad, err := s.Engine.Adapters.Adapter(t.Adapter)
	if err != nil {
		return nil, err
	}
	ref, err := ad.Checkpoint(ctx, t, label)
	if err != nil {
		return nil, err
	}
	cp := &model.Checkpoint{SessionID: sessionID, Ref: *ref}
	if err := s.Store.SaveCheckpoint(ctx, cp, &model.Event{Type: "checkpoint.created", SessionID: sessionID, Actor: sess.Principal.String(), Reason: label,
		Data: []byte(fmt.Sprintf(`{"checkpoint_id":%q}`, ref.ID))}); err != nil {
		return nil, err
	}
	if sess.CheckpointID == "" {
		sess.CheckpointID = ref.ID
		if err := s.Store.UpdateSession(ctx, sess, nil); err != nil {
			return nil, err
		}
	}
	return cp, nil
}

// Restore restores a checkpoint and marks the session's later changes undone
// by checkpoint.
func (s *Service) Restore(ctx context.Context, sessionID, checkpointID, actor string) error {
	sess, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	cp, err := s.Store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return err
	}
	t, err := s.TargetFor(ctx, sess)
	if err != nil {
		return err
	}
	ad, err := s.Engine.Adapters.Adapter(t.Adapter)
	if err != nil {
		return err
	}
	if _, err := ad.Restore(ctx, t, &cp.Ref); err != nil {
		return err
	}
	return s.Store.AppendEvent(ctx, &model.Event{Type: "checkpoint.restored", SessionID: sessionID, Actor: actor,
		Reason: "restored " + checkpointID + "; changes after it are discarded"})
}

// CreateBranch makes a private copy of the session's target and points the
// session at it (branch-as-working-target, §6.3).
func (s *Service) CreateBranch(ctx context.Context, sessionID, name string) (*model.Session, error) {
	sess, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Branch != nil {
		return nil, fmt.Errorf("session already works on branch %s", sess.Branch.Target.Name)
	}
	t, err := s.Resolve(sess.Target)
	if err != nil {
		return nil, err
	}
	if t.Env == adapter.EnvProd || t.Env == adapter.EnvProdRestricted {
		if t.Config["allow_branch"] != "true" {
			return nil, errors.New("branching a production target needs options.allow_branch: \"true\" (data leaves the production database)")
		}
	}
	ad, err := s.Engine.Adapters.Adapter(t.Adapter)
	if err != nil {
		return nil, err
	}
	ref, err := ad.ProvisionCopy(ctx, t, name)
	if err != nil {
		return nil, err
	}
	// The branch keeps the source's name so policy and logs read naturally,
	// but it is a personal copy.
	ref.Target.Name = sess.Target
	sess.Branch = ref
	sess.Env = adapter.EnvPersonal
	if err := s.Store.UpdateSession(ctx, sess, &model.Event{Type: "session.branched", SessionID: sess.ID, Actor: sess.Principal.String(),
		Reason: "working on a private branch of " + t.Name}); err != nil {
		return nil, err
	}
	return sess, nil
}

// DiscardBranch drops the session's branch.
func (s *Service) DiscardBranch(ctx context.Context, sessionID string) error {
	sess, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.Branch == nil {
		return errors.New("session has no branch")
	}
	ad, err := s.Engine.Adapters.Adapter(sess.Branch.Target.Adapter)
	if err != nil {
		return err
	}
	if err := ad.ReleaseCopy(ctx, sess.Branch); err != nil {
		return err
	}
	sess.Branch = nil
	t, err := s.Resolve(sess.Target)
	if err == nil {
		sess.Env = t.Env
	}
	return s.Store.UpdateSession(ctx, sess, &model.Event{Type: "session.branch_discarded", SessionID: sess.ID, Actor: sess.Principal.String()})
}
