// Package planning ties the estimator and scheduler to stored state so both the
// deploy-form evaluation and the controller's scheduling use identical logic:
// build the model estimate + node snapshot, run the scheduler, and (for the
// chosen tp) produce the VRAM breakdown.
package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
	pkgest "gpuworks.ai/gpuworks/pkg/estimate"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// Input is one placement evaluation.
type Input struct {
	Model *store.Model
	// SpecWeightBytes is the authoritative catalog estimate when a legacy model
	// row has not yet been hydrated with metadata.
	SpecWeightBytes int64
	// RuntimeWeightBytes is the selected loader layout's resident weight size.
	// It may be smaller than the repository/download size when a repository
	// contains mutually exclusive layouts (for example Mistral consolidated and
	// Hugging Face indexed shards).
	RuntimeWeightBytes   int64
	ComponentWeightBytes int64
	// KVBytesPerToken is the model's measured KV cost per token across the
	// tensor-parallel group. Zero means unmeasured, and the breakdown then
	// reports no token capacity rather than a derived one.
	KVBytesPerToken int64
	// ComponentArtifacts lets disk admission account for each component's
	// node-local present/partial/absent state. When omitted, the aggregate
	// component bytes are reserved conservatively.
	ComponentArtifacts []Artifact
	// RuntimeImageBytes is the catalog's unpacked image size estimate. It is
	// checked against the runtime filesystem independently from model storage.
	RuntimeImageBytes int64
	// RuntimeImageCached is an agent-observed cache fact. true means the
	// runtime image is already present and requires no additional disk; false
	// and nil conservatively reserve the catalog estimate.
	RuntimeImageCached *bool
	Node               *store.Node // must be loaded with GPUs
	Engine             string
	Quick              deploy.LaunchConfig
	// Parallel is the deployment's parallelism; an unstated one lets the
	// scheduler choose the GPU count on a single node.
	Parallel       parallelism.Spec
	GPUIndexes     []int // optional manual GPU selection
	VRAMLimitBytes int64 // > 0 = dGPU memory-limited (shareable) deployment
	// Sleepable is the deployment's declaration that it may be put to sleep,
	// a platform feature not every vendor provides.
	Sleepable         bool
	PD                bool
	PartitionBackend  string
	PlacementResolver string
	RequestedFeatures *proto.RequestedFeatures
	TargetRequests    map[int]TargetCardRequest
	ValidateGPU       func(store.GPU) error
	// ServingUnavailable rejects the node: its management network may not
	// serve and it has no serving network of its own. PlanLaunch sets it from
	// its Networks.
	ServingUnavailable bool
	// Unloaded plans against the node as if nothing ran on it: every card
	// free and present, no disk spoken for. What is left is what the node
	// is - its cards, their memory, the dtypes they run, whether it may serve
	// - so a refusal here is one no amount of waiting fixes (CanEverFit).
	Unloaded bool
	// ExcludeDeploymentID previews the node without that deployment's
	// instances: an edit of the deployment replaces them, so what they hold
	// now is not in the way of the launch being previewed. Zero excludes none.
	ExcludeDeploymentID uint
	// DeploymentID is the launching deployment's, zero for one not created
	// yet: its own instances never give up their cards to it (launchYield).
	DeploymentID uint
	// YieldPolicy is the launching deployment's (yieldpolicy.Never, Idle, ...):
	// a sleeping deployment that does not share its cards with that policy
	// keeps its whole claim on them (yieldpolicy.SharesCardsWith), so the
	// launch does not take cards it would want back. Empty reads as never.
	YieldPolicy string
	// ExplainHolders names, when the launch does not fit only because yield
	// policy keeps it off awake holders' cards, those holders (launchHeld).
	// The deploy form's evaluation asks for it; the controller does not.
	ExplainHolders bool
	// ModelRules are the deployment's model source rules (store.SourceRules).
	ModelRules store.SourceRules
}

// UnsupportedFeatureReasons reports why no accelerator card can satisfy these
// requested capabilities, using the same per-card gate as placement.
func UnsupportedFeatureReasons(node *store.Node, features ...accelerator.Feature) []scheduler.Reason {
	return TargetRefusals(node, features, nil)
}

// Artifact identifies one independently downloaded model tree.
type Artifact struct {
	ModelID      uint
	StorageBytes int64
}

// Output bundles the schedule result and the VRAM breakdown.
// ScheduleResult and ScheduleReason re-export the scheduler's answer shapes:
// callers that read a plan's result (the API's automatic placement among them)
// stay decoupled from the scheduler package itself.
type ScheduleResult = scheduler.Result
type ScheduleReason = scheduler.Reason

type Output struct {
	GPUCandidates []scheduler.GPUCandidate

	Result    scheduler.Result
	Breakdown estimate.Breakdown
	Config    *modelconfig.Config
	Warnings  []messages.Warning
	Eviction  []EvictSuggestion // when no fit but sleeping these would free room
	// Yield is set when no fit but the deployments sharing the node's cards
	// by yield policy would make room by giving them up (launchYield).
	Yield *LaunchYield
	// Reserved is set when no fit only because sleepers keep their cards for
	// a request's wake against this launch (launchReserved).
	Reserved *LaunchReserved
	// Held is set when no fit only because yield policy keeps the launch off
	// awake holders' cards, and only when Input.ExplainHolders asks (launchHeld).
	Held           *LaunchHeld
	Placement      placement.Result
	PlacementError error
}

// CatalogImageBytes returns the catalog's unpacked image size estimate. A
// missing/unknown catalog entry returns zero so old catalogs remain schedulable
// while nodes that do report both facts get an admission check.
func CatalogImageBytes(st *store.Store, imageRef string) int64 {
	if st == nil || strings.TrimSpace(imageRef) == "" {
		return 0
	}
	image, err := st.ResolveImageRef(strings.TrimSpace(imageRef))
	if err != nil || image.SizeBytes <= 0 {
		return 0
	}
	return image.SizeBytes
}

// engineReserveBase is what the engine itself holds back besides weights and
// KV. It is declared by the engine's own profile rather than by the estimator,
// because only the adapter knows which engine it is; an engine that declares
// none leaves it zero and the estimator's measured default model applies.
func engineReserveBase(engine string) int64 {
	profile, err := deploy.ProfileForEngine(engine)
	if err != nil {
		return 0
	}
	return profile.ReserveBase
}

// Evaluate estimates and schedules a deployment onto a node.
func (in Input) Evaluate(st *store.Store) Output {
	var out Output
	cfg := LoadConfig(in.Model)
	out.Config = cfg
	if _, err := deploy.ProfileForEngine(in.Engine); err != nil {
		out.Warnings = append(out.Warnings, messages.NewWarning(messages.WarningPlanningEngineUnavailable, map[string]any{"engine": in.Engine, "detail": err.Error()}))
	}

	maxLen := in.Quick.MaxModelLen
	if maxLen <= 0 && cfg != nil {
		// Estimate an unset context from the model limit; the engine's effective
		// default may differ. This is not a node-sized context recommendation.
		maxLen = cfg.MaxPositionEmbeddings
	}
	kvDtype := in.Quick.KVCacheDtype
	storageWeightBytes := in.Model.WeightBytes
	if storageWeightBytes <= 0 && in.SpecWeightBytes > 0 {
		storageWeightBytes = in.SpecWeightBytes
	}
	weightBytes := storageWeightBytes
	if in.RuntimeWeightBytes > 0 {
		weightBytes = in.RuntimeWeightBytes
	}
	componentWeightBytes := in.ComponentWeightBytes
	if componentWeightBytes < 0 {
		componentWeightBytes = 0
	}
	totalWeightBytes := weightBytes + componentWeightBytes
	runtimeImageBytes := in.RuntimeImageBytes
	if in.RuntimeImageCached != nil && *in.RuntimeImageCached {
		runtimeImageBytes = 0
	}
	storageAdmit := admitStorage(st, in, storageWeightBytes, componentWeightBytes)
	diskNeedBytes := storageAdmit.NeedBytes
	// KV cache is allocated dynamically by the serving engine. It is exposed as
	// an informational capacity estimate below, but is intentionally excluded
	// from scheduler startup admission.
	mmEncoder := pkgest.MMEncoderRuntimeBytes(cfg, in.Quick.Dtype)

	// dtype support check (drives a hard filter + a warning).
	vendor, archID, arch, cc := nodeGPUProfile(in.Node)
	dtypeOK, dtypeReason := estimate.DtypeSupportForGPU(vendor, archID, arch, in.Quick.Dtype, in.Quick.Quantization, cc)
	for _, q := range estimate.ArchQuirks(arch) {
		out.Warnings = append(out.Warnings, messages.NewWarning(messages.WarningPlanningGPUArchCaution, map[string]any{"detail": q}))
	}

	view := ledgerView{excludeDeployment: in.ExcludeDeploymentID, reserved: reservedAgainst(st, in.YieldPolicy)}
	snap := nodeSnapshot(st, in.Node, dtypeOK, dtypeReason, view)
	snap.ServingUnavailable = in.ServingUnavailable
	if in.Unloaded {
		// A card that is busy or unhealthy now is a reason to wait, not to
		// refuse; so is a disk that is full now.
		for i := range snap.GPUs {
			snap.GPUs[i].AllocatedBytes, snap.GPUs[i].SleepingBytes, snap.GPUs[i].Schedulable = 0, 0, true
			snap.GPUs[i].WholeGPUAllocated = false
		}
		diskNeedBytes, runtimeImageBytes = 0, 0
		// A storage that is not ready here, or a cache full of models in use,
		// passes with time; a model larger than the cache cap does not.
		if storageAdmit.Refusal != nil && storageAdmit.Refusal.Code != scheduler.ReasonModelExceedsCacheLimit {
			storageAdmit.Refusal = nil
		}
	} else if storageAdmit.AvailBytes > 0 {
		snap.DiskAvailBytes = storageAdmit.AvailBytes
	}
	allGPUs := snap.GPUs
	// Inventory-only cards also need feedback, but never enter the scheduler's pool.
	for _, gpu := range in.Node.GPUs {
		if !accelerator.CardUsable(gpu.Ownership) {
			allGPUs = append(allGPUs, scheduler.GPU{Index: gpu.Index})
		}
	}
	if NeedsTargetPreflight(in.Node) {
		if refusal := constrainTargetGPUs(in, &snap); refusal != nil {
			out.Result = scheduler.Result{OK: false, Message: *refusal, Explain: scheduler.Explain{Filters: []scheduler.Reason{*refusal}}}
			return out
		}
	}
	// This is the one place an unset utilization becomes a number. A deployment
	// that stated none runs at the engine's own default and carries no
	// --gpu-memory-utilization flag, so the estimate has to size against that
	// same default - which the dictionary records, and which nothing else in
	// the estimator, the reserve or the scheduler is allowed to guess at.
	util := in.Quick.GPUMemUtil
	if util <= 0 {
		util = deploy.DefaultGPUMemUtil(in.Engine)
	}
	// One reserve for this deployment, computed once and handed to both the
	// scheduler and the breakdown: the number a user is shown and the number
	// admission enforces have to be the same one.
	reserveBase := engineReserveBase(in.Engine)
	reserve := estimate.ReserveBytes(reserveBase, arch, !in.Quick.EnforceEager, util)
	req := scheduler.Request{
		ReserveBytes:         reserve,
		ReserveBase:          reserveBase,
		WeightBytes:          totalWeightBytes,
		MMEncoderBytes:       mmEncoder,
		DiskNeedBytes:        diskNeedBytes,
		RuntimeDiskNeedBytes: runtimeImageBytes,
		HeadCount:            estimate.HeadCount(cfg),
		AllowedGPUIndexes:    in.GPUIndexes,
		PerGPULimitBytes:     in.VRAMLimitBytes,
		PartitionBackend:     in.PartitionBackend,
		GPUMemUtil:           util,
		StorageRefusal:       storageAdmit.Refusal,
		Features:             requestedInputFeaturesForVendor(in, snap.Vendor),
		UtilizationIsShare:   deploy.UtilizationSizesMemory(in.Engine),
	}
	req.Parallel = in.Parallel
	if !in.Parallel.Stated() && strings.TrimSpace(in.PlacementResolver) != "" && len(in.GPUIndexes) > 0 {
		// A user-selected list is exact for a topology resolver. Treating it as
		// a pool would evaluate only a subset and silently turn an explicit
		// eight-card request into a one-card candidate.
		req.AllowedTPs = []int{len(in.GPUIndexes)}
	}
	var candidateError error
	if strings.TrimSpace(in.PlacementResolver) != "" {
		req.EvaluateCandidate = func(candidate []scheduler.GPU) scheduler.CandidateEvaluation {
			indexes := make([]int, len(candidate))
			for index := range candidate {
				indexes[index] = candidate[index].Index
			}
			resolved, err := placement.Resolve(context.Background(), in.PlacementResolver, placementGPU(in.Node, indexes, len(in.GPUIndexes) == 0))
			if err != nil {
				candidateError = err
				return scheduler.CandidateEvaluation{RejectionReason: err.Error()}
			}
			if resolved.Reason != "" {
				return scheduler.CandidateEvaluation{RejectionReason: resolved.Reason}
			}
			return scheduler.CandidateEvaluation{GPUMemoryBytesPerDevice: resolved.GPUMemoryMiBPerDevice << 20, Preference: resolved.Prefer}
		}
	}
	out.GPUCandidates = scheduler.GPUCandidates(req, allGPUs)
	out.Result = scheduler.Schedule(req, snap)
	explainDiskRefusal(st, in, req, snap, &out.Result)
	if candidateError != nil && !out.Result.OK {
		out.PlacementError = candidateError
	}
	if out.Result.OK && out.Result.Plan != nil && strings.TrimSpace(in.PlacementResolver) != "" {
		out.Placement, out.PlacementError = placement.Resolve(context.Background(), in.PlacementResolver, placementGPU(in.Node, out.Result.Plan.GPUIndexes, len(in.GPUIndexes) == 0))
		if out.PlacementError == nil && out.Placement.Reason != "" {
			out.PlacementError = fmt.Errorf("placement rejected chosen GPU set: %s", out.Placement.Reason)
		}
	}

	// The breakdown is per GPU within one DP replica, using the same
	// completed TP × PP topology as scheduling and launch assembly.
	tp := 1
	if in.Parallel.Stated() {
		tp = in.Parallel.World()
	} else if out.Result.OK && out.Result.Plan != nil {
		tp = in.Parallel.Assigned(out.Result.Plan.TP).World()
	}
	runtimeMemoryBytesPerGPU := int64(0)
	if out.Placement.GPUMemoryMiBPerDevice > 0 {
		runtimeMemoryBytesPerGPU = out.Placement.GPUMemoryMiBPerDevice << 20
	} else if !out.Result.OK {
		runtimeMemoryBytesPerGPU = refusedPlacementContract(in.PlacementResolver, in.Node, in.GPUIndexes)
	}
	out.Breakdown = estimate.Compute(estimate.Params{
		Config: cfg, ReserveBase: reserveBase, WeightBytes: weightBytes, ComponentWeightBytes: componentWeightBytes, PerDtype: nil,
		KVDtype: kvDtype, Dtype: in.Quick.Dtype, KVBytesPerToken: in.KVBytesPerToken,
		ReserveBytes: reserve,
		MaxModelLen:  maxLen, TP: tp,
		GPUMemUtil: util, VRAMLimitBytes: in.VRAMLimitBytes,
		GPUCapacityBytes:         selectedGPUCapacity(in.Node, out.Result.Plan),
		RuntimeMemoryBytesPerGPU: runtimeMemoryBytesPerGPU,
	})
	if totalWeightBytes == 0 {
		out.Warnings = append(out.Warnings, messages.NewWarning(messages.WarningPlanningModelWeightUnknown, nil))
	}
	// No fit → suggest sleeping sleepable instances to free room (confirm-based;
	// the platform never sleeps silently).
	if !out.Result.OK {
		out.Eviction = suggestEviction(st, in.Node, req, in.ServingUnavailable, view)
		// Only a launch that shares cards by yield policy may wait for
		// holders, and only one sleepers keep cards from may be refused by
		// their name, or one whose evaluation asks who holds its cards
		// (ExplainHolders): anything else reads nothing more. One read of
		// the node's instances serves every re-plan.
		shares := in.YieldPolicy != "" && in.YieldPolicy != yieldpolicy.Never
		if !in.Unloaded && st != nil && (shares || view.reserved != nil || in.ExplainHolders) {
			if insts, err := st.InstancesForNode(in.Node.ID); err == nil {
				out.Yield = launchYield(st, in.Node, insts, in.DeploymentID, in.YieldPolicy, req, dtypeOK, dtypeReason, in.ServingUnavailable, view)
				if out.Yield == nil {
					out.Reserved = launchReserved(st, in.Node, insts, req, dtypeOK, dtypeReason, in.ServingUnavailable, view)
				}
				if out.Yield == nil && out.Reserved == nil && in.ExplainHolders {
					out.Held = launchHeld(st, in.Node, insts, in.DeploymentID, in.YieldPolicy, req, dtypeOK, dtypeReason, in.ServingUnavailable, view)
				}
			}
		}
	}
	if out.Result.OK && out.Breakdown.ContextExceedsKVPool() {
		// Deliberately non-blocking (GWKS-79): new attention layouts and engine
		// versions can make the KV estimate wrong. Keep Fits, OK and the plan
		// from startup/resource admission so evaluate, create and controller
		// retries all allow the same operator-selected configuration. Do not
		// turn this warning back into a rejection or silently clamp max_model_len;
		// only the actual engine launch can settle the KV capacity question.
		b := out.Breakdown
		out.Warnings = append(out.Warnings, messages.NewWarning(messages.WarningPlanningKVBelowContext, map[string]any{
			"kv_tokens": b.EstimatedKVTokens, "max_model_len": b.MaxModelLen,
		}))
	}
	return out
}

// modelDiskNeed returns only the bytes that still have to arrive on the
// selected node. A complete local artifact consumes no new capacity, while a
// partial transfer reserves its remaining bytes. Missing or unusable state is
// treated conservatively as a full download.
func modelDiskNeed(st *store.Store, model *store.Model, node *store.Node, storageID uint, totalBytes int64) int64 {
	if totalBytes <= 0 || st == nil || model == nil || node == nil || model.ID == 0 || node.ID == 0 {
		return max(totalBytes, 0)
	}
	mf, err := st.ModelFileAt(store.FileRef{ModelID: model.ID, StorageID: storageID, NodeID: node.ID})
	if err != nil {
		return totalBytes
	}
	switch mf.State {
	case proto.MFPresent, proto.MFRegistered:
		return 0
	case proto.MFDownloading, proto.MFSyncing:
		if mf.Downloaded >= totalBytes {
			return 0
		}
		if mf.Downloaded > 0 {
			return totalBytes - mf.Downloaded
		}
	}
	return totalBytes
}

func componentDiskNeed(st *store.Store, node *store.Node, storageID uint, fallbackBytes int64, artifacts []Artifact) int64 {
	if len(artifacts) == 0 {
		return max(fallbackBytes, 0)
	}
	var needed int64
	for _, artifact := range artifacts {
		needed += modelDiskNeed(st, &store.Model{ID: artifact.ModelID}, node, storageID, artifact.StorageBytes)
	}
	return needed
}

// refusedPlacementContract is the per-card memory a placement resolver
// declares, for the breakdown of an evaluation the scheduler refused: with no
// group chosen there is nothing it resolved against, and sizing the breakdown
// on the weights instead would contradict a template that says they do not
// apply (a tiered engine). The request's cards, else the node's, stand in; a
// resolver that rejects them or cannot run declares nothing.
func refusedPlacementContract(resolver string, node *store.Node, indexes []int) int64 {
	if strings.TrimSpace(resolver) == "" || node == nil {
		return 0
	}
	automatic := len(indexes) == 0
	if automatic {
		for _, gpu := range node.OwnedGPUs() {
			indexes = append(indexes, gpu.Index)
		}
	}
	if len(indexes) == 0 {
		return 0
	}
	resolved, err := placement.Resolve(context.Background(), resolver, placementGPU(node, indexes, automatic))
	if err != nil || resolved.Reason != "" {
		return 0
	}
	return resolved.GPUMemoryMiBPerDevice << 20
}

func placementGPU(node *store.Node, indexes []int, automatic bool) placement.GPU {
	input := placement.GPU{Count: len(indexes), Indexes: append([]int(nil), indexes...), Automatic: automatic}
	selected := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		selected[index] = struct{}{}
	}
	for _, gpu := range node.OwnedGPUs() {
		if _, ok := selected[gpu.Index]; !ok {
			continue
		}
		if input.Vendor == "" {
			input.Vendor = strings.ToLower(strings.TrimSpace(gpu.Vendor))
		}
		input.Names = append(input.Names, gpu.Name)
		memory := float64(gpu.MemoryTotal) / (1 << 30)
		input.MemoriesGiB = append(input.MemoriesGiB, memory)
		if input.MinMemoryGiB == 0 || (memory > 0 && memory < input.MinMemoryGiB) {
			input.MinMemoryGiB = memory
		}
	}
	return input
}

func selectedGPUCapacity(node *store.Node, plan *scheduler.Plan) int64 {
	if node == nil {
		return 0
	}
	selected := map[int]bool{}
	if plan != nil {
		for _, index := range plan.GPUIndexes {
			selected[index] = true
		}
	}
	var capacity int64
	for _, gpu := range node.OwnedGPUs() {
		if !gpu.CanTakeWork() || (len(selected) > 0 && !selected[gpu.Index]) {
			continue
		}
		if len(selected) > 0 {
			if capacity == 0 || gpu.MemoryTotal < capacity {
				capacity = gpu.MemoryTotal
			}
		} else if gpu.MemoryTotal > capacity {
			capacity = gpu.MemoryTotal
		}
	}
	return capacity
}

// LoadConfig parses the modelconfig stored in a model's Meta (config.json).
func LoadConfig(m *store.Model) *modelconfig.Config {
	if m == nil || m.Meta == "" {
		return nil
	}
	cfg, err := modelconfig.Parse(strings.NewReader(m.Meta))
	if err != nil {
		return nil
	}
	return cfg
}

func nodeGPUProfile(node *store.Node) (string, string, string, float64) {
	owned := node.OwnedGPUs()
	if len(owned) == 0 {
		return "", "", "", 0
	}
	g := owned[0]
	for _, gpu := range owned {
		if gpu.CanTakeWork() {
			g = gpu
			break
		}
	}
	arch := node.ArchOf(g)
	return node.VendorOf(g), arch.ID, arch.Family, parseCC(g.ComputeCapability)
}

func parseCC(s string) float64 {
	var maj, min int
	if _, err := fscan(s, &maj, &min); err != nil {
		return 0
	}
	return float64(maj) + float64(min)/10
}

func fscan(s string, maj, min *int) (int, error) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) == 0 {
		return 0, errParse
	}
	*maj = atoi(parts[0])
	if len(parts) == 2 {
		*min = atoi(parts[1])
	}
	return 2, nil
}

var errParse = errStr("parse")

type errStr string

func (e errStr) Error() string { return string(e) }

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func parseInts(s string) []int {
	if s == "" {
		return nil
	}
	var v []int
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

// deploymentFeatures are the platform features a deployment asks of the nodes
// it runs on, beyond its engine and accelerator (accelerator.Feature).
func deploymentFeatures(in Input) []accelerator.Feature {
	if in.TargetRequests != nil {
		return nil
	} // Every retained card was checked with its own resolved facts.
	return requestedInputFeaturesForVendor(in, "")
}
