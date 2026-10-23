package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/srawlin/changeward/pkg/adapter"
)

// Neon branching (https://api-docs.neon.tech). Target options:
//   neon_project_id  project
//   neon_branch_id   the branch the target DSN points at (parent for new branches)
//   neon_api_key     API key; defaults to $NEON_API_KEY
//   neon_checkpoint  "lsn" (default: a point in the branch's history) or "branch"
//   neon_preserve_on_restore  "false" to not keep the pre-restore state as a
//                    branch (branch checkpoints only; Neon always keeps it for LSN
//                    restores, and the kept branch becomes an ancestor of the target)
// Branches are copy-on-write: a T3-tier copy in seconds, inside Neon.

type neonRef struct {
	ProjectID string `json:"project_id"`
	BranchID  string `json:"branch_id"`
	Name      string `json:"name"`
	// LSN checkpoints record a point in the branch's own history instead of
	// creating a branch (valid within the project's history retention window).
	LSN string `json:"lsn,omitempty"`
}

// NeonBaseURL can be overridden in tests.
var NeonBaseURL = "https://console.neon.tech/api/v2"

func neonKey(t adapter.Target) string {
	if k := t.Config["neon_api_key"]; k != "" {
		return k
	}
	return os.Getenv("NEON_API_KEY")
}

func neonDo(ctx context.Context, t adapter.Target, method, path string, body, out any) error {
	key := neonKey(t)
	if key == "" {
		return errors.New("neon: set NEON_API_KEY or options.neon_api_key")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, NeonBaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("neon %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type neonBranchResp struct {
	Branch struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"branch"`
	ConnectionURIs []struct {
		ConnectionURI string `json:"connection_uri"`
	} `json:"connection_uris"`
}

func (a *Adapter) neonCreate(ctx context.Context, t adapter.Target, name string, endpoint bool) (*neonBranchResp, error) {
	project := t.Config["neon_project_id"]
	if project == "" {
		return nil, errors.New("neon: options.neon_project_id is required")
	}
	body := map[string]any{"branch": map[string]any{"name": name}}
	if p := t.Config["neon_branch_id"]; p != "" {
		body["branch"].(map[string]any)["parent_id"] = p
	}
	if endpoint {
		body["endpoints"] = []map[string]any{{"type": "read_write"}}
	}
	var out neonBranchResp
	if err := neonDo(ctx, t, http.MethodPost, "/projects/"+project+"/branches", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (a *Adapter) neonCheckpoint(ctx context.Context, t adapter.Target, label string) (checkpointData, error) {
	if t.Config["neon_checkpoint"] != "branch" {
		branch := t.Config["neon_branch_id"]
		if branch == "" {
			return checkpointData{}, errors.New("neon: options.neon_branch_id is required for checkpoints")
		}
		pool, err := a.pool(ctx, t)
		if err != nil {
			return checkpointData{}, err
		}
		var lsn string
		if err := pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn); err != nil {
			return checkpointData{}, err
		}
		return checkpointData{Strategy: StrategyNeon, Neon: &neonRef{ProjectID: t.Config["neon_project_id"], BranchID: branch, LSN: lsn}}, nil
	}
	name := "changeward-cp-" + time.Now().UTC().Format("20060102-150405")
	if label != "" {
		name += "-" + label
	}
	out, err := a.neonCreate(ctx, t, name, false)
	if err != nil {
		return checkpointData{}, err
	}
	return checkpointData{Strategy: StrategyNeon, Neon: &neonRef{ProjectID: t.Config["neon_project_id"], BranchID: out.Branch.ID, Name: name}}, nil
}

// neonRestore restores the target branch from the checkpoint and returns the
// name of the branch that preserves the pre-restore state ("" if not kept).
func (a *Adapter) neonRestore(ctx context.Context, t adapter.Target, ref *neonRef) (string, error) {
	target := t.Config["neon_branch_id"]
	if target == "" || ref == nil {
		return "", errors.New("neon: options.neon_branch_id is required to restore")
	}
	a.dropPool(t.Config["dsn"])
	body := map[string]any{"source_branch_id": ref.BranchID}
	if ref.LSN != "" {
		body["source_lsn"] = ref.LSN
	}
	// Neon requires preserving the pre-restore state when a branch is reset
	// to its own history; for branch checkpoints it is optional.
	preserve := ""
	if ref.LSN != "" || t.Config["neon_preserve_on_restore"] != "false" {
		preserve = "changeward-pre-restore-" + time.Now().UTC().Format("20060102-150405")
		body["preserve_under_name"] = preserve
	}
	return preserve, neonDo(ctx, t, http.MethodPost, fmt.Sprintf("/projects/%s/branches/%s/restore", ref.ProjectID, target), body, nil)
}

func (a *Adapter) neonDeleteBranch(ctx context.Context, t adapter.Target, ref *neonRef) error {
	if ref == nil || ref.LSN != "" {
		return nil // LSN checkpoints own no branch
	}
	return neonDo(ctx, t, http.MethodDelete, fmt.Sprintf("/projects/%s/branches/%s", ref.ProjectID, ref.BranchID), nil, nil)
}

func (a *Adapter) neonBranch(ctx context.Context, t adapter.Target, name string) (*adapter.CopyRef, error) {
	if name == "" {
		name = "changeward-" + time.Now().UTC().Format("20060102-150405")
	}
	out, err := a.neonCreate(ctx, t, name, true)
	if err != nil {
		return nil, err
	}
	if len(out.ConnectionURIs) == 0 {
		return nil, errors.New("neon: branch created without a connection URI")
	}
	cfg := map[string]string{}
	for k, v := range t.Config {
		cfg[k] = v
	}
	cfg["dsn"] = out.ConnectionURIs[0].ConnectionURI
	cfg["neon_branch_id"] = out.Branch.ID
	data, _ := json.Marshal(checkpointData{Strategy: StrategyNeon, Neon: &neonRef{ProjectID: t.Config["neon_project_id"], BranchID: out.Branch.ID, Name: name}})
	return &adapter.CopyRef{Target: adapter.Target{Name: t.Name + "@" + name, Adapter: "postgres", Env: adapter.EnvPersonal, Config: cfg}, Data: data}, nil
}
