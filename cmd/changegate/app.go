package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"sync"

	"github.com/srawlin/changegate/adapters/kubernetes"
	"github.com/srawlin/changegate/adapters/postgres"
	"github.com/srawlin/changegate/adapters/terraform"
	"github.com/srawlin/changegate/adapters/toykv"
	"github.com/srawlin/changegate/internal/config"
	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/policy"
	"github.com/srawlin/changegate/internal/signals"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

// app lazily wires config, store, policy, adapters and the engine.
type app struct {
	cfg     *config.Config
	st      store.Store
	eng     *engine.Engine
	reg     *registry
	closers []func()
}

func newApp() (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &app{cfg: cfg}, nil
}

func (a *app) close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

func (a *app) store() (store.Store, error) {
	if a.st != nil {
		return a.st, nil
	}
	st, err := store.OpenSQLite(a.cfg.StorePath())
	if err != nil {
		return nil, err
	}
	a.st = st
	a.closers = append(a.closers, func() { _ = st.Close() })
	return st, nil
}

func (a *app) policy() (*policy.Engine, error) {
	var f *policy.File
	if p := a.cfg.PolicyPath(); p != "" {
		var err error
		if f, err = policy.LoadFile(p); err != nil {
			return nil, err
		}
	}
	return policy.New(f)
}

func (a *app) engine() (*engine.Engine, error) {
	if a.eng != nil {
		return a.eng, nil
	}
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	pol, err := a.policy()
	if err != nil {
		return nil, err
	}
	a.eng = &engine.Engine{Store: st, Adapters: a.registry(), Policy: pol, PlanTTL: a.cfg.TTL()}
	if n := a.notifier(); n != nil {
		a.eng.Notifier = n
	}
	if j := a.cfg.Signals.Jev; j != nil && j.Enabled {
		key := os.ExpandEnv(j.APIKey)
		if key == "" {
			key = os.Getenv("TYPESAFE_API_KEY")
		}
		a.eng.Signals = &signals.Jev{APIKey: key, Model: j.Model}
		a.eng.SignalThresholds = signals.DefaultThresholds
		if j.MismatchThreshold > 0 {
			a.eng.SignalThresholds.IntentMismatch = j.MismatchThreshold
		}
		if j.InjectionThreshold > 0 {
			a.eng.SignalThresholds.Injection = j.InjectionThreshold
		}
		if len(j.Envs) > 0 {
			a.eng.SignalEnvs = map[string]bool{}
			for _, e := range j.Envs {
				a.eng.SignalEnvs[e] = true
			}
		}
	}
	return a.eng, nil
}

func (a *app) registry() *registry {
	if a.reg == nil {
		a.reg = &registry{cfg: a.cfg, builtin: map[string]adapter.Adapter{
			"postgres":   postgres.New(postgres.Options{StateDir: a.cfg.StateDir()}),
			"toykv":      toykv.New(),
			"terraform":  terraform.New(a.cfg.StateDir()),
			"kubernetes": kubernetes.New(),
		}, plugins: map[string]*adapter.Plugin{}}
		a.closers = append(a.closers, a.reg.close)
	}
	return a.reg
}

// registry resolves built-in adapters and launches configured plugins on demand.
type registry struct {
	mu      sync.Mutex
	cfg     *config.Config
	builtin map[string]adapter.Adapter
	plugins map[string]*adapter.Plugin
}

func (r *registry) Adapter(name string) (adapter.Adapter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.cfg.Adapters[name]; ok {
		if pl, ok := r.plugins[name]; ok {
			return pl, nil
		}
		pl, err := adapter.Open(p.Path, p.Args...)
		if err != nil {
			return nil, fmt.Errorf("start adapter plugin %s: %w", name, err)
		}
		r.plugins[name] = pl
		return pl, nil
	}
	if a, ok := r.builtin[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}

func (r *registry) close() {
	for _, p := range r.plugins {
		p.Close()
	}
}

// humanIdentity is the local human, from $CHANGEGATE_USER or the OS user.
func humanIdentity() string {
	if u := os.Getenv("CHANGEGATE_USER"); u != "" {
		return "user:" + u
	}
	if u, err := user.Current(); err == nil {
		return "user:" + u.Username
	}
	return "user:unknown"
}

// principal returns the principal for CLI-originated changes. When run from
// an agent hook or MCP server, CHANGEGATE_AGENT names the agent.
func principal() model.Principal {
	if ag := os.Getenv("CHANGEGATE_AGENT"); ag != "" {
		return model.Principal{Kind: "agent", Agent: "agent:" + ag, Human: humanIdentity()}
	}
	return model.Principal{Kind: "human", Human: humanIdentity()}
}

func bg() context.Context { return context.Background() }
