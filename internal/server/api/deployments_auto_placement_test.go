package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// autoRig is the real router over a store with four online two-card H100
// nodes (au-1..au-4) on one management network, and a model. Requests go
// through the router with an admin session, so routing, auth and the
// handler are one path.
type autoRig struct {
	*tokenRig
	modelID uint
}

func newAutoRig(t *testing.T, nodes int) *autoRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	model, err := rig.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto", Revision: "main", Name: "org/auto", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= nodes; i++ {
		uuid := fmt.Sprintf("au-%d", i)
		launchConfigNode(t, rig.st, uuid)
		if err := rig.st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
		if err := rig.st.SetNodeState(uuid, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &autoRig{tokenRig: rig, modelID: model.ID}
}

// multiBody is a create/evaluate body for an automatic two-rank deployment
// with tp 2 (one card on each rank's node); extra is spliced in after the
// fixed fields.
func (r *autoRig) multiBody(name, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"nnodes":2,"placement_mode":"auto","placement":{"tp":2}%s}`, name, name, r.modelID, extra)
}

// pdAutoBody is an automatic PD body with the given prefill and decode units.
func (r *autoRig) pdAutoBody(name, prefill, decode, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"placement_mode":"auto"%s,"placement":{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[%s]},"decode":{"tp":1,"units":[%s]}}}}}`,
		name, name, r.modelID, extra, prefill, decode)
}

func (r *autoRig) create(t *testing.T, body string) (*httptest.ResponseRecorder, dto.Deployment) {
	t.Helper()
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", body)
	var out dto.Deployment
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec, out
}

func (r *autoRig) mustCreate(t *testing.T, body string) dto.Deployment {
	t.Helper()
	rec, out := r.create(t, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	return out
}

// occupy puts a running instance on node's cards so the node is busy for
// automatic placement.
func (r *autoRig) occupy(t *testing.T, node string, cards ...int) {
	t.Helper()
	n, err := r.st.NodeByUUID(node)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := r.st.CreateDeployment(&store.Deployment{Name: "hold-" + node, ServedName: "hold-" + node, State: store.DeployActive, NodeID: n.ID, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateInstance(&store.Instance{UUID: "hold-" + node, DeploymentID: dep.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: jsonOf(t, cards)}); err != nil {
		t.Fatal(err)
	}
}

func (r *autoRig) deployments(t *testing.T) int {
	t.Helper()
	all, err := r.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// storedDeployment is the row behind a created deployment.
func (r *autoRig) storedDeployment(t *testing.T, id uint) *store.Deployment {
	t.Helper()
	dep, err := r.st.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

// An automatic two-rank request that names no node gets two distinct nodes
// on one network, binpack unless it says otherwise: with one card of au-3
// held, binpack takes au-3 (the fewest free cards that still fit) and spread
// takes the nodes with the most free ones.
func TestCreateAutoMultiNodePicksTheRanks(t *testing.T) {
	r := newAutoRig(t, 4)
	r.occupy(t, "au-3", 0)

	out := r.mustCreate(t, r.multiBody("packed", ""))
	if out.PlacementMode != "auto" || out.NNodes != 2 || out.PlacementStrategy != "binpack" {
		t.Fatalf("created = mode %q nnodes %d strategy %q", out.PlacementMode, out.NNodes, out.PlacementStrategy)
	}
	if !slices.Equal(out.NodeUUIDs, []string{"au-3", "au-1"}) || out.NodeUUID != "au-3" || out.MasterAddr != "10.0.0.4" {
		t.Fatalf("binpack picked %v leader %s master %s, want au-3 then au-1 led by au-3 (10.0.0.4)", out.NodeUUIDs, out.NodeUUID, out.MasterAddr)
	}
	if len(out.Units) != 1 || out.Units[0].NodeUUID != "au-3" || len(out.Units[0].Followers) != 1 || out.Units[0].Followers[0].NodeUUID != "au-1" {
		t.Fatalf("units = %+v", out.Units)
	}
	dep := r.storedDeployment(t, out.ID)
	if !dep.UnitsAuto || dep.NNodes != 2 || dep.PlacementStrategy != store.PlacementBinpack {
		t.Fatalf("stored = auto %v nnodes %d strategy %q", dep.UnitsAuto, dep.NNodes, dep.PlacementStrategy)
	}
	// The request word is not stored: the placement keeps the parallelism only.
	if strings.Contains(dep.Placement, "auto") {
		t.Fatalf("stored placement = %s", dep.Placement)
	}

	// The mode is on the wire of the deployment read as well.
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", out.ID), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"placement_mode":"auto"`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}

	spread := r.mustCreate(t, r.multiBody("spread", `,"placement_strategy":"spread"`))
	if spread.PlacementMode != "auto" || spread.PlacementStrategy != "spread" || !slices.Equal(spread.NodeUUIDs, []string{"au-1", "au-2"}) {
		t.Fatalf("spread = mode %q strategy %q nodes %v, want au-1 and au-2", spread.PlacementMode, spread.PlacementStrategy, spread.NodeUUIDs)
	}
}

// The nodes a request names take its first ranks; the platform picks the
// rest from the group they are in. Naming every rank leaves nothing to pick.
func TestCreateAutoMultiNodeKeepsTheNamedRanks(t *testing.T) {
	r := newAutoRig(t, 4)
	r.occupy(t, "au-3", 0)

	partial := r.mustCreate(t, r.multiBody("partial", `,"node_uuids":["au-4"]`))
	if partial.PlacementMode != "auto" || !slices.Equal(partial.NodeUUIDs, []string{"au-4", "au-3"}) || partial.NodeUUID != "au-4" {
		t.Fatalf("one named: nodes %v leader %s mode %q, want au-4 leading then binpack's au-3", partial.NodeUUIDs, partial.NodeUUID, partial.PlacementMode)
	}
	full := r.mustCreate(t, r.multiBody("full", `,"node_uuids":["au-4","au-2"]`))
	if full.PlacementMode != "auto" || !slices.Equal(full.NodeUUIDs, []string{"au-4", "au-2"}) {
		t.Fatalf("every rank named: nodes %v mode %q", full.NodeUUIDs, full.PlacementMode)
	}
	if !r.storedDeployment(t, full.ID).UnitsAuto {
		t.Fatal("a request that named every rank is still placed by the platform from then on")
	}
}

func TestCreateAutoMultiNodeRefusals(t *testing.T) {
	r := newAutoRig(t, 4)
	noTP := strings.Replace(r.multiBody("no-tp", ""), `,"placement":{"tp":2}`, "", 1)
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"no placement.tp", noTP, http.StatusBadRequest, ErrorAutoPlacementTPRequired},
		{"an empty placement", strings.Replace(r.multiBody("empty", ""), `"placement":{"tp":2}`, `"placement":{}`, 1), http.StatusBadRequest, ErrorAutoPlacementTPRequired},
		{"more nodes named than ranks", r.multiBody("many", `,"node_uuids":["au-1","au-2","au-3"]`), http.StatusBadRequest, ErrorDistributedNodeCount},
		{"a named node that does not exist", r.multiBody("ghost", `,"node_uuids":["no-such-node"]`), http.StatusConflict, ErrorUnitPlacementNone},
		{"an unknown strategy", r.multiBody("strategy", `,"placement_strategy":"random"`), http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", tc.body)
			code, _ := errorBody(t, rec.Body.String())
			if rec.Code != tc.status || (tc.code != "" && code != tc.code) {
				t.Fatalf("create = %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
	if n := r.deployments(t); n != 0 {
		t.Fatalf("refused creates left %d deployments", n)
	}
}

func TestEvaluateAutoMultiNodeTemplatePlacement(t *testing.T) {
	r := newAutoRig(t, 2)
	const spec = "bf16-vllm-nvidia-general"
	model := store.InferenceModel{ModelID: "org/auto", Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: spec, Engine: "vllm", Scenario: "balanced", Image: "vllm/vllm-openai:v0.25.1",
		Quantization: "bf16", WeightBytes: 1 << 30, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
		WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`,
	}}}
	if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	template, err := r.st.InferenceModelByModelID(model.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, placement, code, field string
		dynamic, unregistered        bool
	}{
		{name: "with TP", placement: `,"placement":{"tp":1,"pp":2}`},
		{name: "missing TP", code: ErrorAutoPlacementTPRequired, field: "placement_mode"},
		{name: "insufficient cards", placement: `,"placement":{"tp":8,"pp":2}`, code: ErrorUnitPlacementNone, field: "placement_mode"},
		{name: "TP alone", placement: `,"placement":{"tp":1}`, code: ErrorDistributedParallelIndivisible, field: "placement.pp"},
		{name: "hardware-selected spec succeeds", placement: `,"placement":{"tp":1,"pp":2}`, dynamic: true},
		{name: "hardware-selected spec with TP alone", placement: `,"placement":{"tp":1}`, code: ErrorDistributedParallelIndivisible, field: "placement.pp", dynamic: true},
		{name: "hardware-selected spec with no TP", code: ErrorAutoPlacementTPRequired, field: "placement_mode", dynamic: true},
		{name: "hardware-selected spec with no capacity", placement: `,"placement":{"tp":8,"pp":2}`, code: ErrorUnitPlacementNone, field: "placement_mode", dynamic: true},
		{name: "unregistered weights with no TP", code: ErrorAutoPlacementTPRequired, field: "placement_mode", unregistered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var specFields string
			if !tc.dynamic {
				specFields = fmt.Sprintf(`,"template_spec_source":"upstream","template_spec_name":%q`, spec)
			}
			modelID, templateID := r.modelID, template.ID
			const unregisteredRepo = "org/unregistered"
			if tc.unregistered {
				unregistered := model
				unregistered.ModelID = unregisteredRepo
				unregistered.Specs = append([]store.InferenceSpecRow(nil), model.Specs...)
				unregistered.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"org/unregistered","revision":"main"}]`
				if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{unregistered}); err != nil {
					t.Fatal(err)
				}
				target, err := r.st.InferenceModelByModelID(unregisteredRepo)
				if err != nil {
					t.Fatal(err)
				}
				modelID, templateID = 0, target.ID
			}
			body := fmt.Sprintf(`{"model_id":%d,"template_id":%d,"target_vendor":"nvidia","nnodes":2,"placement_mode":"auto","quick_params":{"max_model_len":4096}%s%s}`, modelID, templateID, specFields, tc.placement)
			rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
			var out dto.EvaluateResponse
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
				t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
			}
			if tc.unregistered {
				models, err := r.st.ListModels(unregisteredRepo)
				if err != nil || len(models) != 0 {
					t.Fatalf("refused evaluation registered a model: count=%d err=%v", len(models), err)
				}
			}
			if tc.code == "" {
				if !out.OK || !out.AutoPlaced || len(out.RankCommands) != 2 || out.Preflight == nil || !out.Preflight.OK || out.Parallelism == nil || out.Parallelism.GPUsPerNode != 1 {
					t.Fatalf("TP=1 should place two ranks and pass preflight: %s", rec.Body)
				}
			} else {
				if out.OK || len(out.Errors) != 1 || out.Errors[0].Code != tc.code || out.Errors[0].Field != tc.field || len(out.RankCommands) != 0 {
					t.Fatalf("wanted a placement error %s and no rank preview: %s", tc.code, rec.Body)
				}
				if out.Preflight == nil || out.Preflight.OK || len(out.Preflight.Checks) != 5 ||
					out.Preflight.Checks[0].ID != "automatic_placement" || out.Preflight.Checks[0].Code != tc.code || out.Preflight.Checks[0].Status != proto.CheckFail {
					t.Fatalf("preflight should explain why the group was not selected: %s", rec.Body)
				}
				for i, id := range []string{"node_count", "node_list", "port_range", "shared_network"} {
					check := out.Preflight.Checks[i+1]
					if check.ID != id || check.Status != proto.CheckSkip ||
						!slices.Equal(check.BlockedBy, []string{"automatic_placement"}) || check.Code != "PREFLIGHT_BLOCKED" {
						t.Fatalf("expected %s to be blocked by automatic placement: %s", id, rec.Body)
					}
				}
				if tc.code == ErrorUnitPlacementNone && !tc.dynamic && (out.FormValues["tp"] != float64(8) || out.FormValues["pp"] != float64(2)) {
					t.Fatalf("a capacity refusal should keep the entered TP and PP: %s", rec.Body)
				}
			}
			if len(out.EligibleNodes) != 2 {
				t.Fatalf("evaluate lost node eligibility: %s", rec.Body)
			}
			if tc.dynamic && tc.code != "" {
				if out.TemplateMatch != "" || out.TemplateSpecName != "" || out.FormValues != nil {
					t.Fatalf("hardware-dependent defaults should wait for the leader: %s", rec.Body)
				}
				return
			}
			if out.TemplateMatch == "" || out.TemplateSpecName != spec || out.FormValues["max_model_len"] != float64(4096) {
				t.Fatalf("evaluate lost node eligibility, template match or form values: %s", rec.Body)
			}
		})
	}
}

// With nothing to place on the request is a conflict, not a bad request, and
// the counts say how many nodes each check left out as JSON numbers.
func TestCreateAutoMultiNodeWithNoRoomSaysWhy(t *testing.T) {
	r := newAutoRig(t, 4)
	for _, uuid := range []string{"au-1", "au-2", "au-3"} {
		r.occupy(t, uuid, 0, 1)
	}
	if _, err := r.st.SetNodeScheduling("au-4", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nodes": float64(4), "busy": float64(3), "cordoned": float64(1), "offline": float64(0), "unmet": float64(0), "no_network": float64(0), "no_peer_memory": float64(0), "largest": float64(0)}
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("none", ""))
	code, params := errorBody(t, rec.Body.String())
	if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || fmt.Sprint(params) != fmt.Sprint(want) {
		t.Fatalf("create = %d %s, want 409 UNIT_PLACEMENT_NONE with %v", rec.Code, rec.Body, want)
	}

	if n := r.deployments(t); n != 3 { // the three holders
		t.Fatalf("a refused create left %d deployments, want only the 3 holders", n)
	}

	// Offline nodes are counted as offline.
	t.Run("offline", func(t *testing.T) {
		r := newAutoRig(t, 3)
		for _, uuid := range []string{"au-1", "au-2", "au-3"} {
			if err := r.st.SetNodeState(uuid, store.NodeStateOffline); err != nil {
				t.Fatal(err)
			}
		}
		rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("none", ""))
		code, params := errorBody(t, rec.Body.String())
		if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["offline"] != float64(3) || params["nodes"] != float64(3) || params["busy"] != float64(0) {
			t.Fatalf("all offline = %d %s", rec.Code, rec.Body)
		}
	})

	// One node with room is not enough for two ranks: the largest group is 1.
	t.Run("one free node", func(t *testing.T) {
		r := newAutoRig(t, 2)
		r.occupy(t, "au-2", 0, 1)
		rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("one", ""))
		code, params := errorBody(t, rec.Body.String())
		if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["largest"] != float64(1) || params["busy"] != float64(1) {
			t.Fatalf("one free node = %d %s", rec.Code, rec.Body)
		}
	})
}

// A multi-node request that places itself is unchanged: it is stored as the
// operator's own, whether it says "manual" or says nothing, and a
// single-node deployment has no placement mode at all.
func TestCreateManualShapesKeepTheirMode(t *testing.T) {
	r := newAutoRig(t, 4)
	manualBody := func(name, mode string) string {
		return strings.Replace(r.multiBody(name, `,"node_uuids":["au-2","au-4"]`), `"placement_mode":"auto",`, mode, 1)
	}
	for name, mode := range map[string]string{"silent": ``, "explicit": `"placement_mode":"manual",`} {
		out := r.mustCreate(t, manualBody("manual-"+name, mode))
		if out.PlacementMode != "manual" || !slices.Equal(out.NodeUUIDs, []string{"au-2", "au-4"}) || r.storedDeployment(t, out.ID).UnitsAuto {
			t.Fatalf("%s manual create = mode %q nodes %v", name, out.PlacementMode, out.NodeUUIDs)
		}
	}
	// A manual multi-node request still needs its nodes.
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", strings.Replace(r.multiBody("bare", ""), `"placement_mode":"auto",`, ``, 1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a manual multi-node create without nodes = %d %s, want 400", rec.Code, rec.Body)
	}

	// Single node: the mode word is the old per-node one and the response has
	// no placement_mode key.
	single := fmt.Sprintf(`{"name":"single","served_name":"single","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"node_uuid":"au-1","placement_mode":"auto"}`, r.modelID)
	rec = r.do(r.session, http.MethodPost, "/api/v1/deployments", single)
	if rec.Code != http.StatusCreated {
		t.Fatalf("single-node auto create = %d %s", rec.Code, rec.Body)
	}
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	if _, present := wire["placement_mode"]; present {
		t.Fatalf("a single-node deployment carries placement_mode: %s", wire["placement_mode"])
	}
	var created dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if r.storedDeployment(t, created.ID).UnitsAuto {
		t.Fatal("a single-node deployment is not placed by units")
	}
}

func TestEvaluateRefusedPlacementKeepsNodeEligibility(t *testing.T) {
	r := newAutoRig(t, 3)
	const specName = "review-eligible"
	for _, vendor := range []string{"nvidia", "kunlunxin"} {
		t.Run(vendor, func(t *testing.T) {
			status := nodeStatusTwoGPUs("au-3", 3)
			predicate := `{"vendor":"nvidia","cc":">=8.0"}`
			if vendor == "nvidia" {
				for i := range status.GPUs {
					gpu := &status.GPUs[i]
					gpu.Name, gpu.ArchID, gpu.ComputeCapability = "Tesla V100", "sm_70", "7.0"
				}
			} else {
				predicate = `{"vendor":"kunlunxin"}`
				status.Sys.GPUVendor, status.Sys.Framework = "kunlunxin", "xpu"
				status.Sys.OS, status.Sys.RuntimeDriver = "linux", "docker"
				for i := range status.GPUs {
					gpu := &status.GPUs[i]
					gpu.Vendor, gpu.Name, gpu.ArchID, gpu.ComputeCapability = vendor, "P800 PCIe", "kl3", ""
				}
			}
			if err := r.st.UpdateNodeStatus("au-3", status); err != nil {
				t.Fatal(err)
			}
			spec := store.InferenceSpecRow{
				Name: specName, Engine: "vllm", Scenario: "balanced", Image: "vllm/vllm-openai:v0.25.1",
				Quantization: "bf16", WeightBytes: 1 << 30, GPUs: predicate,
				WeightSources: `[{"provider":"huggingface","repo_id":"org/auto","revision":"main"}]`,
			}
			model := store.InferenceModel{ModelID: "org/auto", Source: "upstream", Specs: []store.InferenceSpecRow{spec}}
			if vendor == "nvidia" {
				other := spec
				other.Name, other.Scenario, other.GPUs = "v100-spec", "throughput", `{"vendor":"nvidia","cc":"7.0"}`
				model.Specs = append(model.Specs, other)
			}
			if err := r.st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
				t.Fatal(err)
			}
			template, err := r.st.InferenceModelByModelID(model.ModelID)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, spec, pin string
			}{
				{name: "named", spec: specName},
				{name: "hardware-selected"},
				{name: "stale", spec: "retired"},
				{name: "incompatible pin", spec: specName, pin: `,"node_uuids":["au-3"]`},
			} {
				if tc.pin != "" && vendor != "nvidia" {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					body := fmt.Sprintf(`{"model_id":%d,"template_id":%d,"target_vendor":%q,"scenario":"balanced","nnodes":2,"placement_mode":"auto","template_spec_name":%q%s}`, r.modelID, template.ID, vendor, tc.spec, tc.pin)
					rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
					var out dto.EvaluateResponse
					if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.OK ||
						!slices.ContainsFunc(out.Errors, func(field dto.FieldError) bool { return field.Code == ErrorAutoPlacementTPRequired }) {
						t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
					}
					if tc.spec == "retired" {
						if len(out.EligibleNodes) != 0 || !slices.ContainsFunc(out.Errors, func(field dto.FieldError) bool {
							return field.Field == "template_spec_name" && strings.Contains(fmt.Sprint(field.Params["detail"]), "does not exist")
						}) {
							t.Fatalf("stale spec must be reported with no eligibility result: %s", rec.Body)
						}
						return
					}
					index := slices.IndexFunc(out.EligibleNodes, func(node dto.EligibleNode) bool { return node.UUID == "au-3" })
					if index == -1 {
						t.Fatalf("missing candidate: %s", rec.Body)
					}
					node := out.EligibleNodes[index]
					if vendor == "kunlunxin" {
						if !node.Eligible {
							t.Fatalf("P800 should be eligible: %+v", node)
						}
					} else if node.Eligible || !slices.ContainsFunc(node.Reasons, func(reason dto.ScheduleReason) bool { return reason.Code == "chip_mismatch" }) {
						t.Fatalf("V100 should fail the requested scenario's template predicate: %+v", node)
					}
				})
			}
		})
	}
}
