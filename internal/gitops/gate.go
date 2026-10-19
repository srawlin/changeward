// Package gitops implements the GitOps deployment gate: the desired
// manifests a controller is about to sync are planned against the live
// cluster, and the sync may proceed only if policy approved that exact plan.
package gitops

import (
	"context"
	"fmt"

	"github.com/srawlin/changegate/internal/engine"
	"github.com/srawlin/changegate/internal/model"
	"github.com/srawlin/changegate/internal/store"
	"github.com/srawlin/changegate/pkg/adapter"
)

// Gate plans payload (rendered manifests) for target and returns the change
// carrying the decision. An existing CI-lane change with the same manifests
// and the same live state is reused, so an approval granted after the first
// attempt lets the retried sync pass.
func Gate(ctx context.Context, eng *engine.Engine, t adapter.Target, payload string, p model.Principal, prov model.Provenance) (*model.Change, error) {
	if t.Adapter != "kubernetes" {
		return nil, fmt.Errorf("target %s is not a kubernetes target", t.Name)
	}
	ad, err := eng.Adapters.Adapter("kubernetes")
	if err != nil {
		return nil, err
	}
	plan, err := ad.Plan(ctx, t, adapter.Operation{Kind: "k8s", Payload: payload})
	if err != nil {
		return nil, err
	}
	existing, err := eng.Store.ListChanges(ctx, store.ChangeFilter{Target: t.Name})
	if err != nil {
		return nil, err
	}
	for i := len(existing) - 1; i >= 0; i-- {
		e := existing[i]
		if e.Provenance.Lane == model.LaneCI && e.Plan != nil && e.Plan.PreconditionHash == plan.PreconditionHash && e.Operation.Payload == payload {
			if e.State != model.Rejected || (e.Decision != nil && e.Decision.Decision == "deny") {
				return e, nil
			}
		}
	}
	prov.Host = "gitops-gate"
	return eng.Propose(ctx, engine.ProposeRequest{Target: t, Lane: model.LaneCI, Operation: adapter.Operation{Kind: "k8s", Payload: payload,
		Intent: "GitOps sync"}, Principal: p, Provenance: prov})
}

// Passed reports whether the gate lets the sync proceed.
func Passed(c *model.Change) bool { return c.State == model.AutoApproved || c.State == model.Approved }
