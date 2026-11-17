package planning

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A callable deployment must plan like any other. Its engine publishes no
// parameter dictionary, so an unstated utilization comes from the engine
// profile; resolving it to zero made planning hand the scheduler a value
// Request.Validate rejects ("gpu_memory_utilization must be within (0,1]") and
// no instance was ever created for such a deployment.
func TestEvaluatePlansADictionaryLessEngine(t *testing.T) {
	st, err := store.Open("file:planning_dictionary_less_engine?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("callable-node", store.HashToken("token"), "callable-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 24 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	model := &store.Model{ID: 1, Name: "callable-model", WeightBytes: 4 << 30}

	out := (Input{Model: model, Node: node, Engine: deploy.EngineCallable, Quick: launchConfig(t, deploy.EngineCallable)}).Evaluate(st)
	if !out.Result.OK || out.Result.Plan == nil {
		t.Fatalf("a callable deployment must be placeable: %+v", out.Result)
	}
	if out.Breakdown.GPUMemUtil <= 0 || out.Breakdown.ReserveBytes <= 0 {
		t.Fatalf("callable breakdown = %+v, want a utilization and a reserve", out.Breakdown)
	}

	// The GPU ledger charges the same assumption: a running callable instance
	// with no stated utilization is not cheaper than a vLLM one.
	claim := func(engine string) int64 {
		desired, err := json.Marshal(map[string]any{"engine": engine, "vram_limit_bytes": int64(8) << 30})
		if err != nil {
			t.Fatal(err)
		}
		return instanceClaim(st, &store.Instance{UUID: engine + "-instance", State: proto.InstRunning, DesiredStateJSON: string(desired)}, "Ampere")
	}
	if callable, vllm := claim(deploy.EngineCallable), claim(deploy.EngineVLLM); callable != vllm {
		t.Fatalf("callable claim = %d, want the default engine's %d", callable, vllm)
	}
	if got := claim(deploy.EngineCallable); got <= int64(8)<<30 {
		t.Fatalf("callable claim = %d, want the engine reserve charged on top of the limit", got)
	}
}

// The ledger charges a running NVIDIA dGPU instance what admission charged it:
// the limit plus the context HAMi cannot see, read from the instance's own
// stored backend. A spec from before the backend was stored, or a backend
// whose overhead nobody measured, keeps the conservative whole-card reserve.
func TestLedgerChargesADGPUInstanceItsBackendOverhead(t *testing.T) {
	st, err := store.Open("file:planning_dgpu_claim?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const limit = int64(8) << 30
	claim := func(backend string) int64 {
		desired, err := json.Marshal(map[string]any{"engine": deploy.EngineVLLM, "vram_limit_bytes": limit, "partition_backend": backend})
		if err != nil {
			t.Fatal(err)
		}
		return instanceClaim(st, &store.Instance{UUID: "dgpu-" + backend, State: proto.InstRunning, DesiredStateJSON: string(desired)}, "Turing")
	}
	if got := claim(accelerator.BackendNVIDIALogical); got != limit+512<<20 {
		t.Fatalf("nvidia claim = %d, want the limit plus 512 MiB", got)
	}
	for _, backend := range []string{"", accelerator.BackendAscendLogical} {
		if got := claim(backend); got <= limit+512<<20 {
			t.Fatalf("backend %q claim = %d, want the whole-card reserve on top of the limit", backend, got)
		}
	}
}

// KV capacity is advisory even if the estimate says no full-context request
// fits. Startup weights and resource admission still have their own refusals.
func TestEvaluateWarnsWhenEstimatedKVPoolCannotHoldContext(t *testing.T) {
	st, err := store.Open("file:planning_kv_below_context?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("kv-node", store.HashToken("token"), "kv-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 24 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	model := &store.Model{ID: 1, Name: "kv-model", WeightBytes: 1 << 30}
	evaluate := func(weight int64, maxLen int) Output {
		quick := launchConfig(t, deploy.EngineVLLM)
		quick.MaxModelLen = maxLen
		return (Input{Model: &store.Model{ID: model.ID, Name: model.Name, WeightBytes: weight}, Node: node, Engine: deploy.EngineVLLM, Quick: quick, KVBytesPerToken: 114688}).Evaluate(st)
	}
	// 262144 is the context Qwen3-VL-Embedding-2B launches with when the form
	// leaves max model len unset. Startup memory fits on 24 GiB; one request does not.
	long := evaluate(1<<30, 262144)
	if !long.Result.OK || long.Result.Plan == nil || !long.Breakdown.Fits {
		t.Fatalf("estimated KV shortfall blocked startup: ok=%v plan=%v fits=%v", long.Result.OK, long.Result.Plan != nil, long.Breakdown.Fits)
	}
	if long.Breakdown.MaxModelLen != 262144 || long.Breakdown.MaxConcurrency == nil || *long.Breakdown.MaxConcurrency != 0 {
		t.Fatalf("context was clamped or estimated zero hidden: %+v", long.Breakdown)
	}
	var kvWarnings int
	for _, warning := range long.Warnings {
		if warning.Code == messages.WarningPlanningKVBelowContext {
			kvWarnings++
			if warning.Params["kv_tokens"] != long.Breakdown.EstimatedKVTokens || warning.Params["max_model_len"] != 262144 {
				t.Fatalf("warning = %+v, breakdown = %+v", warning, long.Breakdown)
			}
		}
	}
	if kvWarnings != 1 {
		t.Fatalf("want exactly one KV warning: %+v", long.Warnings)
	}
	short := evaluate(1<<30, 1024)
	if !short.Result.OK || !short.Breakdown.Fits {
		t.Fatalf("a context the pool holds was refused: %+v fits=%v", short.Result.Message, short.Breakdown.Fits)
	}
	for _, warning := range short.Warnings {
		if warning.Code == messages.WarningPlanningKVBelowContext {
			t.Fatalf("short context should not warn: %+v", warning)
		}
	}
	// Native fit may shorten the model ceiling. Keep that ceiling as an
	// estimate, but do not warn as if it were a fixed launch requirement.
	autoQuick := launchConfig(t, deploy.EngineVLLM)
	autoQuick.MaxModelLen = -1
	auto := (Input{
		Model: &store.Model{ID: model.ID, Name: model.Name, WeightBytes: 1 << 30, Meta: `{"max_position_embeddings":262144}`},
		Node:  node, Engine: deploy.EngineVLLM, Quick: autoQuick, KVBytesPerToken: 114688,
	}).Evaluate(st)
	if !auto.Result.OK || auto.Breakdown.MaxModelLen != 262144 || !auto.Breakdown.ContextExceedsKVPool() {
		t.Fatalf("auto lost model ceiling or startup admission: %+v", auto)
	}
	for _, warning := range auto.Warnings {
		if warning.Code == messages.WarningPlanningKVBelowContext {
			t.Fatalf("native auto was treated as a fixed context: %+v", warning)
		}
	}
	unknown := (Input{Model: model, Node: node, Engine: deploy.EngineVLLM, Quick: autoQuick, KVBytesPerToken: 114688}).Evaluate(st)
	if unknown.Breakdown.MaxModelLen != 0 {
		t.Fatalf("unknown model window became a negative token count: %+v", unknown.Breakdown)
	}
	heavy := evaluate(40<<30, 40960)
	if heavy.Result.OK || heavy.Result.Message.Code != scheduler.ReasonNoFit {
		t.Fatalf("weight shortfall = %+v, want %s", heavy.Result.Message, scheduler.ReasonNoFit)
	}
}

func TestEvaluateBreakdownMatchesSchedulerMemoryDecision(t *testing.T) {
	st, err := store.Open("file:planning_single_source_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("single-source-node", store.HashToken("token"), "single-source-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 16 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	for _, weight := range []int64{8 << 30, 40 << 30} {
		model := &store.Model{ID: uint(weight >> 30), Name: "single-source", WeightBytes: weight}
		out := (Input{Model: model, Node: node, Engine: "vllm", Quick: launchConfig(t, "vllm")}).Evaluate(st)
		if out.Result.OK != out.Breakdown.Fits {
			t.Fatalf("weight=%d: schedule ok=%v but breakdown fits=%v: result=%+v breakdown=%+v", weight, out.Result.OK, out.Breakdown.Fits, out.Result, out.Breakdown)
		}
		if out.Result.Plan != nil && out.Result.Plan.PerGPUBytes != out.Breakdown.PerGPUBytes {
			t.Fatalf("weight=%d: scheduler requires %d but breakdown reports %d", weight, out.Result.Plan.PerGPUBytes, out.Breakdown.PerGPUBytes)
		}
	}
}

// The estimator reads the weight total from the model row and falls back to the
// catalog spec's figure for a legacy row that has none - and it does not write
// the fallback back: persisting it belongs to the caller that knows both (the
// evaluate handler), so planning stays a reader.
func TestEvaluateFallsBackToSpecWeightBytesWithoutWriting(t *testing.T) {
	st, err := store.Open("file:planning_spec_weight_fallback?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/spec", Revision: "main", Name: "spec", WeightBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("spec-weight-node", store.HashToken("token"), "spec-weight-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 16 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	out := (Input{Model: model, SpecWeightBytes: 4 << 30, Node: node, Engine: "vllm", Quick: launchConfig(t, "vllm")}).Evaluate(st)
	if out.Breakdown.WeightBytes != 4<<30 {
		t.Fatalf("breakdown=%+v", out.Breakdown)
	}
	stored, _ := st.GetModel(model.ID)
	if stored.WeightBytes != 0 {
		t.Fatalf("planning wrote the fallback back: stored weight=%d", stored.WeightBytes)
	}
}

func TestModelDiskNeedTracksAbsentPartialAndPresentArtifacts(t *testing.T) {
	st, err := store.Open("file:planning_incremental_disk?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/incremental", Revision: "main", Name: "incremental", WeightBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("incremental-node", store.HashToken("token"), "incremental-node")
	if err != nil {
		t.Fatal(err)
	}
	if got := modelDiskNeed(st, model, node, 0, 1000); got != 1000 {
		t.Fatalf("absent need=%d, want 1000", got)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFDownloading, Downloaded: 375}); err != nil {
		t.Fatal(err)
	}
	if got := modelDiskNeed(st, model, node, 0, 1000); got != 625 {
		t.Fatalf("partial need=%d, want 625", got)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, SizeBytes: 1000, Downloaded: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := modelDiskNeed(st, model, node, 0, 1000); got != 0 {
		t.Fatalf("present need=%d, want 0", got)
	}
}

func TestComponentDiskNeedTracksEachArtifact(t *testing.T) {
	st, err := store.Open("file:planning_component_incremental_disk?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("component-disk-node", store.HashToken("token"), "component-disk-node")
	if err != nil {
		t.Fatal(err)
	}
	present, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/present-component", Revision: "main", Name: "present"})
	partial, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/partial-component", Revision: "main", Name: "partial"})
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: present.ID, NodeID: node.ID, State: proto.MFPresent, Downloaded: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: partial.ID, NodeID: node.ID, State: proto.MFSyncing, Downloaded: 25}); err != nil {
		t.Fatal(err)
	}
	artifacts := []Artifact{{ModelID: present.ID, StorageBytes: 100}, {ModelID: partial.ID, StorageBytes: 100}, {ModelID: 999999, StorageBytes: 100}}
	if got := componentDiskNeed(st, node, 0, 9999, artifacts); got != 175 {
		t.Fatalf("component disk need=%d, want partial 75 + absent 100", got)
	}
}

func TestEvaluateUsesSelectedRuntimeLayoutButFullRepositoryStorage(t *testing.T) {
	st, err := store.Open("file:planning_mistral_layout?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const repositoryBytes = int64(35_706_509_936)
	const runtimeBytes = int64(17_836_115_976)
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "mistralai/Ministral-3-8B-Reasoning-2512", Revision: "fixture", Name: "ministral", WeightBytes: repositoryBytes})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("mistral-layout-node", store.HashToken("token"), "mistral-layout-node")
	if err != nil {
		t.Fatal(err)
	}
	node.PathUsage = `[{"kind":"models","available_bytes":1073741824}]`
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 40 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, SizeBytes: repositoryBytes, Downloaded: repositoryBytes}); err != nil {
		t.Fatal(err)
	}
	out := (Input{Model: model, RuntimeWeightBytes: runtimeBytes, Node: node, Engine: "vllm", Quick: deploy.LaunchConfig{GPUMemUtil: 0.8}}).Evaluate(st)
	if !out.Result.OK {
		t.Fatalf("selected Mistral layout should fit A100 at 0.8: %+v", out.Result)
	}
	if out.Breakdown.WeightBytes != runtimeBytes {
		t.Fatalf("runtime weight=%d, want %d", out.Breakdown.WeightBytes, runtimeBytes)
	}

	if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ? AND node_id = ?", model.ID, node.ID).Updates(map[string]any{"state": proto.MFDownloading, "downloaded": repositoryBytes - 2_000_000_000}).Error; err != nil {
		t.Fatal(err)
	}
	out = (Input{Model: model, RuntimeWeightBytes: runtimeBytes, Node: node, Engine: "vllm", Quick: deploy.LaunchConfig{GPUMemUtil: 0.8}}).Evaluate(st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("remaining repository bytes should fail 1 GiB model disk: %+v", out.Result)
	}
}

func TestEvaluateUsesOptInCatalogResourceContractWithoutChangingArtifactBytes(t *testing.T) {
	st, err := store.Open("file:planning_catalog_resource_contract?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("catalog-resource-node", store.HashToken("token"), "catalog-resource-node")
	if err != nil {
		t.Fatal(err)
	}
	node.DiskTotalKB = 1 << 30
	for index := range 4 {
		node.GPUs = append(node.GPUs, store.GPU{Index: index, MemoryTotal: 32 << 30, Vendor: "ascend", ArchID: "910B4"})
	}
	const resolver = `def resolve(gpu):
    if gpu.count != 4 or gpu.min_memory_gib < 32:
        return reject("requires four 32 GiB GPUs")
    return accept(gpu_memory_mib_per_device=29424, variables={"GPU_COUNT": 4, "TP_SIZE": 4, "SP_DEGREE": 1})`
	model := &store.Model{Name: "catalog-resource", WeightBytes: 144051182625}
	quick := launchConfig(t, "sglang")
	// The contract claims 29424 MiB of a 32 GiB card, which is more than
	// SGLang's dictionary default (0.88) leaves available, so a deployment like
	// this states its own utilization.
	quick.GPUMemUtil = 0.92
	out := (Input{Model: model, Node: node, Engine: "sglang", PlacementResolver: resolver, Quick: quick}).Evaluate(st)
	if !out.Result.OK || out.Result.Plan == nil || out.Result.Plan.TP != 4 {
		t.Fatalf("catalog resource placement=%+v err=%v", out.Result, out.PlacementError)
	}
	if out.Breakdown.WeightBytes != model.WeightBytes {
		t.Fatalf("artifact bytes changed from %d to %d", model.WeightBytes, out.Breakdown.WeightBytes)
	}
	if out.Breakdown.PerGPUBytes != 29424<<20 || out.Breakdown.Source != "catalog" {
		t.Fatalf("catalog runtime breakdown=%+v", out.Breakdown)
	}
	if out.Placement.Variables["TP_SIZE"] != int64(4) {
		t.Fatalf("placement variables=%#v", out.Placement.Variables)
	}
}

func TestNodeSnapshotPreservesGPUHealth(t *testing.T) {
	st, err := store.Open("file:planning_gpu_health?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-health", store.HashToken("token"), "node-health")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{Index: 4, MemoryTotal: 32 << 30, Vendor: "ascend", ArchID: "910B4", HealthStatus: "Error"},
		{Index: 5, MemoryTotal: 32 << 30, Vendor: "ascend", ArchID: "910B4", HealthStatus: "OK"},
		{Index: 6, MemoryTotal: 32 << 30, Vendor: "ascend", ArchID: "910B4", HealthStatus: "Warning"},
	}
	snap := NodeSnapshot(st, node, true, "")
	if len(snap.GPUs) != 3 || snap.GPUs[0].Schedulable || !snap.GPUs[1].Schedulable || snap.GPUs[1].Degraded ||
		!snap.GPUs[2].Schedulable || !snap.GPUs[2].Degraded {
		t.Fatalf("snapshot GPUs = %+v", snap.GPUs)
	}
}

func TestRemovingInstanceStillClaimsItsGPUQuota(t *testing.T) {
	st, err := store.Open("file:planning_removing_claim?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("removing-claim", store.HashToken("token"), "removing-claim")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 3, NodeID: node.ID, MemoryTotal: 32 << 30, Vendor: "nvidia"}}
	dep, err := st.CreateDeployment(&store.Deployment{NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateInstance(&store.Instance{UUID: "removing-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRemoving, GPUIndexes: "[3]", DesiredStateJSON: `{"engine":"vllm","vram_limit_bytes":8589934592}`})
	if err != nil {
		t.Fatal(err)
	}
	got := GPUAllocations(st, node)[3]
	if got.AllocatedBytes < 8<<30 || len(got.Instances) != 1 {
		t.Fatalf("removing instance lost its GPU claim: %+v", got)
	}
}

func TestNodeSnapshotKeepsPartitionReadinessOnItsPhysicalGPU(t *testing.T) {
	st, err := store.Open("file:planning_per_gpu_partition?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("per-gpu-partition", store.HashToken("token"), "per-gpu-partition")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUVendor, node.RuntimeDriver = "ascend", "docker"
	node.PartitionAssets = `[{"backend":"ascend-dgpu","root":"/assets","files":["libvruntime.so"]}]`
	node.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"passed"},{"backend":"ascend-dgpu","gpu_index":3,"status":"failed"}]`
	node.GPUs = []store.GPU{
		{Index: 0, NodeID: node.ID, MemoryTotal: 32 << 30, Vendor: "ascend"},
		{Index: 3, NodeID: node.ID, MemoryTotal: 32 << 30, Vendor: "ascend"},
	}
	snap := NodeSnapshot(st, node, true, "")
	if len(snap.GPUs) != 2 || len(snap.GPUs[0].PartitionBackends) != 1 || len(snap.GPUs[1].PartitionBackends) != 0 {
		t.Fatalf("partition readiness was promoted to node-wide: %+v", snap.GPUs)
	}
}

func TestNodeSnapshotUsesModelsFilesystemAvailability(t *testing.T) {
	node := &store.Node{DiskTotalKB: 1 << 30, PathUsage: `[{
        "kind":"models", "path":"/models", "total_bytes":100000, "available_bytes":42000
    }]`}
	if got := modelDiskAvailable(node); got != 42000 {
		t.Fatalf("disk availability = %d, want models filesystem value", got)
	}
	node.PathUsage = "not-json"
	if got := modelDiskAvailable(node); got != 0 {
		t.Fatalf("invalid path usage = %d, want unknown (0)", got)
	}
}

func TestNodeSnapshotUsesSelectedRuntimeFilesystemAvailability(t *testing.T) {
	node := &store.Node{RuntimeDriver: "udocker", PathUsage: `[
        {"kind":"models","available_bytes":9000},
        {"kind":"docker","available_bytes":8000},
        {"kind":"udocker","available_bytes":7000},
        {"kind":"host","available_bytes":6000}
    ]`}
	if got := runtimeDiskAvailable(node); got != 7000 {
		t.Fatalf("udocker availability=%d, want 7000", got)
	}
	node.RuntimeDriver = "docker"
	if got := runtimeDiskAvailable(node); got != 8000 {
		t.Fatalf("docker availability=%d, want 8000", got)
	}
	node.RuntimeDriver = "host"
	if got := runtimeDiskAvailable(node); got != 6000 {
		t.Fatalf("host availability=%d, want 6000", got)
	}
	node.RuntimeDriver = "nerdctl"
	if got := runtimeDiskAvailable(node); got != 0 {
		t.Fatalf("unreported nerdctl availability=%d, want unknown", got)
	}
}

func TestCatalogImageBytesUsesUnpackedCatalogSize(t *testing.T) {
	st, err := store.Open("file:planning_image_bytes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "registry.example/runner:v1", Engine: "vllm", SizeBytes: 12345, OfflineBlobBytes: 4567})
	if err != nil || image == nil {
		t.Fatal(err)
	}
	if got := CatalogImageBytes(st, image.ImageRef); got != 12345 {
		t.Fatalf("catalog image bytes=%d, want unpacked size 12345", got)
	}
	if got := CatalogImageBytes(st, "registry.example/missing:v1"); got != 0 {
		t.Fatalf("missing image bytes=%d, want unknown", got)
	}
}

func TestEvaluateCachedImageDoesNotReserveRuntimeDisk(t *testing.T) {
	st, err := store.Open("file:planning_cached_image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("cached-image", store.HashToken("token"), "cached-image")
	if err != nil {
		t.Fatal(err)
	}
	node.RuntimeDriver = "docker"
	node.PathUsage = `[{"kind":"docker","path":"/var/lib/docker","available_bytes":1,"total_bytes":2000000000}]`
	node.GPUs = []store.GPU{{Index: 0, Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 80 << 30}}
	cached := true
	out := (Input{Node: node, Model: &store.Model{WeightBytes: 0}, Engine: "vllm", RuntimeImageBytes: 8 << 30, RuntimeImageCached: &cached, Quick: launchConfig(t, "vllm")}).Evaluate(st)
	if !out.Result.OK {
		t.Fatalf("cached image unexpectedly rejected: %+v", out.Result.Message)
	}
	cached = false
	out = (Input{Node: node, Model: &store.Model{}, Engine: "vllm", RuntimeImageBytes: 8 << 30, RuntimeImageCached: &cached, Quick: launchConfig(t, "vllm")}).Evaluate(st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientRuntimeDisk {
		t.Fatalf("uncached image result=%+v", out.Result)
	}
	unknown := (Input{Node: node, Model: &store.Model{}, Engine: "vllm", RuntimeImageBytes: 8 << 30, Quick: launchConfig(t, "vllm")}).Evaluate(st)
	if unknown.Result.OK || unknown.Result.Message.Code != scheduler.ReasonInsufficientRuntimeDisk {
		t.Fatalf("unknown image result=%+v", unknown.Result)
	}
}

func TestNodeGPUProfilePrefersHealthyGPU(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{
		{Index: 4, Vendor: "ascend", ArchID: "faulty", HealthStatus: accelerator.HealthError},
		{Index: 5, Vendor: "ascend", ArchID: "910B4"},
	}}
	vendor, archID, arch, _ := nodeGPUProfile(node)
	if vendor != "ascend" || archID != "910B4" || arch != "910b" {
		t.Fatalf("profile = %q/%q/%q", vendor, archID, arch)
	}
}

func TestEvaluateSuggestsSleepingInstanceWhenMemoryIsInsufficient(t *testing.T) {
	st, err := store.Open("file:planning_eviction_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("evict-node", store.HashToken("token"), "evict-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "sleepable-a", ModelID: 1, NodeID: node.ID, ServedName: "a", Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "sleep-a", DeploymentID: dep.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("sleep-a", 0)}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	suggestions := suggestEviction(st, node, req, ledgerView{})
	// Sleeping releases the whole card down to the residue the sleeper keeps.
	wantFrees := float64((16<<30)-sleep.UnknownResidueBytes) / (1 << 30)
	if len(suggestions) != 1 || suggestions[0].InstanceUUID != "sleep-a" || suggestions[0].Deployment != "sleepable-a" || suggestions[0].FreesGiB != wantFrees {
		t.Fatalf("eviction suggestions = %+v, want frees %.1f", suggestions, wantFrees)
	}
	if suggestions[0].GPUIndexes[0] != 0 {
		t.Fatalf("eviction GPU indexes = %+v", suggestions[0].GPUIndexes)
	}

	// Once the instance is asleep it is no longer a candidate - and the card it
	// released is available, so the request now fits outright.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "sleep-a", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentSleep(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := suggestEviction(st, node, req, ledgerView{}); got != nil {
		t.Fatalf("already-sleeping instance suggested again: %+v", got)
	}
	snap := NodeSnapshot(st, node, true, "")
	if snap.GPUs[0].AllocatedBytes != 0 || snap.GPUs[0].SleepingBytes != sleep.UnknownResidueBytes {
		t.Fatalf("ledger for a sleeping whole-GPU instance = %+v, want residue only", snap.GPUs[0])
	}
	if res := scheduler.Schedule(req, snap); !res.OK {
		t.Fatalf("request no longer fits after the sleeper released the card: %+v", res.Message)
	}
	allocation := GPUAllocations(st, node)[0]
	if allocation.SleepingBytes != sleep.UnknownResidueBytes || len(allocation.Instances) != 1 {
		t.Fatalf("GPUAllocations = %+v", allocation)
	}
}

func TestWakingInstanceIsChargedItsFullClaimBeforeItReportsAwake(t *testing.T) {
	st, err := store.Open("file:planning_waking_claim_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("waking-claim-node", store.HashToken("token"), "waking-claim-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	if err := st.DB().Create(&node.GPUs[0]).Error; err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "waking", ModelID: 1, NodeID: node.ID, ServedName: "waking", Engine: "vllm", Sleepable: true, State: "active", DesiredSleep: 0})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "waking-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", SleepLevel: 1, DesiredStateJSON: sleepSpec("waking-1", 0)})
	if err != nil {
		t.Fatal(err)
	}
	allocation := GPUAllocations(st, node)[0]
	if allocation.AllocatedBytes != node.GPUs[0].MemoryTotal || allocation.SleepingBytes != 0 {
		t.Fatalf("waking allocation = %+v, want full claim and no residue", allocation)
	}
	snapshot := NodeSnapshot(st, node, true, "")
	if result := scheduler.Schedule(scheduler.Request{WeightBytes: 1 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}, snapshot); result.OK {
		t.Fatalf("scheduler admitted a request while wake was in flight: %+v", result)
	}
	if err := st.SetDeploymentSleep(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	_ = inst
	allocation = GPUAllocations(st, node)[0]
	if allocation.AllocatedBytes != 0 || allocation.SleepingBytes != sleep.UnknownResidueBytes {
		t.Fatalf("sleeping allocation = %+v, want residue only", allocation)
	}
}

func TestFailedInstanceLedgerMatchesMasterTerminalRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase string
		want  bool
	}{
		{name: "ordinary failure is released", phase: "pull", want: false},
		{name: "sleep failure retains claim", phase: proto.PhaseSleep, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:planning_failed_ledger_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			node, err := st.RegisterNode("failed-ledger-node", store.HashToken("token"), "failed-ledger-node")
			if err != nil {
				t.Fatal(err)
			}
			node.GPUs = []store.GPU{{Index: 0, UUID: "failed-ledger-gpu", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
			if err := st.DB().Create(&node.GPUs[0]).Error; err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "failed-ledger", ModelID: 1, NodeID: node.ID, ServedName: "failed-ledger", Engine: "vllm", State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "failed-ledger-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstFailed, Phase: tc.phase, GPUIndexes: "[0]"}); err != nil {
				t.Fatal(err)
			}
			allocation := GPUAllocations(st, node)[0]
			if (allocation.AllocatedBytes > 0) != tc.want {
				t.Fatalf("failed %s allocation = %+v, retained=%v", tc.phase, allocation, tc.want)
			}
		})
	}
}

func TestWakeIsRefusedWhileAnAwakeCoTenantHoldsTheCard(t *testing.T) {
	st, err := store.Open("file:planning_wake_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("wake-node", store.HashToken("token"), "wake-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	sleeper, err := st.CreateDeployment(&store.Deployment{Name: "sleeper", ModelID: 1, NodeID: node.ID, ServedName: "a", Engine: "vllm", Sleepable: true, State: "active", DesiredSleep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "sleeper-1", DeploymentID: sleeper.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", SleepLevel: 1, DesiredStateJSON: sleepSpec("sleeper-1", 1)}); err != nil {
		t.Fatal(err)
	}
	insts, _ := st.InstancesForDeployment(sleeper.ID)
	if _, refused, err := AdmitWake(st, [][]store.Instance{insts}, false); err != nil || len(refused) != 0 {
		t.Fatalf("lone sleeper refused its own card: %+v %v", refused, err)
	}

	newcomer, err := st.CreateDeployment(&store.Deployment{Name: "newcomer", ModelID: 2, NodeID: node.ID, ServedName: "b", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "newcomer-1", DeploymentID: newcomer.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
	admitted, refused, err := AdmitWake(st, [][]store.Instance{insts}, false)
	if err != nil || len(admitted) != 0 || len(refused) != 1 || len(refused[0].Verdict.Shortfalls) != 1 {
		t.Fatalf("wake admitted over an awake whole-GPU co-tenant: %+v %+v %v", admitted, refused, err)
	}
	v := refused[0].Verdict
	s := v.Shortfalls[0]
	wantNeed := int64(float64(16<<30) * deploy.DefaultGPUMemUtil("vllm"))
	if s.GPUIndex != 0 || s.NeedBytes != wantNeed || s.FreeBytes != 0 || len(s.Occupants) != 1 || s.Occupants[0] != "newcomer" {
		t.Fatalf("shortfall = %+v, want need %d on gpu 0 held by newcomer", s, wantNeed)
	}
}

func TestEvaluateRejectsManualTPThatDoesNotDivideAttentionHeads(t *testing.T) {
	st, err := store.Open("file:planning_tp_reject_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("tp-node", store.HashToken("token"), "tp-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
		{Index: 1, UUID: "gpu-1", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
		{Index: 2, UUID: "gpu-2", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
		{Index: 3, UUID: "gpu-3", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
	}
	model := &store.Model{ID: 1, Name: "six-head-model", Arch: "qwen", WeightBytes: 1 << 30}
	config, _ := json.Marshal(map[string]any{"num_attention_heads": 6, "max_position_embeddings": 4096, "hidden_size": 4096, "num_hidden_layers": 2})
	model.Meta = string(config)
	out := (Input{Model: model, Node: node, Engine: "vllm", Parallel: parallelism.Spec{TP: 4}, Quick: launchConfig(t, "vllm")}).Evaluate(st)
	if out.Result.OK {
		t.Fatalf("manual tp unexpectedly accepted: %+v", out.Result)
	}
	found := false
	for _, rejected := range out.Result.Explain.Rejected {
		if rejected.TP == 4 && rejected.Reason.Code == scheduler.ReasonTPHeadCountNotDivisible && rejected.Reason.Params["tp"] == 4 && rejected.Reason.Params["head_count"] == 6 {
			found = true
		}
	}
	if !found {
		t.Fatalf("tp rejection reasons = %+v", out.Result.Explain.Rejected)
	}
}

// launchConfig is the complete launch configuration a deployment carries by the
// time planning sees it. Materialization fills it at the write boundary, so a
// fixture that left the utilization unset would be describing a request the
// server cannot produce - and one admission now rejects outright.
func launchConfig(t *testing.T, engine string) deploy.LaunchConfig {
	t.Helper()
	out, err := deploy.Materialize(deploy.MaterializeInput{Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	return out.Quick
}

// sleepSpec is an instance's desired state carrying a sleep intent, as the
// controller launches it and the store's sleep orders rewrite it.
func sleepSpec(uuid string, level int) string {
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, DesiredSleep: level})
	return string(raw)
}

// One replica ordered to sleep on its own gives its card up down to the
// residue once it reports asleep, while the deployment as a whole is not
// under a sleep order; the eviction check no longer suggests it.
func TestAReplicaSleepingAloneIsChargedItsResidue(t *testing.T) {
	st, err := store.Open("file:planning_replica_sleep_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("replica-sleep-node", store.HashToken("token"), "replica-sleep-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
		{Index: 1, UUID: "gpu-1", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "replicas", ModelID: 1, NodeID: node.ID, ServedName: "replicas", Engine: "vllm", Sleepable: true, Replicas: 2, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"replica-0", "replica-1"} {
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", i), DesiredStateJSON: sleepSpec(uuid, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-0"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, node)[0]; got.SleepingBytes != 0 {
		t.Fatalf("an ordered sleep not yet reported was charged the residue: %+v", got)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "replica-0", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	allocations := GPUAllocations(st, node)
	if allocations[0].AllocatedBytes != 0 || allocations[0].SleepingBytes != sleep.UnknownResidueBytes {
		t.Fatalf("sleeping replica's card = %+v, want residue only", allocations[0])
	}
	if allocations[1].AllocatedBytes != 16<<30 {
		t.Fatalf("awake replica's card = %+v, want its full claim", allocations[1])
	}
	req := scheduler.Request{WeightBytes: 30 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	for _, suggestion := range suggestEviction(st, node, req, ledgerView{}) {
		if suggestion.InstanceUUID == "replica-0" {
			t.Fatalf("the sleeping replica was suggested for eviction: %+v", suggestion)
		}
	}
}

// A tiered engine declares its per-card admission in the resolver. When the
// scheduler refuses the node (here every card is unhealthy) the breakdown is
// still the contract, not an estimate from 63 GiB of weights that contradicts
// the refusal it sits beside; a resolver that rejects the cards declares none.
func TestRefusedEvaluationStillReportsTheTemplateContract(t *testing.T) {
	st, err := store.Open("file:planning_refused_contract?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("refused-contract", store.HashToken("token"), "refused-contract")
	if err != nil {
		t.Fatal(err)
	}
	node.DiskTotalKB = 1 << 30
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 48 << 30, HealthStatus: accelerator.HealthError, Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6"}}
	const resolver = `def resolve(gpu):
    if gpu.min_memory_gib < 12:
        return reject("Strata requires at least 12 GiB VRAM on every card")
    return accept(gpu_memory_mib_per_device=12288)`
	model := &store.Model{Name: "tiered", WeightBytes: 68026093024}
	quick := launchConfig(t, "vllm")
	out := (Input{Model: model, Node: node, Engine: "vllm", PlacementResolver: resolver, Quick: quick}).Evaluate(st)
	if out.Result.OK {
		t.Fatalf("a node with no healthy card was accepted: %+v", out.Result)
	}
	if out.Breakdown.PerGPUBytes != 12288<<20 || out.Breakdown.Source != "catalog" {
		t.Fatalf("breakdown of the refused evaluation = per GPU %d, source %q", out.Breakdown.PerGPUBytes, out.Breakdown.Source)
	}
	node.GPUs[0].MemoryTotal = 10 << 30
	out = (Input{Model: model, Node: node, Engine: "vllm", PlacementResolver: resolver, Quick: quick}).Evaluate(st)
	if out.Breakdown.Source == "catalog" {
		t.Fatalf("a resolver that rejects the cards declared a contract: %+v", out.Breakdown)
	}
}

// A Mac takes an ordinary deployment but not one that needs engine sleep, a
// KV disk tier or ranks on several nodes; the same requests fit an NVIDIA card.
func TestEvaluateRefusesFeaturesTheNodesVendorLacks(t *testing.T) {
	st, err := store.Open("file:planning_vendor_features?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mac, err := st.RegisterNode("mac", store.HashToken("mac-token"), "mac")
	if err != nil {
		t.Fatal(err)
	}
	mac.GPUs = []store.GPU{{Index: 0, Name: "M1 Max", MemoryTotal: 48 << 30, Vendor: "apple", ArchID: "M1 Max"}}
	model := &store.Model{ID: 1, Name: "m", WeightBytes: 1 << 30}
	plain := launchConfig(t, deploy.EngineVLLM)
	if out := (Input{Model: model, Node: mac, Engine: deploy.EngineVLLM, Quick: plain}).Evaluate(st); !out.Result.OK {
		t.Fatalf("a plain deployment does not fit a Mac: %+v", out.Result)
	}
	kvDisk := plain
	kvDisk.KVDiskOffload = true
	kvDisk.KVOffloadingSize = 4
	for name, tc := range map[string]struct {
		in   Input
		code string
	}{
		"sleep":      {Input{Sleepable: true, Quick: plain}, scheduler.ReasonSleepUnsupportedOnNode},
		"kv disk":    {Input{Quick: kvDisk}, scheduler.ReasonKVDiskUnsupportedOnNode},
		"multi-node": {Input{Quick: plain, Parallel: parallelism.Spec{TP: 1, PP: 2, NNodes: 2}}, scheduler.ReasonMultiNodeUnsupportedOnNode},
	} {
		in := tc.in
		in.Model, in.Node, in.Engine = model, mac, deploy.EngineVLLM
		out := in.Evaluate(st)
		if out.Result.OK || out.Result.Message.Code != tc.code {
			t.Errorf("%s on a Mac: %+v", name, out.Result)
		}
	}
	// Every snapshot carries the vendor, not only Evaluate's: the eviction
	// re-run schedules from a bare snapshot.
	if snap := NodeSnapshot(st, mac, true, ""); snap.Vendor != "apple" {
		t.Fatalf("snapshot vendor = %q", snap.Vendor)
	}
	got := deploymentFeatures(Input{Sleepable: true, PD: true, Quick: kvDisk, Parallel: parallelism.Spec{TP: 1, NNodes: 2}, PartitionBackend: "mig"})
	want := []accelerator.Feature{accelerator.FeatureEngineSleep, accelerator.FeatureMultiNode, accelerator.FeaturePD, accelerator.FeatureKVDiskTier}
	if len(got) != len(want) {
		t.Fatalf("deploymentFeatures = %v, want all explicitly requested features %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deploymentFeatures = %v, want %v", got, want)
		}
	}
}

// A Mac's GPU is shared by memory: the ledger charges each instance the claim
// it was admitted with, and a new deployment fits beside them while the claims
// leave room for its own.
func TestAMacHostsSeveralInstancesByTheirMemoryClaims(t *testing.T) {
	st, err := store.Open("file:planning_mac_shared_by_memory?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mac, err := st.RegisterNode("mac-shared", store.HashToken("mac-shared"), "mac-shared")
	if err != nil {
		t.Fatal(err)
	}
	mac.GPUs = []store.GPU{{Index: 0, NodeID: mac.ID, Name: "M4 Max", MemoryTotal: 96 << 30, Vendor: "apple", ArchID: "M4 Max"}}
	dep, err := st.CreateDeployment(&store.Deployment{NodeID: mac.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "llm", DeploymentID: dep.ID, NodeID: mac.ID, State: proto.InstRunning, GPUIndexes: "[0]",
		DesiredStateJSON: `{"engine":"vllm","memory_claim_bytes":` + fmt.Sprint(int64(40)<<30) + `}`}); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, mac)[0]; got.AllocatedBytes != 40<<30 {
		t.Fatalf("allocation = %+v, want the 40 GiB claim", got)
	}
	// A row written before claims were recorded has none: it is charged the
	// whole GPU, so nothing is admitted beside an engine of unknown size.
	if _, err := st.CreateInstance(&store.Instance{UUID: "old", DeploymentID: dep.ID, NodeID: mac.ID, State: proto.InstRunning, GPUIndexes: "[0]",
		DesiredStateJSON: `{"engine":"vllm"}`}); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, mac)[0]; got.AllocatedBytes != 96<<30 {
		t.Fatalf("allocation with an unclaimed row = %+v, want the whole GPU", got)
	}
	if err := st.DeleteInstance("old"); err != nil {
		t.Fatal(err)
	}
	snap := NodeSnapshot(st, mac, true, "")
	if !snap.GPUs[0].SharedByMemory {
		t.Fatal("a Mac's GPU is not shared by memory")
	}

	model := &store.Model{ID: 1, Name: "m", WeightBytes: 4 << 30}
	quick := launchConfig(t, deploy.EngineVLLM)
	quick.GPUMemUtil = 0.5
	out := (Input{Model: model, Node: mac, Engine: deploy.EngineVLLM, Quick: quick}).Evaluate(st)
	if !out.Result.OK || out.Result.Plan.ClaimBytes != 48<<30 {
		t.Fatalf("a 0.5 vLLM beside a 40 GiB claim: %+v", out.Result)
	}
	quick.GPUMemUtil = 0.7
	if out := (Input{Model: model, Node: mac, Engine: deploy.EngineVLLM, Quick: quick}).Evaluate(st); out.Result.OK {
		t.Fatalf("a 0.7 vLLM (67 GiB) was let in beside a 40 GiB claim: %+v", out.Result.Plan)
	}
	// A callable server takes what it needs, whatever its utilization says.
	out = (Input{Model: model, Node: mac, Engine: deploy.EngineCallable, Quick: launchConfig(t, deploy.EngineCallable)}).Evaluate(st)
	if !out.Result.OK || out.Result.Plan.ClaimBytes <= 0 || out.Result.Plan.ClaimBytes > 16<<30 {
		t.Fatalf("callable beside a 40 GiB claim: %+v", out.Result.Plan)
	}
}

// An edit of a deployment is previewed as the launch that replaces its
// instances: their claims leave the ledger, and so do they as sleep
// candidates. Every other deployment's claims stay.
func TestLedgerViewExcludesTheEditedDeployment(t *testing.T) {
	st, err := store.Open("file:planning_exclude_deployment?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("exclude-dep", store.HashToken("token"), "exclude-dep")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{Index: 0, UUID: "gpu-0", NodeID: node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
		{Index: 1, UUID: "gpu-1", NodeID: node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30},
	}
	edited, err := st.CreateDeployment(&store.Deployment{Name: "edited", NodeID: node.ID, Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "other", NodeID: node.ID, Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "edited-0", DeploymentID: edited.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("edited-0", 0)},
		{UUID: "other-0", DeploymentID: other.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", DesiredStateJSON: `{"engine":"vllm"}`},
	} {
		if _, err := st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	plain := nodeSnapshot(st, node, true, "", ledgerView{})
	if plain.GPUs[0].AllocatedBytes == 0 || plain.GPUs[1].AllocatedBytes == 0 {
		t.Fatalf("plain ledger = %+v, want both cards claimed", plain.GPUs)
	}
	view := ledgerView{excludeDeployment: edited.ID}
	snap := nodeSnapshot(st, node, true, "", view)
	if snap.GPUs[0].AllocatedBytes != 0 || snap.GPUs[0].SleepingBytes != 0 {
		t.Fatalf("edited deployment's card = %+v, want free", snap.GPUs[0])
	}
	if snap.GPUs[1].AllocatedBytes != plain.GPUs[1].AllocatedBytes {
		t.Fatalf("another deployment's claim changed: %+v", snap.GPUs[1])
	}
	req := scheduler.Request{WeightBytes: 30 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	if got := suggestEviction(st, node, req, view); got != nil {
		t.Fatalf("the edited deployment's own instance suggested for sleep: %+v", got)
	}
}
