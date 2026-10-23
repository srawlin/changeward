// Package terraform is the Changeward adapter for Terraform and OpenTofu.
//
// Plan runs `terraform plan -out` and classifies every resource change
// (create, update in place, replace, destroy). Destroying or replacing a
// stateful resource (databases, buckets, volumes, keys, DNS zones) is
// irreversible unless policy inserts a snapshot first. Apply runs the exact
// saved plan file that was approved; Terraform itself rejects it if state
// changed since ("saved plan is stale"), which becomes a conflict. Undo
// restores the configuration files captured before apply and applies them
// again: compensatable, never claimed to be exact (§7.3).
//
// Target config: dir (working directory), binary (terraform or tofu),
// stateful_types (extra comma-separated resource types to treat as
// stateful), var_files (comma-separated).
// Operation kind "terraform": payload "apply" (default) or "destroy".
package terraform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srawlin/changeward/pkg/adapter"
)

// StatefulTypes are resource types whose destruction loses data.
var StatefulTypes = map[string]bool{
	"aws_db_instance": true, "aws_rds_cluster": true, "aws_rds_cluster_instance": true, "aws_dynamodb_table": true,
	"aws_s3_bucket": true, "aws_ebs_volume": true, "aws_efs_file_system": true, "aws_elasticache_cluster": true,
	"aws_elasticache_replication_group": true, "aws_kms_key": true, "aws_route53_zone": true, "aws_redshift_cluster": true,
	"aws_opensearch_domain": true, "aws_elasticsearch_domain": true, "aws_docdb_cluster": true, "aws_neptune_cluster": true,
	"aws_secretsmanager_secret": true, "aws_backup_vault": true, "aws_msk_cluster": true,
	"google_sql_database_instance": true, "google_sql_database": true, "google_storage_bucket": true, "google_compute_disk": true,
	"google_bigquery_dataset": true, "google_bigquery_table": true, "google_kms_crypto_key": true, "google_dns_managed_zone": true,
	"google_spanner_instance": true, "google_spanner_database": true, "google_redis_instance": true, "google_filestore_instance": true,
	"azurerm_storage_account": true, "azurerm_mssql_database": true, "azurerm_mssql_server": true, "azurerm_postgresql_flexible_server": true,
	"azurerm_mysql_flexible_server": true, "azurerm_cosmosdb_account": true, "azurerm_managed_disk": true, "azurerm_key_vault": true,
	"azurerm_dns_zone": true, "azurerm_redis_cache": true,
	"kubernetes_persistent_volume_claim": true, "kubernetes_persistent_volume": true, "kubernetes_namespace": true,
	"digitalocean_volume": true, "digitalocean_database_cluster": true, "neon_project": true, "neon_branch": true,
}

// Adapter implements adapter.Adapter for Terraform/OpenTofu.
type Adapter struct {
	adapter.Unimplemented
	StateDir string
}

// New returns a Terraform adapter keeping plan files under stateDir.
func New(stateDir string) *Adapter { return &Adapter{StateDir: stateDir} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "terraform", Version: "0.1.0", Kinds: []string{"terraform"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo}}, nil
}

func bin(t adapter.Target) string {
	if b := t.Config["binary"]; b != "" {
		return b
	}
	return "terraform"
}

func run(ctx context.Context, t adapter.Target, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, bin(t), args...)
	cmd.Dir = t.Config["dir"]
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	if err != nil && code == 0 {
		return nil, -1, err
	}
	if err != nil {
		return out.Bytes(), code, fmt.Errorf("%s %s: %s", bin(t), args[0], strings.TrimSpace(lastLines(stderr.String()+out.String(), 12)))
	}
	return out.Bytes(), 0, nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// Change is one classified resource change.
type Change struct {
	Address  string   `json:"address"`
	Type     string   `json:"type"`
	Actions  []string `json:"actions"`
	Action   string   `json:"action"` // create, update, replace, delete, read, no-op
	Stateful bool     `json:"stateful,omitempty"`
}

type planJSON struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Mode    string `json:"mode"`
		Change  struct {
			Actions []string `json:"actions"`
		} `json:"change"`
	} `json:"resource_changes"`
	TerraformVersion string `json:"terraform_version"`
}

// ClassifyPlanJSON classifies `terraform show -json` output. It is also used
// in Lane A to review plans produced by an existing pipeline.
func ClassifyPlanJSON(b []byte, extraStateful map[string]bool) ([]Change, error) {
	var p planJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("terraform plan JSON: %w", err)
	}
	var out []Change
	for _, rc := range p.ResourceChanges {
		if rc.Mode == "data" {
			continue
		}
		c := Change{Address: rc.Address, Type: rc.Type, Actions: rc.Change.Actions, Stateful: StatefulTypes[rc.Type] || extraStateful[rc.Type]}
		acts := strings.Join(rc.Change.Actions, ",")
		switch acts {
		case "create":
			c.Action = "create"
		case "update":
			c.Action = "update"
		case "delete":
			c.Action = "delete"
		case "delete,create", "create,delete":
			c.Action = "replace"
		case "read":
			c.Action = "read"
		default:
			c.Action = "no-op"
		}
		if c.Action == "no-op" || c.Action == "read" {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// Summarize derives reversibility, destructiveness, effects and a diff.
func Summarize(changes []Change) (adapter.Reversibility, bool, []adapter.Effect, string, []string) {
	rev := adapter.Compensatable
	destructive := false
	var effects []adapter.Effect
	var diff strings.Builder
	var warnings []string
	sym := map[string]string{"create": "+", "update": "~", "replace": "-/+", "delete": "-"}
	counts := map[string]int{}
	for _, c := range changes {
		counts[c.Action]++
		fmt.Fprintf(&diff, "   %-3s %s", sym[c.Action], c.Address)
		if c.Stateful && (c.Action == "delete" || c.Action == "replace") {
			diff.WriteString("   (stateful: data is destroyed)")
			rev = adapter.Irreversible
			warnings = append(warnings, fmt.Sprintf("%s %s is stateful; its data is destroyed unless a snapshot is taken first", c.Action, c.Address))
		}
		diff.WriteString("\n")
		if c.Action == "delete" || c.Action == "replace" {
			destructive = true
		}
		effects = append(effects, adapter.Effect{Kind: "resource-" + c.Action, Object: c.Address, Description: c.Action + " " + c.Address, Rows: 1})
	}
	header := fmt.Sprintf("%d to add, %d to change, %d to replace, %d to destroy\n", counts["create"], counts["update"], counts["replace"], counts["delete"])
	return rev, destructive, effects, header + diff.String(), warnings
}

func (a *Adapter) Classify(_ context.Context, _ adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	acc := adapter.Write
	if strings.TrimSpace(op.Payload) == "destroy" {
		acc = adapter.Destructive
	}
	return adapter.Classification{Access: acc, Statements: []adapter.StatementClass{{Kind: "TERRAFORM " + strings.ToUpper(payload(op)), Access: acc}}}, nil
}

func payload(op adapter.Operation) string {
	p := strings.TrimSpace(op.Payload)
	if p == "" {
		return "apply"
	}
	return p
}

type planData struct {
	PlanFile   string            `json:"plan_file"`
	PlanSHA    string            `json:"plan_sha256"`
	Changes    []Change          `json:"changes"`
	ConfigSnap map[string]string `json:"config_snapshot"` // configuration being applied
	// Before is the configuration of the infrastructure before this change:
	// the last configuration Changeward applied in this directory, else the
	// files at git HEAD. EmptyState means nothing was deployed (undo = destroy).
	Before     map[string]string `json:"before,omitempty"`
	BeforeFrom string            `json:"before_from,omitempty"`
	EmptyState bool              `json:"empty_state,omitempty"`
}

func (a *Adapter) lastAppliedPath(dir string) string {
	abs, _ := filepath.Abs(dir)
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(a.StateDir, "terraform", "last-applied-"+hex.EncodeToString(sum[:8])+".json")
}

// priorConfig finds the configuration the current infrastructure was built from.
func (a *Adapter) priorConfig(ctx context.Context, dir string, current map[string]string) (map[string]string, string) {
	if b, err := os.ReadFile(a.lastAppliedPath(dir)); err == nil {
		var m map[string]string
		if json.Unmarshal(b, &m) == nil {
			return m, "last apply through Changeward"
		}
	}
	out := map[string]string{}
	found := false
	for n := range current {
		cmd := exec.CommandContext(ctx, "git", "show", "HEAD:./"+n)
		cmd.Dir = dir
		if b, err := cmd.Output(); err == nil {
			out[n] = string(b)
			found = true
		}
	}
	if found {
		return out, "git HEAD"
	}
	return nil, ""
}

func isConfigFile(n string) bool {
	return strings.HasSuffix(n, ".tf") || strings.HasSuffix(n, ".tfvars") || strings.HasSuffix(n, ".tf.json") || n == ".terraform.lock.hcl"
}

func snapshotConfig(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !isConfigFile(n) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		out[n] = string(b)
	}
	return out, nil
}

func planArgs(t adapter.Target, op adapter.Operation, out string) []string {
	args := []string{"plan", "-input=false", "-lock-timeout=30s", "-out=" + out}
	if payload(op) == "destroy" {
		args = append(args, "-destroy")
	}
	for _, f := range strings.Split(t.Config["var_files"], ",") {
		if f = strings.TrimSpace(f); f != "" {
			args = append(args, "-var-file="+f)
		}
	}
	return args
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	if op.Kind != "terraform" && op.Kind != "" {
		return nil, fmt.Errorf("terraform: unsupported operation kind %q", op.Kind)
	}
	if p := payload(op); p != "apply" && p != "destroy" {
		return nil, fmt.Errorf("terraform: payload must be apply or destroy, got %q", p)
	}
	if t.Config["dir"] == "" {
		return nil, errors.New("terraform: target config dir is required")
	}
	if _, _, err := run(ctx, t, "init", "-input=false", "-no-color"); err != nil {
		return nil, err
	}
	snap, err := snapshotConfig(t.Config["dir"])
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(a.StateDir, "terraform"), 0o700); err != nil {
		return nil, err
	}
	id := adapter.NewID("p")
	planFile := filepath.Join(a.StateDir, "terraform", id+".tfplan")
	if _, _, err := run(ctx, t, append(planArgs(t, op, planFile), "-no-color")...); err != nil {
		return nil, err
	}
	js, _, err := run(ctx, t, "show", "-json", planFile)
	if err != nil {
		return nil, err
	}
	before, beforeFrom := a.priorConfig(ctx, t.Config["dir"], snap)
	stateList, _, _ := run(ctx, t, "state", "list")
	emptyState := strings.TrimSpace(string(stateList)) == ""
	extra := map[string]bool{}
	for _, s := range strings.Split(t.Config["stateful_types"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			extra[s] = true
		}
	}
	changes, err := ClassifyPlanJSON(js, extra)
	if err != nil {
		return nil, err
	}
	pb, err := os.ReadFile(planFile)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pb)
	rev, destructive, effects, diff, warnings := Summarize(changes)
	if len(changes) == 0 {
		rev = adapter.Reversible
	}
	if before == nil && !emptyState && rev != adapter.Irreversible {
		rev = adapter.CheckpointOnly
		warnings = append(warnings, "the configuration the current infrastructure was built from is unknown (no previous Changeward apply, not in git); undo needs a state backup")
	}
	cls, _ := a.Classify(ctx, t, op)
	for _, c := range changes {
		cls.Objects = append(cls.Objects, c.Address)
	}
	if destructive {
		cls.Access = adapter.Destructive
	}
	data, _ := json.Marshal(planData{PlanFile: planFile, PlanSHA: hex.EncodeToString(sum[:]), Changes: changes, ConfigSnap: snap,
		Before: before, BeforeFrom: beforeFrom, EmptyState: emptyState})
	cfgSum := sha256.Sum256(mustJSON(snap))
	p := &adapter.Plan{ID: id, Adapter: "terraform", Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Effects: effects, Reversibility: rev, Destructive: destructive, BlastRows: int64(len(changes)), Warnings: warnings,
		Facts: map[string]string{"resources": fmt.Sprint(len(changes))}, RehearsalTier: "plan",
		Diff:             fmt.Sprintf("plan %s · %s (%s) · %s\n%s", id, t.Name, t.Env, rev, diff),
		PreconditionHash: hex.EncodeToString(cfgSum[:]), AdapterData: data, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type undoHandle struct {
	Before     map[string]string `json:"before,omitempty"`
	EmptyState bool              `json:"empty_state,omitempty"`
	Changes    []Change          `json:"changes"`
}

// Apply implements adapter.Adapter: applies exactly the approved plan file.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var d planData
	if err := json.Unmarshal(p.AdapterData, &d); err != nil {
		return nil, err
	}
	pb, err := os.ReadFile(d.PlanFile)
	if err != nil {
		return nil, fmt.Errorf("plan file missing; re-plan required: %w", err)
	}
	if sum := sha256.Sum256(pb); hex.EncodeToString(sum[:]) != d.PlanSHA {
		return nil, &adapter.ConflictError{Reason: "plan file changed since approval; re-plan required"}
	}
	cur, err := snapshotConfig(t.Config["dir"])
	if err != nil {
		return nil, err
	}
	if string(mustJSON(cur)) != string(mustJSON(d.ConfigSnap)) {
		return nil, &adapter.ConflictError{Reason: "configuration files changed since plan; re-plan required"}
	}
	out, _, err := run(ctx, t, "apply", "-input=false", "-lock-timeout=30s", "-no-color", "-auto-approve", d.PlanFile)
	if err != nil {
		if strings.Contains(err.Error(), "stale") {
			return nil, &adapter.ConflictError{Reason: "state changed since plan (saved plan is stale); re-plan required"}
		}
		return nil, err
	}
	_ = os.Remove(d.PlanFile)
	_ = os.WriteFile(a.lastAppliedPath(t.Config["dir"]), mustJSON(d.ConfigSnap), 0o600)
	uh, _ := json.Marshal(undoHandle{Before: d.Before, EmptyState: d.EmptyState, Changes: d.Changes})
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: lastLines(string(out), 3), RowsAffected: int64(len(d.Changes)), UndoHandle: uh, Reversibility: p.Reversibility}, nil
}

// Verify implements adapter.Adapter: a fresh plan must show no changes.
func (a *Adapter) Verify(ctx context.Context, t adapter.Target, _ *adapter.Plan, _ *adapter.Receipt) (*adapter.Verification, error) {
	_, code, err := run(ctx, t, "plan", "-input=false", "-no-color", "-detailed-exitcode", "-lock-timeout=30s")
	if code == 2 {
		return &adapter.Verification{Passed: false, Checks: []adapter.Check{{Name: "no drift", Passed: false, Detail: "a fresh plan still shows changes"}}}, nil
	}
	if err != nil {
		return nil, err
	}
	return &adapter.Verification{Passed: true, Checks: []adapter.Check{{Name: "no drift", Passed: true, Detail: "fresh plan is empty"}}}, nil
}

// Undo implements adapter.Adapter: restore the pre-apply configuration
// files and apply them. Resources come back as new objects (compensatable).
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	if r.Reversibility == adapter.Irreversible {
		return nil, errors.New("change destroyed stateful resources; restore from a snapshot instead")
	}
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	dir := t.Config["dir"]
	if uh.EmptyState {
		// Nothing existed before: destroy what the change created. The
		// configuration files are left in place for the user to review.
		if _, _, err := run(ctx, t, "apply", "-destroy", "-input=false", "-lock-timeout=30s", "-no-color", "-auto-approve"); err != nil {
			return nil, err
		}
		_ = os.Remove(a.lastAppliedPath(dir))
		return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Output: "destroyed the resources the change created", Reversibility: adapter.Compensatable}, nil
	}
	if uh.Before == nil {
		return nil, errors.New("the previous configuration is unknown; restore the state backup instead")
	}
	cur, err := snapshotConfig(dir)
	if err != nil {
		return nil, err
	}
	for n := range cur {
		if _, ok := uh.Before[n]; !ok && n != ".terraform.lock.hcl" {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return nil, err
			}
		}
	}
	for n, body := range uh.Before {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	// For a destroy this re-creates resources from the restored configuration.
	out, _, err := run(ctx, t, "apply", "-input=false", "-lock-timeout=30s", "-no-color", "-auto-approve")
	if err != nil {
		return nil, err
	}
	_ = os.WriteFile(a.lastAppliedPath(dir), mustJSON(uh.Before), 0o600)
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: "re-applied the previous configuration: " + lastLines(string(out), 1), Reversibility: adapter.Compensatable}, nil
}
