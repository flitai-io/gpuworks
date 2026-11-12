package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// candidatesFixture is five nodes for a deployment that requires nvidia and
// pool=a: one with room, one whose card is taken, one offline, one that is
// ascend, one without the label.
func candidatesFixture(t *testing.T) (*apiHandlers, *store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/candidates", Revision: "main", Name: "candidates"})
	if err != nil {
		t.Fatal(err)
	}
	online := map[string]bool{}
	for _, n := range []struct {
		uuid, vendor string
		labels       map[string]string
		online       bool
	}{
		{"n-room", "nvidia", map[string]string{"pool": "a"}, true},
		{"n-busy", "nvidia", map[string]string{"pool": "a"}, true},
		{"n-offline", "nvidia", map[string]string{"pool": "a"}, false},
		{"n-ascend", "ascend", map[string]string{"pool": "a"}, true},
		{"n-plain", "nvidia", nil, true},
	} {
		if _, err := st.RegisterNode(n.uuid, store.HashToken(n.uuid), n.uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(n.uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: n.uuid, Arch: "amd64", GPUVendor: n.vendor},
			GPUs: []proto.GPUInfo{{Index: 0, Name: "GPU", Vendor: n.vendor, MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		if n.labels != nil {
			if _, err := st.SetNodeLabels(n.uuid, n.labels); err != nil {
				t.Fatal(err)
			}
		}
		online[n.uuid] = n.online
	}
	busy, _ := st.NodeByUUID("n-busy")
	room, _ := st.NodeByUUID("n-room")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: room.ID, State: proto.MFPresent, Path: "/m"}); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "candidates", ModelID: model.ID, NodeID: room.ID, Engine: "vllm", Replicas: 2, Placement: `{"tp":1}`},
		map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "pool": {"a"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "other", ModelID: model.ID, NodeID: busy.ID, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "other-1", DeploymentID: other.ID, NodeID: busy.ID, State: proto.InstRunning, GPUIndexes: "[0]"}).Error; err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st, Online: func(uuid string) bool { return online[uuid] }}}, st, dep
}

func getCandidates(t *testing.T, h *apiHandlers, id uint, query string) (int, []byte) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(id))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/"+fmt.Sprint(id)+"/candidates"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.deploymentCandidates(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// The live picture of a deployment's pool, as wire bytes: every node counted
// once by the first check it fails, candidates first in the order replicas
// try them, unmet nodes with the requirement they fail.
func TestDeploymentCandidatesWire(t *testing.T) {
	h, _, dep := candidatesFixture(t)
	status, body := getCandidates(t, h, dep.ID, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, body)
	}
	const head = `{"nodes":5,"counts":{"busy":1,"cordoned":0,"offline":1,"room":1,"unmet":2},"unmet_by_key":{"gpuworks.ai/vendor":1,"pool":1},"placement_strategy":"spread","total":5,"offset":0,"limit":50,"items":[` +
		`{"uuid":"n-room","name":"n-room","status":"room","rank":1,"replicas":0,"free_gpus":1,"gpus":1,"model":"present"},` +
		`{"uuid":"n-busy","name":"n-busy","status":"busy","rank":2,"replicas":0,"free_gpus":0,"gpus":1,"model":"absent"},`
	if !strings.HasPrefix(string(body), head) {
		t.Fatalf("wire = %s\nwant prefix %s", body, head)
	}
	// Non-candidates follow in the store's node order; compare them as a set.
	var got dto.DeploymentCandidates
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	rest := map[string]string{}
	for _, item := range got.Items[2:] {
		if item.Rank != 0 {
			t.Fatalf("%s is not a candidate but has rank %d", item.UUID, item.Rank)
		}
		rest[item.UUID] = item.Status + "|" + item.Detail
	}
	want := map[string]string{
		"n-offline": "offline|",
		"n-ascend":  "unmet|node n-ascend does not satisfy gpuworks.ai/vendor ∈ {nvidia}",
		"n-plain":   "unmet|node n-plain does not satisfy pool ∈ {a}",
	}
	if fmt.Sprint(rest) != fmt.Sprint(want) {
		t.Fatalf("non-candidates = %v, want %v", rest, want)
	}
}

// A page and a status filter: a thousand nodes are never one response. The
// counts always cover the whole cluster.
func TestDeploymentCandidatesPagesAndFilters(t *testing.T) {
	h, _, dep := candidatesFixture(t)
	status, body := getCandidates(t, h, dep.ID, "?status=unmet&limit=1&offset=1")
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, body)
	}
	var page dto.DeploymentCandidates
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Offset != 1 || page.Limit != 1 || len(page.Items) != 1 || page.Items[0].Status != "unmet" || page.Counts["room"] != 1 {
		t.Fatalf("page = %s", body)
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=x", "?offset=-1", "?status=nope"} {
		if status, body := getCandidates(t, h, dep.ID, query); status != http.StatusBadRequest || !strings.Contains(string(body), ErrorFieldInvalid) {
			t.Errorf("%s = %d %s", query, status, body)
		}
	}
	if status, _ := getCandidates(t, h, 9999, ""); status != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", status)
	}
	rec := httptest.NewRecorder()
	Router(Deps{Store: h.d.Store}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/candidates", dep.ID), nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", rec.Code)
	}
}

// A deployment whose replicas wait for a node says how many, and why - the
// controller's latest pool reason with its closest candidates - while one
// whose replicas all run says nothing.
func TestDeploymentPlacementStatus(t *testing.T) {
	h, st, dep := candidatesFixture(t)
	st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{
		"scheduler": map[string]any{"code": "pool_no_fit", "params": map[string]any{"candidates": 2, "tried": 2,
			"closest": []map[string]any{{"node": "n-busy", "name": "n-busy", "reason": map[string]any{"code": "no_free_gpu"}}}}},
	})
	got := h.deploymentDTO(dep, false)
	if got.PlacementStatus == nil || got.PlacementStatus.Unscheduled != 2 || got.PlacementStatus.Reason == nil ||
		got.PlacementStatus.Reason.Code != "pool_no_fit" || got.PlacementStatus.At == nil || got.PlacementStatus.Reason.Params["closest"] == nil {
		t.Fatalf("placement status = %+v", got.PlacementStatus)
	}
	room, _ := st.NodeByUUID("n-room")
	for i := 0; i < 2; i++ {
		if err := st.DB().Create(&store.Instance{UUID: fmt.Sprintf("r-%d", i), DeploymentID: dep.ID, NodeID: room.ID, State: proto.InstRunning}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got := h.deploymentDTO(dep, false); got.PlacementStatus != nil {
		t.Fatalf("every replica runs, status = %+v", got.PlacementStatus)
	}
}

// A cordoned node is counted apart and can be listed on its own: it would
// otherwise be the deployment's best candidate.
func TestDeploymentCandidatesCountCordonedNodes(t *testing.T) {
	h, st, dep := candidatesFixture(t)
	if _, err := st.SetNodeScheduling("n-room", store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	status, body := getCandidates(t, h, dep.ID, "?status=cordoned")
	if status != http.StatusOK || !strings.Contains(string(body), `"counts":{"busy":1,"cordoned":1,"offline":1,"room":0,"unmet":2}`) ||
		!strings.Contains(string(body), `"total":1`) || !strings.Contains(string(body), `{"uuid":"n-room","name":"n-room","status":"cordoned"`) {
		t.Fatalf("cordoned page = %d %s", status, body)
	}
}

func TestDeploymentCandidatesRequiresGlobalModelOnlyForEligibleTargets(t *testing.T) {
	const missingModelID = uint(999999)
	setMissingModel := func(t *testing.T, st *store.Store, dep *store.Deployment) {
		t.Helper()
		if err := st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("model_id", missingModelID).Error; err != nil {
			t.Fatal(err)
		}
	}

	for _, blocked := range []string{"cordoned", "unmet"} {
		t.Run(blocked+" pool defers global model preparation", func(t *testing.T) {
			h, st, dep := candidatesFixture(t)
			setMissingModel(t, st, dep)
			if blocked == "cordoned" {
				for _, uuid := range []string{"n-room", "n-busy"} {
					if _, err := st.SetNodeScheduling(uuid, store.NodeDraining); err != nil {
						t.Fatal(err)
					}
				}
			} else if err := st.SetDeploymentRequirements(dep.ID, nodelabel.Requirements{"gpuworks.ai/node": {"missing-node"}}, store.PlacementSpread); err != nil {
				t.Fatal(err)
			}
			reads := countCandidateModelReads(t, st)
			status, body := getCandidates(t, h, dep.ID, "")
			if status != http.StatusOK || reads.Load() != 0 {
				t.Fatalf("inactive pool status=%d model reads=%d body=%s", status, reads.Load(), body)
			}
		})
	}

	t.Run("online NVIDIA candidate requires the global model record", func(t *testing.T) {
		h, st, dep := candidatesFixture(t)
		setMissingModel(t, st, dep)
		reads := countCandidateModelReads(t, st)
		status, body := getCandidates(t, h, dep.ID, "")
		if status != http.StatusInternalServerError || !strings.Contains(string(body), "model record 999999") {
			t.Fatalf("global model record error must be vendor-neutral: %d %s", status, body)
		}
		if got := reads.Load(); got != 1 {
			t.Fatalf("global model reads=%d want1", got)
		}
	})

	t.Run("offline AMD defers global model preparation", func(t *testing.T) {
		h, st, dep := candidatesFixture(t)
		if _, err := st.RegisterNode("n-amd", store.HashToken("n-amd"), "n-amd"); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus("n-amd", proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: "n-amd", Arch: "amd64", GPUVendor: "amd"},
			GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Radeon 8060S", Vendor: "amd", ArchID: "gfx1151", MemoryTotalBytes: 96 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetDeploymentRequirements(dep.ID, nodelabel.Requirements{"gpuworks.ai/vendor": {"amd"}}, store.PlacementSpread); err != nil {
			t.Fatal(err)
		}
		setMissingModel(t, st, dep)
		reads := countCandidateModelReads(t, st)
		status, body := getCandidates(t, h, dep.ID, "")
		if got := reads.Load(); got != 0 {
			t.Fatalf("offline pool loaded global model %d times", got)
		}
		if status != http.StatusOK || !strings.Contains(string(body), `"status":"offline"`) {
			t.Fatalf("offline AMD candidate should not load its model: %d %s", status, body)
		}
		online := h.d.Online
		h.d.Online = func(uuid string) bool { return uuid == "n-amd" || online(uuid) }
		status, body = getCandidates(t, h, dep.ID, "")
		if status != http.StatusInternalServerError || !strings.Contains(string(body), "model record 999999") {
			t.Fatalf("online target global model lookup=%d %s", status, body)
		}
		if got := reads.Load(); got != 1 {
			t.Fatalf("global model reads=%d want1", got)
		}
	})
}

func countCandidateModelReads(t *testing.T, st *store.Store) *atomic.Int64 {
	t.Helper()
	reads := &atomic.Int64{}
	name := "test:candidate-model-reads"
	if err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "models" {
			reads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Callback().Query().Remove(name) })
	return reads
}

// A node whose accelerator cannot run the launch is unmet under the
// "accelerator" key and carries a structured reason, not a bare code in detail.
func TestDeploymentCandidatesWireAcceleratorRefusal(t *testing.T) {
	h, st, dep := candidatesFixture(t)
	if _, err := st.RegisterNode("n-amd", store.HashToken("n-amd"), "n-amd"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("n-amd", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "n-amd", OS: "linux", Arch: "amd64", GPUVendor: "amd", RuntimeDriver: "host"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Instinct MI325X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 256 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentRequirements(dep.ID, nodelabel.Requirements{"gpuworks.ai/vendor": {"amd"}}, store.PlacementSpread); err != nil {
		t.Fatal(err)
	}
	h.d.Online = func(uuid string) bool { return uuid == "n-amd" }
	status, body := getCandidates(t, h, dep.ID, "?status=unmet")
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, body)
	}
	const wantKeys = `"unmet_by_key":{"accelerator":1,"gpuworks.ai/vendor":5}`
	const wantItem = `{"uuid":"n-amd","name":"n-amd","status":"unmet","reason":{"code":"runtime_unsupported","params":{"arch":"amd64","missing_facts":false,"os":"linux","required_arch":"amd64","required_os":"linux","required_runtime":"docker/nerdctl/udocker/kube","runtime":"host","vendor":"AMD"}},"replicas":0,"free_gpus":1,"gpus":1,"model":"absent"}`
	if !strings.Contains(string(body), wantKeys) || !strings.Contains(string(body), wantItem) {
		t.Fatalf("wire = %s\nwant %s and %s", body, wantKeys, wantItem)
	}
}
