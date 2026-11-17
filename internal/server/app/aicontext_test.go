package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

type aiFixture struct {
	st   *store.Store
	logs logstore.Store
	ai   *aiService
	node *store.Node
	dep  *store.Deployment
}

func newAIFixture(t *testing.T, name string) *aiFixture {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ls, err := logstore.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	node, err := st.RegisterNode(name+"-node", store.HashToken(name+"-token"), name+"-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: name + "-dep", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return &aiFixture{st: st, logs: ls, ai: newAIService(st, &observability{obsingest.New(obsingest.Stores{Logs: ls})}, nil), node: node, dep: dep}
}

func (f *aiFixture) instance(t *testing.T, uuid, state, message string, argv []string) *store.Instance {
	t.Helper()
	raw, err := json.Marshal(proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{Entrypoint: []string{"vllm", "serve"}, Argv: argv},
		UUID:           uuid, DeploymentID: f.dep.ID, DesiredState: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: f.dep.ID, NodeID: f.node.ID, State: state, Message: message, GPUIndexes: "[0]", Image: "vllm/vllm-openai:v0.10.1", DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// The seed is the first hop of a diagnosis: the newest lines, the error lines
// from further back that the tail would miss, events, the launch command, and
// the known-failure findings as hypotheses - with a manifest that says what
// was left out.
func TestErrorContextSeedsTailErrorLinesEventsAndHypotheses(t *testing.T) {
	f := newAIFixture(t, "app_ai_error_context")
	in := f.instance(t, "ai-ctx-failed", proto.InstFailed, "engine exited with code 1", []string{"/models/qwen3-32b", "--max-model-len", "32768"})
	f.st.AddInstanceEventCode(in, "error", "engine_exited", map[string]any{"code": 1})

	base := time.Now().Add(-time.Hour).UnixMilli()
	var entries []logstore.Entry
	// The cause prints early, at error level, and 200 lines of shutdown noise
	// follow it: a 60-line tail alone would not show it.
	entries = append(entries, logstore.Entry{Instance: in.UUID, Level: logstore.LevelError, Source: logstore.SourceInstance, TimeMS: base,
		Line: "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB"})
	for i := 1; i <= 200; i++ {
		entries = append(entries, logstore.Entry{Instance: in.UUID, Level: logstore.LevelInfo, Source: logstore.SourceInstance, TimeMS: base + int64(i)*1000, Line: fmt.Sprintf("shutdown step %d", i)})
	}
	if err := f.logs.Append(entries); err != nil {
		t.Fatal(err)
	}

	c, err := f.ai.errorContext(in.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Logs) != seedTailLines+1 {
		t.Fatalf("seed has %d log lines, want the %d-line tail plus the 1 error line", len(c.Logs), seedTailLines)
	}
	if !strings.Contains(c.Logs[0].Line, "CUDA out of memory") || c.Logs[len(c.Logs)-1].Line != "shutdown step 200" {
		t.Fatalf("logs are not oldest-first with the early error line kept: first %q last %q", c.Logs[0].Line, c.Logs[len(c.Logs)-1].Line)
	}
	if len(c.Findings) != 1 || c.Findings[0].Code != "CUDA_OOM" {
		t.Fatalf("findings = %+v, want CUDA_OOM", c.Findings)
	}
	if !reflect.DeepEqual(c.Command, []string{"vllm", "serve", "/models/qwen3-32b", "--max-model-len", "32768"}) || !reflect.DeepEqual(c.GPUIndexes, []int{0}) {
		t.Fatalf("command %v gpus %v", c.Command, c.GPUIndexes)
	}
	if len(c.Events) == 0 || c.Events[0].Code != "engine_exited" {
		t.Fatalf("events = %+v", c.Events)
	}
	prompt := c.Prompt()
	for _, want := range []string{"failure message: engine exited with code 1", "--max-model-len 32768", "unverified", "[CUDA_OOM]", "GET /api/v1/logs/search", "compare_with_healthy_replica"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	findings, err := f.ai.InstanceFindings(in.UUID)
	if err != nil || len(findings) != 1 || findings[0].Code != "CUDA_OOM" {
		t.Fatalf("InstanceFindings = %+v, %v", findings, err)
	}
}

func TestErrorContextUnknownInstanceIsNotFound(t *testing.T) {
	f := newAIFixture(t, "app_ai_error_context_missing")
	if _, err := f.ai.errorContext("no-such-instance"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("errorContext err = %v, want ErrNotFound", err)
	}
	if _, err := f.ai.InstanceFindings("no-such-instance"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("InstanceFindings err = %v, want ErrNotFound", err)
	}
}

// A seed with no log store wired is smaller, not an error.
func TestErrorContextWithoutLogStore(t *testing.T) {
	f := newAIFixture(t, "app_ai_error_context_nologs")
	in := f.instance(t, "ai-ctx-nologs", proto.InstFailed, "pull access denied for registry.local/x", nil)
	f.ai.obs = nil
	c, err := f.ai.errorContext(in.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Logs) != 0 || len(c.Findings) != 1 || c.Findings[0].Code != "IMAGE_PULL_DENIED" {
		t.Fatalf("logs %d findings %+v: the failure message alone should still match", len(c.Logs), c.Findings)
	}
}

// An instance whose agent reported a structured reason is diagnosed by its
// codes: the reported causes are the findings, and the prompt names the code,
// even when the stored logs would match something else.
func TestErrorContextDiagnosesByReportedCode(t *testing.T) {
	f := newAIFixture(t, "app_ai_error_context_reported")
	in := f.instance(t, "ai-ctx-reported", proto.InstStarting, "", nil)
	exit := 1
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: in.UUID, State: proto.InstFailed, Message: "container exited (code 1): ValueError: KV cache",
		RuntimeFailure: &proto.RuntimeFailure{Code: proto.ErrorContainerExited, ExitCode: &exit, Phase: proto.PhaseHealthCheck,
			Causes: []proto.FailureCause{{Code: "KV_CACHE_INSUFFICIENT", Evidence: []string{"ValueError: larger than the maximum number of tokens that can be stored in KV cache"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.logs.Append([]logstore.Entry{{Instance: in.UUID, Level: logstore.LevelError, Source: logstore.SourceInstance, TimeMS: time.Now().UnixMilli(),
		Line: "torch.OutOfMemoryError: CUDA out of memory"}}); err != nil {
		t.Fatal(err)
	}
	c, err := f.ai.errorContext(in.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Findings) != 1 || c.Findings[0].Code != "KV_CACHE_INSUFFICIENT" {
		t.Fatalf("findings = %+v, want the reported cause only", c.Findings)
	}
	prompt := c.Prompt()
	for _, want := range []string{"failure code: CONTAINER_EXITED", `"exit_code":1`, "[KV_CACHE_INSUFFICIENT]"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "[CUDA_OOM]") {
		t.Fatalf("prompt re-guessed from logs:\n%s", prompt)
	}
	findings, err := f.ai.InstanceFindings(in.UUID)
	if err != nil || len(findings) != 1 || findings[0].Code != "KV_CACHE_INSUFFICIENT" {
		t.Fatalf("InstanceFindings = %+v, %v", findings, err)
	}
}

func TestCompareWithHealthyReplicaReportsArgumentDifferences(t *testing.T) {
	f := newAIFixture(t, "app_ai_compare_replica")
	failed := f.instance(t, "ai-cmp-failed", proto.InstFailed, "engine exited", []string{"/m", "--max-model-len", "32768"})
	f.instance(t, "ai-cmp-healthy", proto.InstRunning, "", []string{"/m", "--max-model-len", "8192"})

	out, err := f.ai.CompareWithHealthyReplica(failed.UUID)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if got := m["only_in_failed"]; !reflect.DeepEqual(got, []string{"32768"}) {
		t.Fatalf("only_in_failed = %#v", got)
	}
	if got := m["only_in_healthy"]; !reflect.DeepEqual(got, []string{"8192"}) {
		t.Fatalf("only_in_healthy = %#v", got)
	}
	if m["healthy_replica"].(map[string]any)["instance_uuid"] != "ai-cmp-healthy" {
		t.Fatalf("healthy replica = %#v", m["healthy_replica"])
	}
}

func TestCompareWithHealthyReplicaWithoutOne(t *testing.T) {
	f := newAIFixture(t, "app_ai_compare_none")
	failed := f.instance(t, "ai-cmp-alone", proto.InstFailed, "engine exited", nil)
	f.instance(t, "ai-cmp-also-failed", proto.InstFailed, "engine exited", nil)
	out, err := f.ai.CompareWithHealthyReplica(failed.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["healthy_replica"] != nil || m["note"] == nil {
		t.Fatalf("out = %#v, want no replica and a note", out)
	}
}

// The fit is the smallest power-of-two TP whose per-GPU share fits one node,
// by the estimator's own requirement formula; a type's per-node maximum, not
// its fleet total, bounds the TP.
func TestFleetFitPicksSmallestTPPerAcceleratorType(t *testing.T) {
	model := &store.Model{ID: 7, WeightBytes: 61 << 30}
	g := func(node uint, name string, cards int, memGiB int64, cc string) store.OnlineGPUGroup {
		return store.OnlineGPUGroup{NodeID: node, Vendor: "nvidia", Name: name, ComputeCapability: cc, Cards: cards, MaxMemory: memGiB << 30}
	}
	fit := fleetFit(model, []store.OnlineGPUGroup{
		g(1, "H100 80GB", 8, 80, "9.0"),
		g(2, "A100 40GB", 4, 40, "8.0"),
		g(3, "RTX 4090", 2, 24, "8.9"),
		g(4, "RTX 4090", 1, 24, "8.9"),
	}, nil)
	util := deploy.DefaultGPUMemUtil(deploy.DefaultEngine)
	if fit.ModelID != 7 || fit.WeightBytes != 61<<30 || fit.GPUMemUtil != util || len(fit.Rows) != 3 {
		t.Fatalf("fit = %+v", fit)
	}
	byName := map[string]aihelper.FitRow{}
	for _, row := range fit.Rows {
		byName[row.Name] = row
	}
	h100, a100, rtx := byName["H100 80GB"], byName["A100 40GB"], byName["RTX 4090"]
	if !h100.Fits || h100.TP != 1 || !a100.Fits || a100.TP != 2 {
		t.Fatalf("H100 %+v A100 %+v", h100, a100)
	}
	if rtx.Fits || rtx.TP != 0 || rtx.Nodes != 2 || rtx.Cards != 3 || rtx.MaxCardsPerNode != 2 {
		t.Fatalf("RTX 4090 = %+v, want no fit across 2 nodes with at most 2 cards each", rtx)
	}
	reserve := estimate.ReserveBytes(0, "ampere", true, util)
	if want := estimate.RequiredPerGPUBytes(61<<30, 0, reserve, 2); a100.PerGPUBytes != want || a100.HeadroomBytes != a100.UsableBytes-want {
		t.Fatalf("A100 per GPU %d headroom %d, want %d and usable-need", a100.PerGPUBytes, a100.HeadroomBytes, want)
	}
	if fit.Rows[0].Name != "H100 80GB" {
		t.Fatalf("rows not sorted by memory: %+v", fit.Rows)
	}
}

// A 200-card fleet of three card models is three rows: the table follows the
// models, not the cards or the nodes.
func TestFleetFitOfALargeFleetIsOneRowPerCardModel(t *testing.T) {
	var groups []store.OnlineGPUGroup
	for node := uint(1); node <= 25; node++ {
		groups = append(groups, store.OnlineGPUGroup{NodeID: node, Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", Cards: 8, MaxMemory: 80 << 30})
	}
	groups = append(groups, store.OnlineGPUGroup{NodeID: 26, Vendor: "nvidia", Name: "L40S", ComputeCapability: "8.9", Cards: 4, MaxMemory: 48 << 30},
		store.OnlineGPUGroup{NodeID: 26, Vendor: "ascend", Name: "910B", Cards: 4, MaxMemory: 64 << 30})
	// 140 GiB at TP 2 is 70 GiB a card plus the Hopper reserve: over the 72 GiB
	// usable at 0.9, so TP 4.
	fit := fleetFit(&store.Model{ID: 1, WeightBytes: 140 << 30}, groups, nil)
	if len(fit.Rows) != 3 {
		t.Fatalf("rows = %+v", fit.Rows)
	}
	h100 := fit.Rows[0]
	if h100.Name != "H100" || h100.Cards != 200 || h100.Nodes != 25 || h100.MaxCardsPerNode != 8 || !h100.Fits || h100.TP != 4 {
		t.Fatalf("H100 row = %+v", h100)
	}
}

// The table states what the hardware is: a Turing card cannot compute
// bfloat16, broken cards are counted apart, and the fast-link group comes
// from the sampled nodes' topology.
func TestFleetFitStatesDtypeHealthAndInterconnect(t *testing.T) {
	groups := []store.OnlineGPUGroup{
		{NodeID: 1, Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", Cards: 7, Unusable: 1, MaxMemory: 80 << 30},
		{NodeID: 2, Vendor: "nvidia", Name: "Quadro RTX 8000", ComputeCapability: "7.5", Cards: 2, MaxMemory: 48 << 30},
		{NodeID: 3, Vendor: "nvidia", Name: "L4", ComputeCapability: "8.9", Cards: 1, MaxMemory: 24 << 30},
	}
	layouts := map[uint]store.NodeCardLayout{
		1: {Topology: &topoparse.Topology{FastCliques: [][]int{{0, 1, 2, 3}, {4, 5, 6, 7}}}, Names: map[int]string{0: "H100", 1: "H100", 2: "H100", 3: "H100", 4: "H100", 5: "H100", 6: "H100", 7: "H100"}},
		2: {Topology: &topoparse.Topology{}, Names: map[int]string{0: "Quadro RTX 8000", 1: "Quadro RTX 8000"}},
	}
	fit := fleetFit(&store.Model{ID: 3, WeightBytes: 16 << 30, Dtype: "bfloat16"}, groups, layouts)
	rows := map[string]aihelper.FitRow{}
	for _, r := range fit.Rows {
		rows[r.Name] = r
	}
	if fit.Dtype != "bfloat16" {
		t.Fatalf("dtype = %q", fit.Dtype)
	}
	if h := rows["H100"]; !h.DtypeSupported || h.Cards != 7 || h.Unusable != 1 || h.FastLinkCards != 4 {
		t.Fatalf("H100 = %+v", h)
	}
	if q := rows["Quadro RTX 8000"]; q.DtypeSupported || !strings.Contains(q.DtypeNote, "bfloat16") || q.FastLinkCards != 1 || !q.Fits {
		t.Fatalf("RTX 8000 = %+v, want fits in memory, no bf16, PCIe only", q)
	}
	if l := rows["L4"]; l.FastLinkCards != 0 {
		t.Fatalf("L4 without a topology = %+v", l)
	}
}

// At most layoutSample nodes of one card type have their topology read,
// however many carry it.
func TestFitSampleBoundsTopologyReadsPerCardType(t *testing.T) {
	var groups []store.OnlineGPUGroup
	for node := uint(1); node <= 100; node++ {
		groups = append(groups, store.OnlineGPUGroup{NodeID: node, Vendor: "nvidia", Name: "H100", Cards: 8})
	}
	groups = append(groups, store.OnlineGPUGroup{NodeID: 1, Vendor: "nvidia", Name: "L4", Cards: 1},
		store.OnlineGPUGroup{NodeID: 101, Vendor: "nvidia", Name: "L4", Cards: 1})
	got := fitSample(groups)
	if len(got) != layoutSample+1 {
		t.Fatalf("sampled %d nodes: %v", len(got), got)
	}
}

func TestArgDiffKeepsDuplicatesAndOrder(t *testing.T) {
	got := argDiff([]string{"--a", "1", "--b", "1", "--c"}, []string{"--a", "1", "--c"})
	if !reflect.DeepEqual(got, []string{"--b", "1"}) {
		t.Fatalf("argDiff = %v", got)
	}
}
