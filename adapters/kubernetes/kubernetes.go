// Package kubernetes is the Changeward adapter for Kubernetes (§7.4).
//
// Operation kind "k8s". Payload is either YAML manifests (applied with
// server-side apply), or a command line:
//
//	delete <kind>/<name> [<kind>/<name> ...] [-n <namespace>]
//	scale <kind>/<name> --replicas=<n> [-n <namespace>]
//
// Plan runs a server-side dry run (dryRun=All) and diffs the result with the
// live objects, with managedFields, status and other server-set fields
// stripped. Previous live objects are the undo handle. Apply refuses if any
// object's resourceVersion changed since the plan; undo refuses if an object
// changed since apply. Deleting a PersistentVolumeClaim, PersistentVolume,
// Namespace or CustomResourceDefinition is irreversible (data or every
// contained object is lost) unless a snapshot is taken first. Objects owned
// by a GitOps controller (Argo CD, Flux) are flagged: a direct change would
// be reverted on the next sync, so it belongs in Git (Lane A).
//
// Target config: context, kubeconfig, namespace (default "default"),
// kubectl (binary path).
package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/srawlin/changeward/pkg/adapter"
	"gopkg.in/yaml.v3"
)

// Adapter implements adapter.Adapter for Kubernetes.
type Adapter struct{ adapter.Unimplemented }

// New returns a Kubernetes adapter.
func New() *Adapter { return &Adapter{} }

var _ adapter.Adapter = (*Adapter)(nil)

// IrreversibleDeleteKinds lose data or contained objects when deleted.
var IrreversibleDeleteKinds = map[string]bool{"PersistentVolumeClaim": true, "PersistentVolume": true, "Namespace": true, "CustomResourceDefinition": true}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.Info, error) {
	return adapter.Info{Name: "kubernetes", Version: "0.1.0", Kinds: []string{"k8s"},
		Capabilities: []string{adapter.CapClassify, adapter.CapPlan, adapter.CapApply, adapter.CapVerify, adapter.CapUndo}}, nil
}

func kubectl(ctx context.Context, t adapter.Target, stdin []byte, args ...string) ([]byte, error) {
	bin := t.Config["kubectl"]
	if bin == "" {
		bin = "kubectl"
	}
	var full []string
	if c := t.Config["context"]; c != "" {
		full = append(full, "--context="+c)
	}
	if k := t.Config["kubeconfig"]; k != "" {
		full = append(full, "--kubeconfig="+k)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("kubectl %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// Obj is a Kubernetes object.
type Obj = map[string]any

// Ref identifies an object.
type Ref struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

func (r Ref) String() string {
	if r.Namespace != "" {
		return r.Namespace + "/" + r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Name
}

func refOf(o Obj, defaultNS string) Ref {
	md, _ := o["metadata"].(map[string]any)
	r := Ref{APIVersion: str(o["apiVersion"]), Kind: str(o["kind"]), Name: str(md["name"]), Namespace: str(md["namespace"])}
	if r.Namespace == "" && namespaced(r.Kind) {
		r.Namespace = defaultNS
	}
	return r
}

func str(v any) string { s, _ := v.(string); return s }

var clusterScoped = map[string]bool{"Namespace": true, "Node": true, "PersistentVolume": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"CustomResourceDefinition": true, "StorageClass": true, "PriorityClass": true, "ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration": true, "APIService": true, "IngressClass": true, "RuntimeClass": true, "VolumeSnapshotClass": true}

func namespaced(kind string) bool { return !clusterScoped[kind] }

// parseManifests splits multi-document YAML into objects (flattening Lists).
func parseManifests(src string) ([]Obj, error) {
	dec := yaml.NewDecoder(strings.NewReader(src))
	var out []Obj
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if doc == nil {
			continue
		}
		if str(doc["kind"]) == "List" {
			items, _ := doc["items"].([]any)
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
			continue
		}
		if str(doc["kind"]) == "" || refOf(doc, "").Name == "" {
			return nil, errors.New("manifest: every object needs kind and metadata.name")
		}
		out = append(out, doc)
	}
	if len(out) == 0 {
		return nil, errors.New("no Kubernetes objects in the operation")
	}
	return out, nil
}

// command is a parsed delete/scale command.
type command struct {
	Verb     string
	Refs     []Ref
	Replicas int
}

func parseCommand(p, defaultNS string) (*command, bool, error) {
	f := strings.Fields(p)
	if len(f) == 0 || (f[0] != "delete" && f[0] != "scale") {
		return nil, false, nil
	}
	c := &command{Verb: f[0], Replicas: -1}
	ns := defaultNS
	var names []string
	for i := 1; i < len(f); i++ {
		switch {
		case f[i] == "-n" || f[i] == "--namespace":
			if i+1 < len(f) {
				ns = f[i+1]
				i++
			}
		case strings.HasPrefix(f[i], "--namespace="):
			ns = strings.TrimPrefix(f[i], "--namespace=")
		case strings.HasPrefix(f[i], "--replicas="):
			_, _ = fmt.Sscan(strings.TrimPrefix(f[i], "--replicas="), &c.Replicas)
		case strings.HasPrefix(f[i], "-"):
			return nil, true, fmt.Errorf("unsupported flag %s", f[i])
		default:
			names = append(names, f[i])
		}
	}
	for _, n := range names {
		kind, name, ok := strings.Cut(n, "/")
		if !ok {
			return nil, true, fmt.Errorf("refer to objects as kind/name, got %q", n)
		}
		c.Refs = append(c.Refs, Ref{Kind: canonicalKind(kind), Name: name, Namespace: ns})
	}
	for i := range c.Refs {
		if !namespaced(c.Refs[i].Kind) {
			c.Refs[i].Namespace = ""
		}
	}
	if len(c.Refs) == 0 {
		return nil, true, errors.New(c.Verb + " needs at least one kind/name")
	}
	if c.Verb == "scale" && c.Replicas < 0 {
		return nil, true, errors.New("scale needs --replicas=N")
	}
	return c, true, nil
}

var kindAliases = map[string]string{"deploy": "Deployment", "deployment": "Deployment", "deployments": "Deployment", "sts": "StatefulSet",
	"statefulset": "StatefulSet", "ds": "DaemonSet", "daemonset": "DaemonSet", "svc": "Service", "service": "Service", "cm": "ConfigMap",
	"configmap": "ConfigMap", "secret": "Secret", "pvc": "PersistentVolumeClaim", "persistentvolumeclaim": "PersistentVolumeClaim",
	"pv": "PersistentVolume", "ns": "Namespace", "namespace": "Namespace", "pod": "Pod", "po": "Pod", "job": "Job", "cronjob": "CronJob",
	"ingress": "Ingress", "ing": "Ingress", "crd": "CustomResourceDefinition", "sa": "ServiceAccount", "serviceaccount": "ServiceAccount",
	"rs": "ReplicaSet", "replicaset": "ReplicaSet", "role": "Role", "rolebinding": "RoleBinding", "hpa": "HorizontalPodAutoscaler"}

func canonicalKind(k string) string {
	if c, ok := kindAliases[strings.ToLower(k)]; ok {
		return c
	}
	return k
}

func ns(t adapter.Target) string {
	if n := t.Config["namespace"]; n != "" {
		return n
	}
	return "default"
}

func getArgs(r Ref) []string {
	args := []string{"get", resourceName(r), r.Name, "-o", "json", "--ignore-not-found"}
	if r.Namespace != "" {
		args = append(args, "-n", r.Namespace)
	}
	return args
}

func resourceName(r Ref) string {
	if r.APIVersion != "" && strings.Contains(r.APIVersion, "/") {
		group := strings.SplitN(r.APIVersion, "/", 2)[0]
		switch group {
		case "apps", "batch", "networking.k8s.io", "rbac.authorization.k8s.io", "policy", "autoscaling", "storage.k8s.io", "apiextensions.k8s.io":
			return r.Kind
		}
		return r.Kind + "." + group
	}
	return r.Kind
}

func getLive(ctx context.Context, t adapter.Target, r Ref) (Obj, error) {
	out, err := kubectl(ctx, t, nil, getArgs(r)...)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var o Obj
	return o, json.Unmarshal(out, &o)
}

var serverFields = []string{"managedFields", "resourceVersion", "uid", "creationTimestamp", "generation", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"}

// strip removes server-set fields so objects can be compared and re-applied.
func strip(o Obj) Obj {
	if o == nil {
		return nil
	}
	b, _ := json.Marshal(o)
	var c Obj
	_ = json.Unmarshal(b, &c)
	delete(c, "status")
	if md, ok := c["metadata"].(map[string]any); ok {
		for _, f := range serverFields {
			delete(md, f)
		}
		if an, ok := md["annotations"].(map[string]any); ok {
			delete(an, "kubectl.kubernetes.io/last-applied-configuration")
			delete(an, "deployment.kubernetes.io/revision")
			if len(an) == 0 {
				delete(md, "annotations")
			}
		}
	}
	return c
}

// stateHash fingerprints an object's desired state (status and server-set
// fields stripped), so controller status updates are not mistaken for drift.
func stateHash(o Obj) string {
	if o == nil {
		return "absent"
	}
	b, _ := json.Marshal(strip(o))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func resourceVersion(o Obj) string {
	if o == nil {
		return "absent"
	}
	md, _ := o["metadata"].(map[string]any)
	return str(md["resourceVersion"])
}

// otherManagers lists field managers other than Changeward (ignoring
// controllers that only write status).
func otherManagers(o Obj) []string {
	md, _ := o["metadata"].(map[string]any)
	mf, _ := md["managedFields"].([]any)
	seen := map[string]bool{}
	var out []string
	for _, e := range mf {
		m, _ := e.(map[string]any)
		name := str(m["manager"])
		if name == "" || name == "changeward" || str(m["subresource"]) == "status" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// GitOpsOwner reports the GitOps controller managing an object, if any.
func GitOpsOwner(o Obj) string {
	md, _ := o["metadata"].(map[string]any)
	labels, _ := md["labels"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	switch {
	case ann["argocd.argoproj.io/tracking-id"] != nil, labels["argocd.argoproj.io/instance"] != nil:
		return "argocd"
	case labels["kustomize.toolkit.fluxcd.io/name"] != nil, labels["helm.toolkit.fluxcd.io/name"] != nil:
		return "flux"
	case labels["app.kubernetes.io/managed-by"] == "Helm":
		return "helm"
	}
	return ""
}

// diffObjects lists leaf differences between two stripped objects.
func diffObjects(a, b any, path string, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			diffObjects(am[k], bm[k], path+"."+k, out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, fmt.Sprintf("%s: %s → %s", strings.TrimPrefix(path, "."), short(a), short(b)))
	}
}

func short(v any) string {
	if v == nil {
		return "(none)"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// step is one object in a plan.
type step struct {
	Ref    Ref    `json:"ref"`
	Action string `json:"action"`           // create, update, unchanged, delete, scale
	PrevRV string `json:"prev_state"`       // stateHash of the live object at plan time
	Prev   Obj    `json:"prev,omitempty"`   // stripped live object before
	Desire Obj    `json:"desire,omitempty"` // manifest to apply
	Owner  string `json:"owner,omitempty"`
	// Managers are other field managers whose fields this change takes over.
	Managers []string `json:"managers,omitempty"`
	Diff     []string `json:"diff,omitempty"`
	Replicas int      `json:"replicas,omitempty"`
}

type planData struct {
	Steps []step `json:"steps"`
}

// Classify implements adapter.Adapter.
func (a *Adapter) Classify(_ context.Context, t adapter.Target, op adapter.Operation) (adapter.Classification, error) {
	c, isCmd, err := parseCommand(op.Payload, ns(t))
	if err != nil {
		return adapter.Classification{}, err
	}
	cls := adapter.Classification{Access: adapter.Write}
	if isCmd {
		if c.Verb == "delete" {
			cls.Access = adapter.Destructive
		}
		for _, r := range c.Refs {
			cls.Objects = append(cls.Objects, r.String())
		}
		cls.Statements = []adapter.StatementClass{{Kind: strings.ToUpper(c.Verb), Access: cls.Access, Objects: cls.Objects}}
		return cls, nil
	}
	objs, err := parseManifests(op.Payload)
	if err != nil {
		return adapter.Classification{}, err
	}
	for _, o := range objs {
		cls.Objects = append(cls.Objects, refOf(o, ns(t)).String())
	}
	cls.Statements = []adapter.StatementClass{{Kind: "APPLY", Access: adapter.Write, Objects: cls.Objects}}
	return cls, nil
}

// Plan implements adapter.Adapter.
func (a *Adapter) Plan(ctx context.Context, t adapter.Target, op adapter.Operation) (*adapter.Plan, error) {
	if op.Kind != "k8s" && op.Kind != "" {
		return nil, fmt.Errorf("kubernetes: unsupported operation kind %q", op.Kind)
	}
	cls, err := a.Classify(ctx, t, op)
	if err != nil {
		return nil, err
	}
	var steps []step
	cmd, isCmd, _ := parseCommand(op.Payload, ns(t))
	if isCmd {
		for _, r := range cmd.Refs {
			live, err := getLive(ctx, t, r)
			if err != nil {
				return nil, err
			}
			if live == nil {
				return nil, fmt.Errorf("%s not found", r)
			}
			r.APIVersion = str(live["apiVersion"])
			s := step{Ref: r, Action: cmd.Verb, PrevRV: stateHash(live), Prev: strip(live), Owner: GitOpsOwner(live), Replicas: cmd.Replicas}
			if cmd.Verb == "scale" {
				spec, _ := s.Prev["spec"].(map[string]any)
				s.Diff = []string{fmt.Sprintf("spec.replicas: %v → %d", spec["replicas"], cmd.Replicas)}
				desire := strip(live)
				desire["spec"].(map[string]any)["replicas"] = cmd.Replicas
				s.Desire = desire
			}
			steps = append(steps, s)
		}
	} else {
		objs, err := parseManifests(op.Payload)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			r := refOf(o, ns(t))
			if r.Namespace != "" {
				o["metadata"].(map[string]any)["namespace"] = r.Namespace
			}
			live, err := getLive(ctx, t, r)
			if err != nil {
				return nil, err
			}
			manifest, _ := json.Marshal(o)
			out, err := kubectl(ctx, t, manifest, "apply", "--server-side", "--force-conflicts", "--dry-run=server", "--field-manager=changeward", "-o", "json", "-f", "-")
			if err != nil {
				return nil, fmt.Errorf("server-side dry run of %s: %w", r, err)
			}
			var dry Obj
			if err := json.Unmarshal(out, &dry); err != nil {
				return nil, err
			}
			s := step{Ref: r, PrevRV: stateHash(live), Desire: o}
			if live == nil {
				s.Action = "create"
			} else {
				s.Prev, s.Owner, s.Managers = strip(live), GitOpsOwner(live), otherManagers(live)
				diffObjects(s.Prev, strip(dry), "", &s.Diff)
				s.Action = "update"
				if len(s.Diff) == 0 {
					s.Action = "unchanged"
				}
			}
			steps = append(steps, s)
		}
	}
	p := &adapter.Plan{ID: adapter.NewID("p"), Adapter: "kubernetes", Target: t.Name, Env: t.Env, Operation: op, Classification: cls,
		Reversibility: adapter.Reversible, Facts: map[string]string{}, RehearsalTier: "server-dry-run", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	var diff strings.Builder
	h := sha256.New()
	owners := map[string]bool{}
	sym := map[string]string{"create": "+", "update": "~", "unchanged": "=", "delete": "-", "scale": "~"}
	for _, s := range steps {
		fmt.Fprintf(h, "%s=%s;", s.Ref, s.PrevRV)
		fmt.Fprintf(&diff, "%s %s (%s)\n", sym[s.Action], s.Ref, s.Action)
		for i, d := range s.Diff {
			if i == 12 {
				fmt.Fprintf(&diff, "     … %d more\n", len(s.Diff)-12)
				break
			}
			fmt.Fprintf(&diff, "     %s\n", d)
		}
		if s.Action != "unchanged" {
			p.BlastRows++
			p.Effects = append(p.Effects, adapter.Effect{Kind: "object-" + s.Action, Object: s.Ref.String(), Description: s.Action + " " + s.Ref.String(), Rows: 1})
		}
		if s.Action == "delete" {
			p.Destructive = true
			if IrreversibleDeleteKinds[s.Ref.Kind] {
				p.Reversibility = adapter.Irreversible
				p.Warnings = append(p.Warnings, fmt.Sprintf("deleting %s destroys its data or every object in it; take a VolumeSnapshot or backup first", s.Ref))
			}
		}
		if len(s.Managers) > 0 && s.Action == "update" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: takes over fields currently managed by %s", s.Ref, strings.Join(s.Managers, ", ")))
		}
		if s.Ref.Kind == "Secret" && s.Action != "unchanged" {
			p.Warnings = append(p.Warnings, "Secret values are kept in the undo handle in the local store")
		}
		if s.Owner != "" && s.Owner != "helm" {
			owners[s.Owner] = true
			if op.Params["gitops_sync"] != "true" { // the GitOps gate plans the controller's own sync
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s is managed by %s; a direct change will be reverted on the next sync. Change it in Git, or pause sync for the incident", s.Ref, s.Owner))
			}
		}
	}
	if len(owners) > 0 {
		var o []string
		for k := range owners {
			o = append(o, k)
		}
		sort.Strings(o)
		p.Facts["gitops_owner"] = strings.Join(o, ",")
		p.Classification.Flags = append(p.Classification.Flags, "gitops-owned")
	}
	p.PreconditionHash = hex.EncodeToString(h.Sum(nil))
	p.Diff = fmt.Sprintf("plan %s · %s (%s) · %s · %d objects · server-side dry run\n%s", p.ID, t.Name, t.Env, p.Reversibility, p.BlastRows, diff.String())
	if len(p.Warnings) > 0 {
		p.Diff += "warnings:\n"
		for _, w := range p.Warnings {
			p.Diff += "  ! " + w + "\n"
		}
	}
	p.AdapterData, _ = json.Marshal(planData{Steps: steps})
	p.Hash = adapter.HashPlan(p)
	return p, nil
}

type undoStep struct {
	Ref    Ref    `json:"ref"`
	Prev   Obj    `json:"prev,omitempty"` // nil: did not exist
	PostRV string `json:"post_state"`     // stateHash right after apply ("absent" after delete)
}

type undoHandle struct {
	Steps []undoStep `json:"steps"`
}

// Apply implements adapter.Adapter.
func (a *Adapter) Apply(ctx context.Context, t adapter.Target, p *adapter.Plan) (*adapter.Receipt, error) {
	var d planData
	if err := json.Unmarshal(p.AdapterData, &d); err != nil {
		return nil, err
	}
	// Preconditions: nothing changed since the plan.
	for _, s := range d.Steps {
		live, err := getLive(ctx, t, s.Ref)
		if err != nil {
			return nil, err
		}
		if stateHash(live) != s.PrevRV {
			return nil, &adapter.ConflictError{Reason: "objects changed since plan; re-plan required", Conflicts: []string{s.Ref.String()}}
		}
	}
	var uh undoHandle
	var out []string
	for _, s := range d.Steps {
		switch s.Action {
		case "unchanged":
			continue
		case "delete":
			args := []string{"delete", resourceName(s.Ref), s.Ref.Name, "--wait=true"}
			if s.Ref.Namespace != "" {
				args = append(args, "-n", s.Ref.Namespace)
			}
			if _, err := kubectl(ctx, t, nil, args...); err != nil {
				return a.partial(ctx, t, p, uh, err)
			}
			uh.Steps = append(uh.Steps, undoStep{Ref: s.Ref, Prev: s.Prev, PostRV: "absent"})
		default: // create, update, scale
			manifest, _ := json.Marshal(s.Desire)
			res, err := kubectl(ctx, t, manifest, "apply", "--server-side", "--force-conflicts", "--field-manager=changeward", "-o", "json", "-f", "-")
			if err != nil {
				return a.partial(ctx, t, p, uh, err)
			}
			var o Obj
			_ = json.Unmarshal(res, &o)
			uh.Steps = append(uh.Steps, undoStep{Ref: s.Ref, Prev: s.Prev, PostRV: stateHash(o)})
		}
		out = append(out, s.Action+" "+s.Ref.String())
	}
	raw, _ := json.Marshal(uh)
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: p.Hash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output: strings.Join(out, "; "), RowsAffected: int64(len(uh.Steps)), UndoHandle: raw, Reversibility: p.Reversibility}, nil
}

// partial undoes the steps already applied when a later one fails.
func (a *Adapter) partial(ctx context.Context, t adapter.Target, p *adapter.Plan, uh undoHandle, cause error) (*adapter.Receipt, error) {
	raw, _ := json.Marshal(uh)
	if _, err := a.Undo(ctx, t, p, &adapter.Receipt{UndoHandle: raw, Reversibility: adapter.Reversible}); err != nil {
		return nil, fmt.Errorf("%v; rolling back the objects already applied also failed: %w", cause, err)
	}
	return nil, fmt.Errorf("%w (objects already applied were rolled back)", cause)
}

// Undo implements adapter.Adapter, last object first, guarded by resourceVersion.
func (a *Adapter) Undo(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Receipt, error) {
	if r.Reversibility == adapter.Irreversible {
		return nil, errors.New("this change deleted objects whose data cannot be restored; restore from a snapshot")
	}
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	var conflicts []string
	for _, s := range uh.Steps {
		live, err := getLive(ctx, t, s.Ref)
		if err != nil {
			return nil, err
		}
		if stateHash(live) != s.PostRV {
			conflicts = append(conflicts, s.Ref.String()+" changed since apply")
		}
	}
	if len(conflicts) > 0 {
		return nil, &adapter.ConflictError{Reason: "objects changed since apply; undo refused (nothing was changed)", Conflicts: conflicts}
	}
	for i := len(uh.Steps) - 1; i >= 0; i-- {
		s := uh.Steps[i]
		nsArgs := []string{}
		if s.Ref.Namespace != "" {
			nsArgs = []string{"-n", s.Ref.Namespace}
		}
		switch {
		case s.Prev == nil: // was created: delete it
			if _, err := kubectl(ctx, t, nil, append([]string{"delete", resourceName(s.Ref), s.Ref.Name, "--wait=true"}, nsArgs...)...); err != nil {
				return nil, err
			}
		case s.PostRV == "absent": // was deleted: re-create it
			b, _ := json.Marshal(s.Prev)
			if _, err := kubectl(ctx, t, b, "create", "--field-manager=changeward", "-f", "-"); err != nil {
				return nil, err
			}
		default: // was updated: put the previous object back exactly
			live, err := getLive(ctx, t, s.Ref)
			if err != nil {
				return nil, err
			}
			prev := strip(s.Prev)
			prev["metadata"].(map[string]any)["resourceVersion"] = resourceVersion(live)
			b, _ := json.Marshal(prev)
			if _, err := kubectl(ctx, t, b, "replace", "--field-manager=changeward", "-f", "-"); err != nil {
				return nil, err
			}
		}
	}
	return &adapter.Receipt{ID: adapter.NewID("r"), PlanHash: r.PlanHash, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RowsAffected: int64(len(uh.Steps)), Reversibility: adapter.Reversible, Output: fmt.Sprintf("restored %d objects", len(uh.Steps))}, nil
}

// Verify implements adapter.Adapter: workloads must finish rolling out.
func (a *Adapter) Verify(ctx context.Context, t adapter.Target, _ *adapter.Plan, r *adapter.Receipt) (*adapter.Verification, error) {
	var uh undoHandle
	if err := json.Unmarshal(r.UndoHandle, &uh); err != nil {
		return nil, err
	}
	v := &adapter.Verification{Passed: true}
	timeout := t.Config["rollout_timeout"]
	if timeout == "" {
		timeout = "120s"
	}
	for _, s := range uh.Steps {
		switch s.Ref.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			if s.PostRV == "absent" {
				continue
			}
			args := []string{"rollout", "status", strings.ToLower(s.Ref.Kind) + "/" + s.Ref.Name, "--timeout=" + timeout}
			if s.Ref.Namespace != "" {
				args = append(args, "-n", s.Ref.Namespace)
			}
			_, err := kubectl(ctx, t, nil, args...)
			ok := err == nil
			detail := "rolled out"
			if err != nil {
				detail = err.Error()
			}
			v.Checks = append(v.Checks, adapter.Check{Name: "rollout " + s.Ref.String(), Passed: ok, Detail: detail})
			v.Passed = v.Passed && ok
		}
	}
	if len(v.Checks) == 0 {
		v.Checks = []adapter.Check{{Name: "applied", Passed: true}}
	}
	return v, nil
}

// EnvKubeconfig is a helper for tests and the CLI: the default kubeconfig path.
func EnvKubeconfig() string { return os.Getenv("KUBECONFIG") }
