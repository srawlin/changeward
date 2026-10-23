package adapter

import (
	pb "github.com/srawlin/changeward/gen/changeward/v1"
)

var revToPB = map[Reversibility]pb.Reversibility{
	Reversible:     pb.Reversibility_REVERSIBLE,
	Compensatable:  pb.Reversibility_COMPENSATABLE,
	CheckpointOnly: pb.Reversibility_CHECKPOINT_ONLY,
	Irreversible:   pb.Reversibility_IRREVERSIBLE,
}

var accessToPB = map[Access]pb.Access{
	Read:        pb.Access_READ,
	Write:       pb.Access_WRITE,
	Destructive: pb.Access_DESTRUCTIVE,
}

func revFromPB(r pb.Reversibility) Reversibility {
	for k, v := range revToPB {
		if v == r {
			return k
		}
	}
	return ""
}

func accessFromPB(a pb.Access) Access {
	for k, v := range accessToPB {
		if v == a {
			return k
		}
	}
	return ""
}

// TargetToPB converts a Target to its wire form.
func TargetToPB(t Target) *pb.Target {
	return &pb.Target{Name: t.Name, Adapter: t.Adapter, Env: t.Env, Config: t.Config}
}

// TargetFromPB converts a wire Target.
func TargetFromPB(t *pb.Target) Target {
	if t == nil {
		return Target{}
	}
	return Target{Name: t.Name, Adapter: t.Adapter, Env: t.Env, Config: t.Config}
}

func opToPB(o Operation) *pb.Operation {
	return &pb.Operation{Kind: o.Kind, Payload: o.Payload, Params: o.Params, Intent: o.Intent}
}

func opFromPB(o *pb.Operation) Operation {
	if o == nil {
		return Operation{}
	}
	return Operation{Kind: o.Kind, Payload: o.Payload, Params: o.Params, Intent: o.Intent}
}

func classToPB(c Classification) *pb.Classification {
	out := &pb.Classification{Access: accessToPB[c.Access], Objects: c.Objects, Flags: c.Flags}
	for _, s := range c.Statements {
		out.Statements = append(out.Statements, &pb.StatementClass{Text: s.Text, Kind: s.Kind, Access: accessToPB[s.Access], Objects: s.Objects, Flags: s.Flags})
	}
	return out
}

func classFromPB(c *pb.Classification) Classification {
	if c == nil {
		return Classification{}
	}
	out := Classification{Access: accessFromPB(c.Access), Objects: c.Objects, Flags: c.Flags}
	for _, s := range c.Statements {
		out.Statements = append(out.Statements, StatementClass{Text: s.Text, Kind: s.Kind, Access: accessFromPB(s.Access), Objects: s.Objects, Flags: s.Flags})
	}
	return out
}

func effectsToPB(es []Effect) []*pb.Effect {
	var out []*pb.Effect
	for _, e := range es {
		out = append(out, &pb.Effect{Kind: e.Kind, Object: e.Object, Description: e.Description, Rows: e.Rows})
	}
	return out
}

func effectsFromPB(es []*pb.Effect) []Effect {
	var out []Effect
	for _, e := range es {
		out = append(out, Effect{Kind: e.Kind, Object: e.Object, Description: e.Description, Rows: e.Rows})
	}
	return out
}

func checksToPB(cs []Check) []*pb.Check {
	var out []*pb.Check
	for _, c := range cs {
		out = append(out, &pb.Check{Name: c.Name, Passed: c.Passed, Detail: c.Detail})
	}
	return out
}

func checksFromPB(cs []*pb.Check) []Check {
	var out []Check
	for _, c := range cs {
		out = append(out, Check{Name: c.Name, Passed: c.Passed, Detail: c.Detail})
	}
	return out
}

// PlanToPB converts a Plan to its wire form.
func PlanToPB(p *Plan) *pb.Plan {
	if p == nil {
		return nil
	}
	return &pb.Plan{
		Id: p.ID, Hash: p.Hash, Adapter: p.Adapter, Target: p.Target, Env: p.Env,
		Operation: opToPB(p.Operation), Classification: classToPB(p.Classification),
		Effects: effectsToPB(p.Effects), Reversibility: revToPB[p.Reversibility],
		Destructive: p.Destructive, BlastRows: p.BlastRows, Warnings: p.Warnings,
		Facts: p.Facts, Diff: p.Diff, PreconditionHash: p.PreconditionHash,
		RehearsalTier: p.RehearsalTier, AdapterData: p.AdapterData,
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
}

// PlanFromPB converts a wire Plan.
func PlanFromPB(p *pb.Plan) *Plan {
	if p == nil {
		return nil
	}
	return &Plan{
		ID: p.Id, Hash: p.Hash, Adapter: p.Adapter, Target: p.Target, Env: p.Env,
		Operation: opFromPB(p.Operation), Classification: classFromPB(p.Classification),
		Effects: effectsFromPB(p.Effects), Reversibility: revFromPB(p.Reversibility),
		Destructive: p.Destructive, BlastRows: p.BlastRows, Warnings: p.Warnings,
		Facts: p.Facts, Diff: p.Diff, PreconditionHash: p.PreconditionHash,
		RehearsalTier: p.RehearsalTier, AdapterData: p.AdapterData,
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
}

func receiptToPB(r *Receipt) *pb.Receipt {
	if r == nil {
		return nil
	}
	return &pb.Receipt{Id: r.ID, PlanHash: r.PlanHash, AppliedAt: r.AppliedAt, Output: r.Output,
		RowsAffected: r.RowsAffected, UndoHandle: r.UndoHandle, Reversibility: revToPB[r.Reversibility], Warnings: r.Warnings}
}

func receiptFromPB(r *pb.Receipt) *Receipt {
	if r == nil {
		return nil
	}
	return &Receipt{ID: r.Id, PlanHash: r.PlanHash, AppliedAt: r.AppliedAt, Output: r.Output,
		RowsAffected: r.RowsAffected, UndoHandle: r.UndoHandle, Reversibility: revFromPB(r.Reversibility), Warnings: r.Warnings}
}

func checkpointToPB(c *CheckpointRef) *pb.CheckpointRef {
	if c == nil {
		return nil
	}
	return &pb.CheckpointRef{Id: c.ID, Adapter: c.Adapter, Target: c.Target, Data: c.Data, CreatedAt: c.CreatedAt}
}

func checkpointFromPB(c *pb.CheckpointRef) *CheckpointRef {
	if c == nil {
		return nil
	}
	return &CheckpointRef{ID: c.Id, Adapter: c.Adapter, Target: c.Target, Data: c.Data, CreatedAt: c.CreatedAt}
}

func copyToPB(c *CopyRef) *pb.CopyRef {
	if c == nil {
		return nil
	}
	return &pb.CopyRef{Target: TargetToPB(c.Target), Data: c.Data}
}

func copyFromPB(c *pb.CopyRef) *CopyRef {
	if c == nil {
		return nil
	}
	return &CopyRef{Target: TargetFromPB(c.Target), Data: c.Data}
}
