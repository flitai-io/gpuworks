package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TargetRequest is the per-card view of one resolved capability snapshot.
type TargetRequest struct {
	Features     []accelerator.Feature
	CommandKnown bool
}

// targetResolution is memoized only inside one immutable launch evaluation.
// The preview resolver is pure: paths/IO are absent, and all host/target facts
// that affect Params are part of the key. Both syntax failures and successful
// snapshots therefore apply to that key. Concrete node-local paths are checked
// later by DeploymentLaunch with ModelPaths, outside this cache.
type targetResolution struct {
	snapshot *proto.RequestedFeatures
	refusal  *scheduler.Reason
}

// Each cache belongs to one serial node/card scan; no callback shares it
// with another request or reconcile pass. Cached results are read-only.
type targetRequestCache map[string]targetResolution

// TargetCardRequest binds a resolved request or local resolution failure to
// one actual card. It never retains the request Params or the full launch.
type TargetCardRequest struct {
	Request TargetRequest
	Refusal *scheduler.Reason
}

func TargetRequestFor(snapshot *proto.RequestedFeatures) TargetRequest {
	result := TargetRequest{}
	if snapshot == nil {
		return result
	}
	result.CommandKnown = snapshot.CommandKnown
	for _, name := range snapshot.Names() {
		result.Features = append(result.Features, accelerator.Feature(name))
	}
	return result
}

// NeedsTargetPreflight reports whether a node has any owned accelerator card.
func NeedsTargetPreflight(node *store.Node) bool {
	return node != nil && len(node.OwnedGPUs()) > 0
}

// TargetRefusals returns a platform refusal only when no card satisfying the
// template predicate supports the requested feature set and host runtime.
func TargetRefusals(node *store.Node, features []accelerator.Feature, template func(store.GPU) bool) []scheduler.Reason {
	return TargetRefusalsByVendor(node, template, func(string) TargetRequest { return TargetRequest{Features: features, CommandKnown: true} })
}

// TargetRefusalsByVendor checks each template-compatible card using its own
// target requirements. Any compatible card keeps a mixed node eligible.
func TargetRefusalsByVendor(node *store.Node, template func(store.GPU) bool, featuresForVendor func(string) TargetRequest) []scheduler.Reason {
	return TargetRefusalsByCard(node, template, func(gpu store.GPU) TargetCardRequest {
		request := TargetRequest{CommandKnown: true}
		if featuresForVendor != nil {
			request = featuresForVendor(node.VendorOf(gpu))
		}
		return TargetCardRequest{Request: request}
	})
}

// TargetRefusalsByCard checks the actual resolved facts of each matching card.
func TargetRefusalsByCard(node *store.Node, template func(store.GPU) bool, forCard func(store.GPU) TargetCardRequest) []scheduler.Reason {
	if node == nil {
		return nil
	}
	owned := node.OwnedGPUs()
	if len(owned) == 0 {
		return nil
	}
	var out []scheduler.Reason
	seen := map[string]bool{}
	checked := false
	for _, gpu := range owned {
		if template != nil && !template(gpu) {
			continue
		}
		checked = true
		facts := TargetCardRequest{Request: TargetRequest{CommandKnown: true}}
		if forCard != nil {
			facts = forCard(gpu)
		}
		var refusals []scheduler.Reason
		if facts.Refusal != nil {
			refusals = []scheduler.Reason{*facts.Refusal}
		} else {
			refusals = targetCardRequestRefusals(node, gpu, facts.Request)
		}
		if len(refusals) == 0 {
			return nil
		}
		for _, refusal := range refusals {
			key := refusal.Code + ":" + fmt.Sprint(refusal.Params)
			if !seen[key] {
				seen[key] = true
				out = append(out, refusal)
			}
		}
	}
	if !checked {
		return nil
	}
	return out
}

func targetCardRequestRefusals(node *store.Node, gpu store.GPU, request TargetRequest) []scheduler.Reason {
	vendor := node.VendorOf(gpu)
	if mismatch := accelerator.RuntimeMismatchFor(vendor, accelerator.HostRuntime{OS: node.OS, Arch: node.Arch, Driver: node.RuntimeDriver}); mismatch != nil {
		code := scheduler.ReasonRuntimeUnsupported
		if mismatch.MissingFacts {
			code = scheduler.ReasonRuntimeUnreported
		}
		return []scheduler.Reason{{Code: code, Params: mismatch.Params()}}
	}

	var refusals []scheduler.Reason
	for _, feature := range request.Features {
		if !accelerator.Supports(vendor, feature) {
			refusals = append(refusals, scheduler.UnsupportedFeature(vendor, feature))
		}
	}
	if len(refusals) == 0 && !request.CommandKnown && accelerator.RequiresKnownLaunchFeatures(vendor) {
		return []scheduler.Reason{{Code: scheduler.ReasonLaunchFeaturesUnknown, Params: map[string]any{"vendor": accelerator.VendorBrand(vendor)}}}
	}
	return refusals
}

func constrainTargetGPUs(in Input, snap *scheduler.NodeSnapshot) *scheduler.Reason {
	if snap == nil || in.Node == nil {
		return nil
	}
	cards := make(map[int]store.GPU, len(in.Node.GPUs))
	for _, card := range in.Node.OwnedGPUs() {
		card.Vendor = in.Node.VendorOf(card)
		cards[card.Index] = card
	}
	selected := map[int]bool{}
	for _, index := range in.GPUIndexes {
		selected[index] = true
	}
	if len(selected) > 0 && in.ValidateGPU != nil {
		for _, index := range in.GPUIndexes {
			card, ok := cards[index]
			if !ok {
				continue
			}
			if err := in.ValidateGPU(card); err != nil {
				r := scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}
				return &r
			}
		}
	}
	var kept []scheduler.GPU
	var first *scheduler.Reason
	for _, candidate := range snap.GPUs {
		if len(selected) > 0 && !selected[candidate.Index] {
			continue
		}
		card, ok := cards[candidate.Index]
		if !ok {
			continue
		}
		if len(selected) == 0 && in.ValidateGPU != nil {
			if err := in.ValidateGPU(card); err != nil {
				r := scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}
				if len(selected) > 0 {
					return &r
				}
				if first == nil {
					first = &r
				}
				continue
			}
		}
		if refusal := targetInputRefusal(in.Node, card, in); refusal != nil {
			if len(selected) > 0 {
				return refusal
			}
			if first == nil {
				first = refusal
			}
			continue
		}
		kept = append(kept, candidate)
	}
	if len(selected) > 0 && len(kept) != len(selected) {
		if first != nil {
			return first
		}
		r := scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": "one or more selected GPUs are no longer available"}}
		return &r
	}
	if len(kept) == 0 {
		if first != nil {
			return first
		}
		r := scheduler.Reason{Code: scheduler.ReasonNoOwnedGPU}
		return &r
	}
	snap.GPUs = kept
	vendor := ""
	haveVendor, mixed := false, false
	for _, g := range kept {
		card := cards[g.Index]
		v := accelerator.NormalizeVendor(in.Node.VendorOf(card))
		if !haveVendor {
			vendor = v
			haveVendor = true
		} else if vendor != v {
			mixed = true
		}
	}
	if mixed {
		vendor = ""
	}
	snap.Vendor = vendor
	return nil
}
func requestedInputFeaturesForVendor(in Input, vendor string) []accelerator.Feature {
	var out []accelerator.Feature
	snapshot := in.RequestedFeatures
	if snapshot == nil {
		// Direct internal evaluation callers can state structural constraints only.
		// Runtime launch callers always supply the actual resolved snapshot.
		params := deploy.Params{Sleepable: in.Sleepable, NNodes: in.Parallel.Normalized().NNodes, PipelineParallel: in.Parallel.PP, Quick: in.Quick}
		if in.PD {
			params.PD = &deploy.PDLaunch{}
		}
		snapshot = deploy.ResolveRequestedFeatures(in.Engine, deploy.ResolvedLaunch{}, params)
	}
	for _, name := range snapshot.Names() {
		out = append(out, accelerator.Feature(name))
	}
	return out
}
func targetLaunchRefusal(err error) *scheduler.Reason {
	reason, ok := scheduler.TargetLaunchRefusal(err)
	if !ok {
		return nil
	}
	return reason
}

// TargetCandidateRefusal is the inexpensive, template-first gate used before
// automatic pool trial budgets. Full memory and placement planning still runs
// only for the bounded candidates.
func TargetCandidateRefusal(req LaunchRequest, layer deploy.CatalogSelection, node *store.Node) (*scheduler.Reason, error) {
	req.Node = node
	req.Selection = layer
	validator, err := TemplateGPUValidator(layer)
	if err != nil {
		return nil, err
	}
	requests, _ := targetRequestsFor(req, validator, targetRequestCache{}, previewRequestedFeatures)
	return targetCandidateRefusal(node, req.Placement.GPUIndexes, validator, requests)
}

func targetCandidateRefusal(node *store.Node, indexes []int, validator func(store.GPU) error, requests map[int]TargetCardRequest) (*scheduler.Reason, error) {
	input := Input{Node: node, GPUIndexes: indexes, ValidateGPU: validator, TargetRequests: requests}
	selected := map[int]bool{}
	for _, index := range indexes {
		selected[index] = true
	}
	var first *scheduler.Reason
	checked := 0
	selectedTemplateMismatch := false
	owned := node.OwnedGPUs()
	if len(owned) == 0 {
		r := scheduler.Reason{Code: scheduler.ReasonNoOwnedGPU}
		return &r, nil
	}
	for _, gpu := range owned {
		if len(selected) > 0 && !selected[gpu.Index] {
			continue
		}
		checked++
		if input.ValidateGPU != nil {
			if e := input.ValidateGPU(gpu); e != nil {
				// An explicit set is one indivisible candidate. If any selected
				// card misses the template, let the bounded trial report that
				// catalog refusal instead of an accumulated target refusal from
				// another card.
				if len(selected) > 0 {
					selectedTemplateMismatch = true
				}
				continue
			}
		}
		if refusal := targetInputRefusal(node, gpu, input); refusal != nil {
			if first == nil {
				first = refusal
			}
			continue
		}
		if len(selected) == 0 {
			return nil, nil
		}
	}
	if selectedTemplateMismatch {
		return nil, nil
	}
	if len(selected) > 0 && checked != len(selected) && first == nil {
		r := scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": "one or more selected GPUs are no longer available"}}
		return &r, nil
	}
	return first, nil
}

// TargetAdmission belongs to one serial BuildPool scan. Preparation errors
// are deployment-global; card/host refusals never enter Err. The caller must
// inspect Err after the scan and must not reuse this object concurrently.
type TargetAdmission struct {
	dep          *store.Deployment
	requirements map[string][]string
	loadModel    func() (*store.Model, error)
	online       func(string) bool
	prepared     bool
	refusal      func(*store.Node) *scheduler.Reason
	err          error
}

// TargetRefusalFor defers the one global model read until a node can actually
// participate. Offline/cordoned/unmet pools remain observable without loading
// a model that no target needs; BuildPool checks requirements before Refusal.
func TargetRefusalFor(dep *store.Deployment, reqs map[string][]string, loadModel func() (*store.Model, error), online func(string) bool) *TargetAdmission {
	return &TargetAdmission{dep: dep, requirements: reqs, loadModel: loadModel, online: online}
}

func targetNodeParticipates(node *store.Node, online func(string) bool) bool {
	return node != nil && node.Scheduling.TakesReplicas() && (online == nil || online(node.UUID))
}

func (a *TargetAdmission) Refusal(node *store.Node) *scheduler.Reason {
	if !targetNodeParticipates(node, a.online) {
		return nil
	}
	if len(node.OwnedGPUs()) == 0 {
		return &scheduler.Reason{Code: scheduler.ReasonNoOwnedGPU}
	}
	if !a.prepared {
		a.prepared = true
		if a.dep == nil || a.loadModel == nil {
			a.err = fmt.Errorf("deployment target admission requires a model loader")
			return nil
		}
		model, err := a.loadModel()
		if err != nil {
			a.err = fmt.Errorf("load deployment %d model record %d: %w", a.dep.ID, a.dep.ModelID, err)
			return nil
		}
		if model == nil && a.dep.ModelID != 0 {
			a.err = fmt.Errorf("load deployment %d model record %d: no model returned", a.dep.ID, a.dep.ModelID)
			return nil
		}
		a.refusal, a.err = targetRefusalFor(a.dep, a.requirements, model, a.online, previewRequestedFeatures)
	}
	if a.err != nil {
		return nil
	}
	return a.refusal(node)
}

// Err reports only observed global preparation failures and never starts IO.
func (a *TargetAdmission) Err() error { return a.err }

func targetRefusalFor(dep *store.Deployment, reqs map[string][]string, model *store.Model, online func(string) bool, resolve func(LaunchRequest) (*proto.RequestedFeatures, error)) (func(*store.Node) *scheduler.Reason, error) {
	req, layer, _, err := DeploymentLaunch(dep, reqs, nil, model, nil)
	if err != nil {
		return nil, err
	}
	validator, err := TemplateGPUValidator(layer)
	if err != nil {
		return nil, err
	}
	req.Topology.NNodes = dep.NNodes
	cache := targetRequestCache{}
	// Without pinned GPU indexes the verdict depends only on the node's
	// hardware shape, so a fleet of identical nodes is judged once per scan
	// instead of validating every card of every node.
	byShape := map[string]*scheduler.Reason{}
	pinned := len(req.Placement.GPUIndexes) > 0
	return func(node *store.Node) *scheduler.Reason {
		if !targetNodeParticipates(node, online) {
			return nil
		}
		shape := ""
		if !pinned {
			shape = HardwareShape(node, true)
			if verdict, ok := byShape[shape]; ok {
				return verdict
			}
		}
		candidate := req
		candidate.Node = node
		requests, _ := targetRequestsFor(candidate, validator, cache, resolve)
		refusal, e := targetCandidateRefusal(node, req.Placement.GPUIndexes, validator, requests)
		if e != nil {
			refusal = &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": e.Error()}}
		}
		if !pinned {
			byShape[shape] = refusal
		}
		return refusal
	}, nil
}

func targetInputRefusal(node *store.Node, card store.GPU, in Input) *scheduler.Reason {
	if facts, ok := in.TargetRequests[card.Index]; ok {
		if facts.Refusal != nil {
			return facts.Refusal
		}
		refusals := targetCardRequestRefusals(node, card, facts.Request)
		if len(refusals) > 0 {
			return &refusals[0]
		}
		return nil
	}

	request := TargetRequestFor(in.RequestedFeatures)
	if in.RequestedFeatures == nil {
		request = TargetRequest{Features: requestedInputFeaturesForVendor(in, node.VendorOf(card)), CommandKnown: true}
	}
	refusals := targetCardRequestRefusals(node, card, request)
	if len(refusals) > 0 {
		return &refusals[0]
	}
	return nil
}

// targetRequestsFor resolves only template-compatible cards. Its cache key
// includes the effective target configuration, so inventory ordering cannot
// make the first card stand in for a different vendor or architecture.
func targetRequestsFor(req LaunchRequest, validator func(store.GPU) error, cache targetRequestCache, resolve func(LaunchRequest) (*proto.RequestedFeatures, error)) (map[int]TargetCardRequest, *proto.RequestedFeatures) {
	out := map[int]TargetCardRequest{}
	selected := map[int]bool{}
	for _, index := range req.Placement.GPUIndexes {
		selected[index] = true
	}
	nodeShape := HardwareShape(req.Node, true)
	var representative *proto.RequestedFeatures
	representativeKey := ""
	for _, gpu := range req.Node.OwnedGPUs() {
		if len(selected) > 0 && !selected[gpu.Index] {
			continue
		}
		if validator != nil && validator(gpu) != nil {
			continue
		}
		key := nodeShape + "|target:" + fmt.Sprintf("%s/%s/%s/%s", req.Node.VendorOf(gpu), accelerator.NormalizeGPUName(gpu.Name), gpu.ArchID, gpu.ComputeCapability)
		resolution, ok := cache[key]
		if !ok {
			cardRequest := req
			cardRequest.Placement.GPUIndexes = []int{gpu.Index}
			snapshot, err := resolve(cardRequest)
			resolution.snapshot = snapshot
			if err != nil {
				resolution.refusal = targetLaunchRefusal(err)
				if resolution.refusal == nil {
					resolution.refusal = &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}
				}
			}
			cache[key] = resolution
		}
		if resolution.refusal != nil {
			out[gpu.Index] = TargetCardRequest{Refusal: resolution.refusal}
			continue
		}
		snapshot := resolution.snapshot
		out[gpu.Index] = TargetCardRequest{Request: TargetRequestFor(snapshot)}
		if representative == nil || key < representativeKey {
			representative, representativeKey = snapshot, key
		}
	}
	return out, representative
}

// TargetRequestsForLaunch prepares one real launch draft for repeated node
// eligibility checks, sharing pure results only by effective target shape.
// The returned callback belongs to the caller's serial node scan.
func TargetRequestsForLaunch(req LaunchRequest) (func(*store.Node) map[int]TargetCardRequest, error) {
	validator, err := TemplateGPUValidator(req.Selection)
	if err != nil {
		return nil, err
	}
	cache := targetRequestCache{}
	return func(node *store.Node) map[int]TargetCardRequest {
		if node == nil {
			return nil
		}
		candidate := req
		candidate.Node = node
		requests, _ := targetRequestsFor(candidate, validator, cache, previewRequestedFeatures)
		return requests
	}, nil
}
