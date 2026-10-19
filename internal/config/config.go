// Package config loads changegate.yaml: targets (connections bound to an
// environment class), policy, store location, approvals and plugin adapters.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/changegate/changegate/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// FileName is the config file Changegate looks for.
const FileName = "changegate.yaml"

// Target is a connection to one system, bound to an environment class.
// The environment class comes from the connection, never from the agent.
type Target struct {
	Adapter string `yaml:"adapter"`
	Env     string `yaml:"env"`
	// DSN is the connection string; ${VAR} references are expanded.
	DSN string `yaml:"dsn,omitempty"`
	// Sensitive columns are redacted in plans shown to agents ("table.column", "*.password").
	Sensitive []string          `yaml:"sensitive,omitempty"`
	Options   map[string]string `yaml:"options,omitempty"`
}

// Slack configures Slack approvals. Values support ${VAR}.
type Slack struct {
	// WebhookURL posts notifications only (approve with the CLI).
	WebhookURL string `yaml:"webhook_url,omitempty"`
	// BotToken + Channel post interactive Approve/Reject messages; Slack sends
	// clicks to `changegate serve` (/slack/interactions), verified with SigningSecret.
	BotToken      string `yaml:"bot_token,omitempty"`
	Channel       string `yaml:"channel,omitempty"`
	SigningSecret string `yaml:"signing_secret,omitempty"`
	// Users maps Slack user IDs to Changegate identities (user:<name>), so
	// separation of duties compares like with like.
	Users map[string]string `yaml:"users,omitempty"`
}

// Approvals configures approval channels.
type Approvals struct {
	Slack *Slack `yaml:"slack,omitempty"`
}

// Plugin is an out-of-process adapter.
type Plugin struct {
	Path string   `yaml:"path"`
	Args []string `yaml:"args,omitempty"`
}

// Config is changegate.yaml.
type Config struct {
	Targets   map[string]Target `yaml:"targets"`
	Policy    string            `yaml:"policy,omitempty"`
	Store     string            `yaml:"store,omitempty"`
	PlanTTL   string            `yaml:"plan_ttl,omitempty"`
	Approvals Approvals         `yaml:"approvals,omitempty"`
	Adapters  map[string]Plugin `yaml:"adapters,omitempty"`
	// DefaultTarget is used when a command does not name one.
	DefaultTarget string `yaml:"default_target,omitempty"`

	// Dir is the directory holding the config (or the working directory).
	Dir string `yaml:"-"`
	// Path is the config file path, empty if none was found.
	Path string `yaml:"-"`
}

// Load finds and loads the config: $CHANGEGATE_CONFIG, else changegate.yaml
// in the working directory or a parent. A missing file yields an empty config.
func Load() (*Config, error) {
	if p := os.Getenv("CHANGEGATE_CONFIG"); p != "" {
		return LoadFile(p)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return LoadFile(p)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return &Config{Targets: map[string]Target{}, Dir: wd}, nil
}

// LoadFile loads a specific config file.
func LoadFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	c.Path, c.Dir = abs, filepath.Dir(abs)
	if c.Targets == nil {
		c.Targets = map[string]Target{}
	}
	return &c, c.Validate()
}

// Validate checks targets.
func (c *Config) Validate() error {
	for name, t := range c.Targets {
		if t.Adapter == "" {
			return fmt.Errorf("target %q: adapter is required", name)
		}
		if !adapter.ValidEnv(t.Env) {
			return fmt.Errorf("target %q: env must be one of personal, shared-dev, staging, prod, prod-restricted (got %q)", name, t.Env)
		}
	}
	if c.DefaultTarget != "" {
		if _, ok := c.Targets[c.DefaultTarget]; !ok {
			return fmt.Errorf("default_target %q is not defined", c.DefaultTarget)
		}
	}
	return nil
}

// Save writes the config back to its path (or Dir/changegate.yaml).
func (c *Config) Save() error {
	path := c.Path
	if path == "" {
		path = filepath.Join(c.Dir, FileName)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	c.Path = path
	return nil
}

// StorePath returns the SQLite store location.
func (c *Config) StorePath() string {
	if p := os.Getenv("CHANGEGATE_STORE"); p != "" {
		return p
	}
	if c.Store != "" {
		if filepath.IsAbs(c.Store) {
			return c.Store
		}
		return filepath.Join(c.Dir, c.Store)
	}
	return filepath.Join(c.Dir, ".changegate", "changegate.db")
}

// StateDir is where Changegate keeps local state (checkpoints, dumps).
func (c *Config) StateDir() string { return filepath.Dir(c.StorePath()) }

// PolicyPath returns the policy file path, or "" for built-in defaults only.
func (c *Config) PolicyPath() string {
	if p := os.Getenv("CHANGEGATE_POLICY"); p != "" {
		return p
	}
	if c.Policy != "" {
		if filepath.IsAbs(c.Policy) {
			return c.Policy
		}
		return filepath.Join(c.Dir, c.Policy)
	}
	p := filepath.Join(c.Dir, "changegate.policy.yaml")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// TTL returns the plan expiry.
func (c *Config) TTL() time.Duration {
	if d, err := time.ParseDuration(c.PlanTTL); err == nil && d > 0 {
		return d
	}
	return 15 * time.Minute
}

// ErrNoTarget is returned when no target can be resolved.
var ErrNoTarget = errors.New("no target: pass --target, set default_target, or run `changegate connect`")

// Resolve returns the named target as an adapter.Target with secrets expanded.
// An empty name resolves the default (or the only) target.
func (c *Config) Resolve(name string) (adapter.Target, error) {
	if name == "" {
		name = c.DefaultTarget
	}
	if name == "" && len(c.Targets) == 1 {
		for n := range c.Targets {
			name = n
		}
	}
	if name == "" {
		return adapter.Target{}, ErrNoTarget
	}
	t, ok := c.Targets[name]
	if !ok {
		return adapter.Target{}, fmt.Errorf("unknown target %q (have: %v)", name, c.TargetNames())
	}
	cfg := map[string]string{}
	for k, v := range t.Options {
		cfg[k] = os.ExpandEnv(v)
	}
	if t.DSN != "" {
		cfg["dsn"] = os.ExpandEnv(t.DSN)
	}
	for i, s := range t.Sensitive {
		cfg[fmt.Sprintf("sensitive.%d", i)] = s
	}
	return adapter.Target{Name: name, Adapter: t.Adapter, Env: t.Env, Config: cfg}, nil
}

// TargetNames lists target names, sorted.
func (c *Config) TargetNames() []string {
	var out []string
	for n := range c.Targets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
