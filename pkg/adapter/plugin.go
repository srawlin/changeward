package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"

	pb "github.com/changegate/changegate/gen/changegate/v1"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Handshake is shared by the executor and every adapter plugin binary.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "CHANGEGATE_ADAPTER_PLUGIN",
	MagicCookieValue: "8c1b2f0e-changegate-adapter-v1",
}

const pluginName = "adapter"

// Serve runs impl as an out-of-process adapter plugin. Call it from main()
// of an adapter binary; it does not return.
func Serve(impl Adapter) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         plugin.PluginSet{pluginName: &grpcPlugin{impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// Plugin is a running adapter plugin process.
type Plugin struct {
	Adapter
	client *plugin.Client
}

// Close kills the plugin process.
func (p *Plugin) Close() { p.client.Kill() }

// Open launches the adapter plugin binary at path with args and returns a
// client implementing Adapter.
func Open(path string, args ...string) (*Plugin, error) {
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          plugin.PluginSet{pluginName: &grpcPlugin{}},
		Cmd:              exec.Command(path, args...),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.New(&hclog.LoggerOptions{Level: hclog.Warn, Name: "adapter"}),
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, err
	}
	raw, err := rpc.Dispense(pluginName)
	if err != nil {
		client.Kill()
		return nil, err
	}
	return &Plugin{Adapter: raw.(Adapter), client: client}, nil
}

type grpcPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	impl Adapter
}

func (p *grpcPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAdapterServer(s, &grpcServer{impl: p.impl})
	return nil
}

func (p *grpcPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &grpcClient{c: pb.NewAdapterClient(c)}, nil
}

const conflictPrefix = "changegate-conflict:"

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnsupported) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		b, _ := json.Marshal(ce)
		return status.Error(codes.FailedPrecondition, conflictPrefix+string(b))
	}
	return status.Error(codes.Unknown, err.Error())
}

func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Unimplemented:
		return ErrUnsupported
	case codes.FailedPrecondition:
		if strings.HasPrefix(st.Message(), conflictPrefix) {
			var ce ConflictError
			if json.Unmarshal([]byte(strings.TrimPrefix(st.Message(), conflictPrefix)), &ce) == nil {
				return &ce
			}
		}
	}
	return errors.New(st.Message())
}

type grpcServer struct {
	pb.UnimplementedAdapterServer
	impl Adapter
}

func (s *grpcServer) Describe(ctx context.Context, _ *pb.DescribeRequest) (*pb.AdapterInfo, error) {
	i, err := s.impl.Describe(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.AdapterInfo{Name: i.Name, Version: i.Version, Kinds: i.Kinds, Capabilities: i.Capabilities}, nil
}

func (s *grpcServer) Classify(ctx context.Context, r *pb.ClassifyRequest) (*pb.Classification, error) {
	c, err := s.impl.Classify(ctx, TargetFromPB(r.Target), opFromPB(r.Operation))
	if err != nil {
		return nil, toStatus(err)
	}
	return classToPB(c), nil
}

func (s *grpcServer) Plan(ctx context.Context, r *pb.PlanRequest) (*pb.Plan, error) {
	p, err := s.impl.Plan(ctx, TargetFromPB(r.Target), opFromPB(r.Operation))
	if err != nil {
		return nil, toStatus(err)
	}
	return PlanToPB(p), nil
}

func (s *grpcServer) Rehearse(ctx context.Context, r *pb.RehearseRequest) (*pb.RehearsalResult, error) {
	res, err := s.impl.Rehearse(ctx, TargetFromPB(r.Copy), opFromPB(r.Operation))
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.RehearsalResult{Passed: res.Passed, Tier: res.Tier, Checks: checksToPB(res.Checks), Effects: effectsToPB(res.Effects), Report: res.Report}, nil
}

func (s *grpcServer) Apply(ctx context.Context, r *pb.ApplyRequest) (*pb.Receipt, error) {
	rc, err := s.impl.Apply(ctx, TargetFromPB(r.Target), PlanFromPB(r.Plan))
	if err != nil {
		return nil, toStatus(err)
	}
	return receiptToPB(rc), nil
}

func (s *grpcServer) Verify(ctx context.Context, r *pb.VerifyRequest) (*pb.Verification, error) {
	v, err := s.impl.Verify(ctx, TargetFromPB(r.Target), PlanFromPB(r.Plan), receiptFromPB(r.Receipt))
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.Verification{Passed: v.Passed, Checks: checksToPB(v.Checks)}, nil
}

func (s *grpcServer) Undo(ctx context.Context, r *pb.UndoRequest) (*pb.Receipt, error) {
	rc, err := s.impl.Undo(ctx, TargetFromPB(r.Target), PlanFromPB(r.Plan), receiptFromPB(r.Receipt))
	if err != nil {
		return nil, toStatus(err)
	}
	return receiptToPB(rc), nil
}

func (s *grpcServer) Checkpoint(ctx context.Context, r *pb.CheckpointRequest) (*pb.CheckpointRef, error) {
	c, err := s.impl.Checkpoint(ctx, TargetFromPB(r.Target), r.Label)
	if err != nil {
		return nil, toStatus(err)
	}
	return checkpointToPB(c), nil
}

func (s *grpcServer) Restore(ctx context.Context, r *pb.RestoreRequest) (*pb.Receipt, error) {
	rc, err := s.impl.Restore(ctx, TargetFromPB(r.Target), checkpointFromPB(r.Checkpoint))
	if err != nil {
		return nil, toStatus(err)
	}
	return receiptToPB(rc), nil
}

func (s *grpcServer) ProvisionCopy(ctx context.Context, r *pb.CopyRequest) (*pb.CopyRef, error) {
	c, err := s.impl.ProvisionCopy(ctx, TargetFromPB(r.Target), r.Name)
	if err != nil {
		return nil, toStatus(err)
	}
	return copyToPB(c), nil
}

func (s *grpcServer) ReleaseCopy(ctx context.Context, r *pb.ReleaseCopyRequest) (*pb.ReleaseCopyResponse, error) {
	if err := s.impl.ReleaseCopy(ctx, copyFromPB(r.Copy)); err != nil {
		return nil, toStatus(err)
	}
	return &pb.ReleaseCopyResponse{}, nil
}

type grpcClient struct{ c pb.AdapterClient }

func (g *grpcClient) Describe(ctx context.Context) (Info, error) {
	i, err := g.c.Describe(ctx, &pb.DescribeRequest{})
	if err != nil {
		return Info{}, fromStatus(err)
	}
	return Info{Name: i.Name, Version: i.Version, Kinds: i.Kinds, Capabilities: i.Capabilities}, nil
}

func (g *grpcClient) Classify(ctx context.Context, t Target, op Operation) (Classification, error) {
	c, err := g.c.Classify(ctx, &pb.ClassifyRequest{Target: TargetToPB(t), Operation: opToPB(op)})
	if err != nil {
		return Classification{}, fromStatus(err)
	}
	return classFromPB(c), nil
}

func (g *grpcClient) Plan(ctx context.Context, t Target, op Operation) (*Plan, error) {
	p, err := g.c.Plan(ctx, &pb.PlanRequest{Target: TargetToPB(t), Operation: opToPB(op)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return PlanFromPB(p), nil
}

func (g *grpcClient) Rehearse(ctx context.Context, copy Target, op Operation) (*RehearsalResult, error) {
	r, err := g.c.Rehearse(ctx, &pb.RehearseRequest{Copy: TargetToPB(copy), Operation: opToPB(op)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return &RehearsalResult{Passed: r.Passed, Tier: r.Tier, Checks: checksFromPB(r.Checks), Effects: effectsFromPB(r.Effects), Report: r.Report}, nil
}

func (g *grpcClient) Apply(ctx context.Context, t Target, p *Plan) (*Receipt, error) {
	r, err := g.c.Apply(ctx, &pb.ApplyRequest{Target: TargetToPB(t), Plan: PlanToPB(p)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return receiptFromPB(r), nil
}

func (g *grpcClient) Verify(ctx context.Context, t Target, p *Plan, rc *Receipt) (*Verification, error) {
	v, err := g.c.Verify(ctx, &pb.VerifyRequest{Target: TargetToPB(t), Plan: PlanToPB(p), Receipt: receiptToPB(rc)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return &Verification{Passed: v.Passed, Checks: checksFromPB(v.Checks)}, nil
}

func (g *grpcClient) Undo(ctx context.Context, t Target, p *Plan, rc *Receipt) (*Receipt, error) {
	r, err := g.c.Undo(ctx, &pb.UndoRequest{Target: TargetToPB(t), Plan: PlanToPB(p), Receipt: receiptToPB(rc)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return receiptFromPB(r), nil
}

func (g *grpcClient) Checkpoint(ctx context.Context, t Target, label string) (*CheckpointRef, error) {
	c, err := g.c.Checkpoint(ctx, &pb.CheckpointRequest{Target: TargetToPB(t), Label: label})
	if err != nil {
		return nil, fromStatus(err)
	}
	return checkpointFromPB(c), nil
}

func (g *grpcClient) Restore(ctx context.Context, t Target, cp *CheckpointRef) (*Receipt, error) {
	r, err := g.c.Restore(ctx, &pb.RestoreRequest{Target: TargetToPB(t), Checkpoint: checkpointToPB(cp)})
	if err != nil {
		return nil, fromStatus(err)
	}
	return receiptFromPB(r), nil
}

func (g *grpcClient) ProvisionCopy(ctx context.Context, t Target, name string) (*CopyRef, error) {
	c, err := g.c.ProvisionCopy(ctx, &pb.CopyRequest{Target: TargetToPB(t), Name: name})
	if err != nil {
		return nil, fromStatus(err)
	}
	return copyFromPB(c), nil
}

func (g *grpcClient) ReleaseCopy(ctx context.Context, c *CopyRef) error {
	_, err := g.c.ReleaseCopy(ctx, &pb.ReleaseCopyRequest{Copy: copyToPB(c)})
	return fromStatus(err)
}
