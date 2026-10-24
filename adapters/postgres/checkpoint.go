package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/srawlin/changeward/pkg/adapter"
)

// Checkpoint strategies.
const (
	StrategyTemplate = "template" // CREATE DATABASE ... TEMPLATE (needs no other connections)
	StrategyDump     = "dump"     // pg_dump / pg_restore (client must match server major version)
	StrategyNeon     = "neon"     // Neon branch via API
)

type checkpointData struct {
	Strategy string   `json:"strategy"`
	Database string   `json:"database,omitempty"` // template strategy: the copy's name
	File     string   `json:"file,omitempty"`     // dump strategy
	Neon     *neonRef `json:"neon,omitempty"`
}

// WithDatabase returns dsn pointing at database db. Both URL and key/value
// DSNs are supported.
func WithDatabase(dsn, db string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + db
		return u.String(), nil
	}
	re := regexp.MustCompile(`(^|\s)dbname=\S+`)
	if re.MatchString(dsn) {
		return re.ReplaceAllString(dsn, "${1}dbname="+db), nil
	}
	return strings.TrimSpace(dsn + " dbname=" + db), nil
}

// DatabaseName returns the database a DSN connects to.
func DatabaseName(ctx context.Context, dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return cfg.Database, nil
}

func (a *Adapter) adminConn(ctx context.Context, t adapter.Target) (*pgx.Conn, string, error) {
	dsn := t.Config["dsn"]
	db, err := DatabaseName(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	maint := t.Config["maintenance_db"]
	if maint == "" {
		maint = "postgres"
	}
	adminDSN, err := WithDatabase(dsn, maint)
	if err != nil {
		return nil, "", err
	}
	c, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, "", fmt.Errorf("connect to maintenance database %q: %w", maint, err)
	}
	return c, db, nil
}

// drainWait is how long otherConnections waits for sessions to go away.
// Backends of a pool we just closed leave pg_stat_activity asynchronously.
const drainWait = 2 * time.Second

func otherConnections(ctx context.Context, c *pgx.Conn, db string) (int, error) {
	deadline := time.Now().Add(drainWait)
	for {
		var n int
		err := c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, db).Scan(&n)
		if err != nil || n == 0 || time.Now().After(deadline) {
			return n, err
		}
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// allowForce reports whether Changeward may terminate other sessions on the
// target. Only ever in personal environments, and only when configured.
func allowForce(t adapter.Target) bool {
	return t.Env == adapter.EnvPersonal && t.Config["allow_terminate"] == "true"
}

func copyName(base, suffix string) string {
	n := base + "_" + suffix
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// Checkpoint implements adapter.Adapter.
func (a *Adapter) Checkpoint(ctx context.Context, t adapter.Target, label string) (*adapter.CheckpointRef, error) {
	id := adapter.NewID("cp")
	ref := &adapter.CheckpointRef{ID: id, Adapter: "postgres", Target: t.Name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	strategy := t.Config["checkpoint_strategy"]
	if strategy == "" {
		strategy = StrategyTemplate
		if t.Config["neon_project_id"] != "" {
			strategy = StrategyNeon
		}
	}
	var data checkpointData
	var err error
	switch strategy {
	case StrategyTemplate:
		data, err = a.templateCheckpoint(ctx, t, id)
		if err != nil && t.Config["pg_dump"] != "" {
			data, err = a.dumpCheckpoint(ctx, t, id)
		}
	case StrategyDump:
		data, err = a.dumpCheckpoint(ctx, t, id)
	case StrategyNeon:
		data, err = a.neonCheckpoint(ctx, t, label)
	default:
		err = fmt.Errorf("unknown checkpoint_strategy %q", strategy)
	}
	if err != nil {
		return nil, err
	}
	ref.Data, _ = json.Marshal(data)
	return ref, nil
}

func (a *Adapter) templateCheckpoint(ctx context.Context, t adapter.Target, id string) (checkpointData, error) {
	a.dropPool(t.Config["dsn"])
	c, db, err := a.adminConn(ctx, t)
	if err != nil {
		return checkpointData{}, err
	}
	defer func() { _ = c.Close(ctx) }()
	if n, err := otherConnections(ctx, c, db); err != nil {
		return checkpointData{}, err
	} else if n > 0 {
		if !allowForce(t) {
			return checkpointData{}, fmt.Errorf("template checkpoint needs exclusive access to %q but %d other sessions are connected "+
				"(set options.allow_terminate: \"true\" on a personal target, use checkpoint_strategy: dump, or use a Neon branch)", db, n)
		}
		if _, err := c.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, db); err != nil {
			return checkpointData{}, err
		}
	}
	cp := copyName("cw_"+strings.TrimPrefix(id, "cp_"), db)
	if _, err := c.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", QuoteIdent(cp), QuoteIdent(db))); err != nil {
		return checkpointData{}, fmt.Errorf("create checkpoint database: %w", err)
	}
	return checkpointData{Strategy: StrategyTemplate, Database: cp}, nil
}

func (a *Adapter) dumpCheckpoint(ctx context.Context, t adapter.Target, id string) (checkpointData, error) {
	bin := t.Config["pg_dump"]
	if bin == "" {
		bin = "pg_dump"
	}
	dir := filepath.Join(a.opts.StateDir, "checkpoints")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return checkpointData{}, err
	}
	file := filepath.Join(dir, id+".dump")
	cmd := exec.CommandContext(ctx, bin, "--format=custom", "--file="+file, "--dbname="+t.Config["dsn"])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return checkpointData{}, fmt.Errorf("pg_dump: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return checkpointData{Strategy: StrategyDump, File: file}, nil
}

// Restore implements adapter.Adapter. The target is returned to exactly the
// checkpointed state; everything after the checkpoint is discarded.
func (a *Adapter) Restore(ctx context.Context, t adapter.Target, cp *adapter.CheckpointRef) (*adapter.Receipt, error) {
	var data checkpointData
	if err := json.Unmarshal(cp.Data, &data); err != nil {
		return nil, err
	}
	switch data.Strategy {
	case StrategyTemplate:
		a.dropPool(t.Config["dsn"])
		c, db, err := a.adminConn(ctx, t)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close(ctx) }()
		if n, err := otherConnections(ctx, c, db); err != nil {
			return nil, err
		} else if n > 0 && !allowForce(t) {
			return nil, fmt.Errorf("restore needs exclusive access to %q but %d other sessions are connected", db, n)
		}
		if _, err := c.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, db); err != nil {
			return nil, err
		}
		// Keep the current state as a safety copy until the restore succeeds.
		aside := copyName("cw_pre_restore_"+strings.TrimPrefix(cp.ID, "cp_"), db)
		if _, err := c.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", QuoteIdent(db), QuoteIdent(aside))); err != nil {
			return nil, fmt.Errorf("set current database aside: %w", err)
		}
		if _, err := c.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", QuoteIdent(db), QuoteIdent(data.Database))); err != nil {
			_, _ = c.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", QuoteIdent(aside), QuoteIdent(db)))
			return nil, fmt.Errorf("restore from checkpoint: %w", err)
		}
		if _, err := c.Exec(ctx, fmt.Sprintf("DROP DATABASE %s", QuoteIdent(aside))); err != nil {
			return nil, fmt.Errorf("restored, but could not drop the pre-restore copy %s: %w", aside, err)
		}
	case StrategyDump:
		bin := t.Config["pg_restore"]
		if bin == "" {
			bin = "pg_restore"
		}
		pool, err := a.pool(ctx, t)
		if err != nil {
			return nil, err
		}
		// Remove objects created after the checkpoint, then restore.
		if _, err := pool.Exec(ctx, `DO $$ DECLARE s text; BEGIN
			FOR s IN SELECT nspname FROM pg_namespace WHERE nspname NOT IN ('pg_catalog','information_schema','pg_toast')
			  AND nspname NOT LIKE 'pg_temp_%' AND nspname NOT LIKE 'pg_toast_temp_%' LOOP
			  EXECUTE format('DROP SCHEMA %I CASCADE', s);
			END LOOP; END $$`); err != nil {
			return nil, err
		}
		a.dropPool(t.Config["dsn"])
		cmd := exec.CommandContext(ctx, bin, "--single-transaction", "--no-owner", "--dbname="+t.Config["dsn"], data.File)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("pg_restore: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
	case StrategyNeon:
		kept, err := a.neonRestore(ctx, t, data.Neon)
		if err != nil {
			return nil, err
		}
		out := "restored checkpoint " + cp.ID
		if kept != "" {
			out += "; Neon keeps the pre-restore state as branch " + kept + " (now an ancestor of the target branch)"
		}
		return &adapter.Receipt{ID: adapter.NewID("r"), AppliedAt: time.Now().UTC().Format(time.RFC3339Nano), Reversibility: adapter.CheckpointOnly, Output: out}, nil
	default:
		return nil, fmt.Errorf("unknown checkpoint strategy %q", data.Strategy)
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), AppliedAt: time.Now().UTC().Format(time.RFC3339Nano), Reversibility: adapter.CheckpointOnly,
		Output: "restored checkpoint " + cp.ID}, nil
}

// DeleteCheckpoint removes a checkpoint's storage.
func (a *Adapter) DeleteCheckpoint(ctx context.Context, t adapter.Target, cp *adapter.CheckpointRef) error {
	var data checkpointData
	if err := json.Unmarshal(cp.Data, &data); err != nil {
		return err
	}
	switch data.Strategy {
	case StrategyTemplate:
		c, _, err := a.adminConn(ctx, t)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(ctx) }()
		_, err = c.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", QuoteIdent(data.Database)))
		return err
	case StrategyDump:
		return os.Remove(data.File)
	case StrategyNeon:
		return a.neonDeleteBranch(ctx, t, data.Neon)
	}
	return nil
}

// ProvisionCopy implements adapter.Adapter: a branch of the target usable as
// a private working target or rehearsal copy. Strategy follows the target's
// checkpoint_strategy (template by default, Neon when configured).
func (a *Adapter) ProvisionCopy(ctx context.Context, t adapter.Target, name string) (*adapter.CopyRef, error) {
	if t.Config["neon_project_id"] != "" {
		return a.neonBranch(ctx, t, name)
	}
	c, db, err := a.adminConn(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(ctx) }()
	if name == "" {
		name = copyName(db, "cw_"+strings.TrimPrefix(adapter.NewID("b"), "b_"))
	}
	src := t.Config["copy_from"] // e.g. a replica-side template or golden image
	if src == "" {
		src = db
		a.dropPool(t.Config["dsn"])
	}
	if n, err := otherConnections(ctx, c, src); err != nil {
		return nil, err
	} else if n > 0 {
		if !allowForce(t) {
			return nil, fmt.Errorf("creating a branch of %q with CREATE DATABASE ... TEMPLATE needs no other connections, but %d are open; "+
				"close them, set options.allow_terminate: \"true\" on this personal target, or configure a Neon project", src, n)
		}
		if _, err := c.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, src); err != nil {
			return nil, err
		}
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", QuoteIdent(name), QuoteIdent(src))); err != nil {
		return nil, fmt.Errorf("create branch database: %w", err)
	}
	dsn, err := WithDatabase(t.Config["dsn"], name)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{}
	for k, v := range t.Config {
		cfg[k] = v
	}
	cfg["dsn"] = dsn
	cfg["copy_from"] = ""
	data, _ := json.Marshal(checkpointData{Strategy: StrategyTemplate, Database: name})
	return &adapter.CopyRef{Target: adapter.Target{Name: t.Name + "@" + name, Adapter: "postgres", Env: adapter.EnvPersonal, Config: cfg}, Data: data}, nil
}

// ReleaseCopy implements adapter.Adapter.
func (a *Adapter) ReleaseCopy(ctx context.Context, c *adapter.CopyRef) error {
	var data checkpointData
	if err := json.Unmarshal(c.Data, &data); err != nil {
		return err
	}
	if data.Strategy == StrategyNeon {
		return a.neonDeleteBranch(ctx, c.Target, data.Neon)
	}
	if data.Database == "" {
		return errors.New("copy has no database")
	}
	a.dropPool(c.Target.Config["dsn"])
	conn, _, err := a.adminConn(ctx, c.Target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", QuoteIdent(data.Database)))
	return err
}
