package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func putDGPUQuota(t *testing.T, h *apiHandlers, id uint, body string) (int, string) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(id))
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/dgpu", id), strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.setDeploymentDGPUQuota(rec, req)
	return rec.Code, rec.Body.String()
}

// quotaFixture is a dGPU deployment created through the API with an 8 GiB
// slice on the one 64 GiB card of an NVIDIA node, its running instance, and
// a co-tenant holding a 40 GiB slice on the same card: 64 - 8.5 - 40.5 =
// 15 GiB free, so the deployment's slice may grow to 23.5 GiB with its own
// claim handed back.
func quotaFixture(t *testing.T) (*apiHandlers, *store.Store, dto.Deployment) {
	t.Helper()
	h, modelID := partitionPoolFixture(t, map[string]string{"quota-node": accelerator.BackendNVIDIALogical})
	st := h.d.Store
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"name":"sliced","model_id":%d,"engine":"vllm","node_uuid":"quota-node","quick_params":{},"vram_limit_gib":8}`, modelID)
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	node, err := st.NodeByUUID("quota-node")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "neighbour", ModelID: modelID, NodeID: node.ID, Engine: "vllm", VRAMLimitBytes: 40 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if err != nil {
		t.Fatal(err)
	}
	for _, slice := range []struct {
		uuid  string
		dep   uint
		limit int64
	}{{"quota-own", dep.ID, 8 << 30}, {"quota-neighbour", other.ID, 40 << 30}} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: slice.uuid, Engine: "vllm", VRAMLimitBytes: slice.limit, PartitionBackend: accelerator.BackendNVIDIALogical})
		if _, err := st.CreateInstance(&store.Instance{UUID: slice.uuid, DeploymentID: slice.dep, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	return h, st, dep
}

// heldBy makes the handler see each named instance holding bytes on its card,
// as the agents' dGPU usage reports say.
func heldBy(h *apiHandlers, held map[string]int64) {
	h.d.MetricsLatest = func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		if metric != "gpuworks_dgpu_memory_used_bytes" {
			return nil, nil
		}
		var out []MetricLatest
		for uuid, bytes := range held {
			out = append(out, MetricLatest{Labels: map[string]string{"node": "quota-node", "gpu": "0", "instance": uuid}, Value: float64(bytes), TimeMS: time.Now().UnixMilli()})
		}
		return out, nil
	}
}

func instanceLimit(t *testing.T, st *store.Store, uuid string) int64 {
	t.Helper()
	inst, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := inst.DesiredState()
	if err != nil {
		t.Fatal(err)
	}
	return spec.VRAMLimitBytes
}

// A higher limit is in effect at once: the deployment and its running slice
// carry it (the ledger charges the slice by it, the controller pushes it to
// the agent), the change is announced and written on the timeline, and the
// answer lists the slice as held to it in place. Nothing restarts.
func TestDGPUQuotaRaiseTakesEffectInPlace(t *testing.T) {
	h, st, dep := quotaFixture(t)
	if dep.VRAMLimitBytes != 8<<30 {
		t.Fatalf("created deployment reads vram_limit_bytes=%d", dep.VRAMLimitBytes)
	}
	sub := st.Bus.Subscribe(16, "deployment", "instance")
	defer sub.Close()

	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`)
	for _, want := range []string{`"vram_limit_bytes":12884901888`, `"live":["quota-own"]`, `"pending":[]`} {
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("raise = %d %s, want %s", status, body, want)
		}
	}
	if row, _ := st.GetDeployment(dep.ID); row.VRAMLimitBytes != 12<<30 {
		t.Fatalf("stored limit = %d", row.VRAMLimitBytes)
	}
	if got := instanceLimit(t, st, "quota-own"); got != 12<<30 {
		t.Fatalf("instance limit = %d", got)
	}
	if inst, _ := st.InstanceByUUID("quota-own"); inst.State != proto.InstRunning {
		t.Fatalf("instance state = %s", inst.State)
	}
	reasons := map[string]string{}
	for range 2 {
		select {
		case ev := <-sub.C():
			reasons[ev.Topic+"/"+ev.Key] = ev.Reason
		default:
		}
	}
	if reasons["deployment/"+fmt.Sprint(dep.ID)] != store.DeploymentChangeDGPUQuota || reasons["instance/quota-own"] != "desired-state" {
		t.Fatalf("events = %v", reasons)
	}
	events, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Code == store.EventDeploymentDGPUQuotaChanged {
			found = true
			for _, want := range []string{`"from_bytes":8589934592`, `"to_bytes":12884901888`, `"live":1`, `"pending":0`} {
				if !strings.Contains(ev.Data, want) {
					t.Fatalf("event params = %s, want %s", ev.Data, want)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no %s on the timeline: %+v", store.EventDeploymentDGPUQuotaChanged, events)
	}

	// The same limit again changes nothing.
	if status, _ := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`); status != http.StatusOK {
		t.Fatalf("unchanged = %d", status)
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("an unchanged quota was announced: %+v", ev)
	default:
	}
}

// A lower limit is held in place only on a slice that holds no more than it:
// one holding more would fail its next allocation, so it keeps its limit
// until a restart and the answer says what it holds. The deployment takes the
// new limit either way, for the instances it starts next.
func TestDGPUQuotaLowerIsHeldInPlaceOnlyUnderWhatTheSliceHolds(t *testing.T) {
	h, st, dep := quotaFixture(t)
	heldBy(h, map[string]int64{"quota-own": 5 << 30, "quota-neighbour": 30 << 30})
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":6}`)
	if status != http.StatusOK || !strings.Contains(body, `"live":["quota-own"]`) {
		t.Fatalf("6 GiB over 5 held = %d %s", status, body)
	}
	if got := instanceLimit(t, st, "quota-own"); got != 6<<30 {
		t.Fatalf("instance limit = %d", got)
	}
	status, body = putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":4}`)
	for _, want := range []string{`"live":[]`, `"pending":[{"instance_uuid":"quota-own","reason":"in_use","used_bytes":5368709120}]`, `"vram_limit_bytes":4294967296`} {
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("4 GiB under 5 held = %d %s, want %s", status, body, want)
		}
	}
	if got := instanceLimit(t, st, "quota-own"); got != 6<<30 {
		t.Fatalf("a slice holding more than the limit was lowered to %d", got)
	}
	if row, _ := st.GetDeployment(dep.ID); row.VRAMLimitBytes != 4<<30 {
		t.Fatalf("deployment limit = %d", row.VRAMLimitBytes)
	}

	// Without a report of what the slice holds, a lower limit is not risked.
	heldBy(h, nil)
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":5.5}`); status != http.StatusOK || !strings.Contains(body, `"reason":"no_usage"`) {
		t.Fatalf("no report = %d %s", status, body)
	}
	// Starting, or asleep, it could need more than it holds now.
	if err := st.SetInstanceState("quota-own", proto.InstStarting, "loading"); err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":5}`); status != http.StatusOK || !strings.Contains(body, `"reason":"starting"`) {
		t.Fatalf("starting = %d %s", status, body)
	}
	if err := st.SetInstanceState("quota-own", proto.InstRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentSleep(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":4.5}`); status != http.StatusOK || !strings.Contains(body, `"reason":"asleep"`) {
		t.Fatalf("asleep = %d %s", status, body)
	}
	if got := instanceLimit(t, st, "quota-own"); got != 6<<30 {
		t.Fatalf("instance limit = %d, want the 6 GiB it last took", got)
	}
}

// A backend that reads its limit only at start keeps the instance's limit
// until a restart, raised or lowered.
func TestDGPUQuotaOnAStartTimeBackendWaitsForARestart(t *testing.T) {
	h, st, dep := quotaFixture(t)
	inst, _ := st.InstanceByUUID("quota-own")
	spec, _ := inst.DesiredState()
	spec.PartitionBackend = accelerator.BackendPPULogical
	raw, _ := json.Marshal(spec)
	if err := st.UpdateInstanceDesiredState("quota-own", string(raw)); err != nil {
		t.Fatal(err)
	}
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`)
	if status != http.StatusOK || !strings.Contains(body, `"pending":[{"instance_uuid":"quota-own","reason":"backend"}]`) || !strings.Contains(body, `"live":[]`) {
		t.Fatalf("ppu slice = %d %s", status, body)
	}
	if got := instanceLimit(t, st, "quota-own"); got != 8<<30 {
		t.Fatalf("instance limit = %d", got)
	}
}

// The card a raised slice sits on must keep it at the new limit, its own
// claim handed back: 23.5 GiB of room, a 24 GiB slice claims 24.5. The
// refusal names the card and both figures, and nothing changes. A stopped
// deployment holds no card; its new limit is placed at its next start.
func TestDGPUQuotaRefusedWhenTheCardCannotKeepIt(t *testing.T) {
	h, st, dep := quotaFixture(t)
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":23}`); status != http.StatusOK {
		t.Fatalf("23 GiB (claims 23.5, exactly the room) = %d %s", status, body)
	}
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":24}`)
	for _, want := range []string{`"code":"` + ErrorDGPUQuotaNoRoom + `"`, `"node":"quota-node"`, `"gpu":0`, `"free_gib":23.5`, `"need_gib":24.5`} {
		if status != http.StatusConflict || !strings.Contains(body, want) {
			t.Fatalf("24 GiB = %d %s, want %s", status, body, want)
		}
	}
	if row, _ := st.GetDeployment(dep.ID); row.VRAMLimitBytes != 23<<30 {
		t.Fatalf("a refused change was stored: %d", row.VRAMLimitBytes)
	}
	if got := instanceLimit(t, st, "quota-own"); got != 23<<30 {
		t.Fatalf("a refused change reached the instance: %d", got)
	}
	if err := st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstanceState("quota-own", proto.InstStopped, "stopped"); err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":24}`); status != http.StatusOK || !strings.Contains(body, `"live":[]`) {
		t.Fatalf("stopped deployment = %d %s", status, body)
	}
}

// A limit no node could run the deployment under gets the never-fits
// refusal a create with that limit gets: here, a slice larger than the card.
func TestDGPUQuotaRefusedWhenNoNodeCouldRunIt(t *testing.T) {
	h, st, dep := quotaFixture(t)
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":80}`)
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorDeploymentNeverFits+`"`) {
		t.Errorf("80 GiB = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.VRAMLimitBytes != 8<<30 {
		t.Fatalf("a refused change was stored: %d", row.VRAMLimitBytes)
	}
}

func TestDGPUQuotaRefusals(t *testing.T) {
	h, st, dep := quotaFixture(t)
	for body, want := range map[string]string{
		`{"vram_limit_gib":0}`:  ErrorDGPUQuotaInvalid,
		`{"vram_limit_gib":-4}`: ErrorDGPUQuotaInvalid,
		`{}`:                    ErrorDGPUQuotaInvalid,
		`{"vram_limit_gib":8,"sm_limit_pct":"x"}`: "unknown field",
		`{"vram_limit_gib":"8"}`:                  "cannot unmarshal",
	} {
		if status, got := putDGPUQuota(t, h, dep.ID, body); status != http.StatusBadRequest || !strings.Contains(got, want) {
			t.Errorf("%s = %d %s, want %s", body, status, got, want)
		}
	}
	if status, _ := putDGPUQuota(t, h, 9999, `{"vram_limit_gib":8}`); status != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", status)
	}
	node, _ := st.NodeByUUID("quota-node")
	whole, err := st.CreateDeployment(&store.Deployment{Name: "whole", ModelID: dep.ModelID, NodeID: node.ID, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, whole.ID, `{"vram_limit_gib":8}`); status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorDGPUQuotaNotPartitioned+`"`) {
		t.Errorf("whole-card = %d %s", status, body)
	}
	multi, err := st.CreateDeployment(&store.Deployment{Name: "multi", ModelID: dep.ModelID, NodeID: node.ID, Engine: "vllm", NNodes: 2, VRAMLimitBytes: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, multi.ID, `{"vram_limit_gib":12}`); status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorDGPUQuotaNotEditable+`"`) || !strings.Contains(body, `"shape":"multi_node"`) {
		t.Errorf("multi-node = %d %s", status, body)
	}
	if err := st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`); status != http.StatusConflict || !strings.Contains(body, ErrorDeploymentDeleting) {
		t.Errorf("deleting = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.VRAMLimitBytes != 8<<30 {
		t.Fatalf("a refused change was stored: %d", row.VRAMLimitBytes)
	}
	rec := httptest.NewRecorder()
	Router(Deps{Store: st}).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/dgpu", dep.ID), strings.NewReader(`{"vram_limit_gib":12}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", rec.Code)
	}
}

// Two replicas on one card hand both claims back and ask for both: 64 - 8.5 -
// 8.5 - 40.5 = 6.5 free, so the pair has 23.5 GiB. Two 11 GiB slices claim
// 23; two 12 GiB slices would claim 25.
func TestDGPUQuotaCountsEveryReplicaOnTheCard(t *testing.T) {
	h, st, dep := quotaFixture(t)
	node, _ := st.NodeByUUID("quota-node")
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "quota-own-2", Engine: "vllm", VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if _, err := st.CreateInstance(&store.Instance{UUID: "quota-own-2", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting, GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`)
	if status != http.StatusConflict || !strings.Contains(body, `"free_gib":23.5`) || !strings.Contains(body, `"need_gib":25`) {
		t.Fatalf("12 GiB x2 = %d %s", status, body)
	}
	status, body = putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":11}`)
	var result dto.DGPUQuotaResult
	if status != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil {
		t.Fatalf("11 GiB x2 = %d %s", status, body)
	}
	if slices.Sort(result.Live); !slices.Equal(result.Live, []string{"quota-own", "quota-own-2"}) {
		t.Fatalf("11 GiB x2 held in place = %v", result.Live)
	}
}

// The quota dialog's answers, from the real handlers: the deployment it
// opens on, an accepted change, and a refusal naming the card.
//
// Regenerate: GPUWORKS_UPDATE_UI_FIXTURES=1 go test ./internal/server/api/ -run TestDGPUQuotaUIFixtures
func TestDGPUQuotaUIFixtures(t *testing.T) {
	const dir = "../../../ui/tests/fixtures/dgpu-quota"
	update := os.Getenv(evaluateFixturesEnv) != ""
	h, _, dep := quotaFixture(t)
	stamp := regexp.MustCompile(`"(created_at|updated_at)":"[^"]+"`)
	fixed := func(body string) []byte {
		return []byte(stamp.ReplaceAllString(body, `"$1":"2026-10-18T08:00:00Z"`))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(dep.ID))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", dep.ID), nil)
	rec := httptest.NewRecorder()
	h.getDeployment(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	checkUIFixture(t, dir, "deployment", fixed(rec.Body.String()), update)
	status, body := putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":24}`)
	if status != http.StatusConflict {
		t.Fatalf("no room = %d %s", status, body)
	}
	checkUIFixture(t, dir, "no-room", fixed(body), update)
	status, body = putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":12}`)
	if status != http.StatusOK {
		t.Fatalf("change = %d %s", status, body)
	}
	checkUIFixture(t, dir, "changed", fixed(body), update)
	heldBy(h, map[string]int64{"quota-own": 10 << 30})
	status, body = putDGPUQuota(t, h, dep.ID, `{"vram_limit_gib":6}`)
	if status != http.StatusOK || !strings.Contains(body, `"in_use"`) {
		t.Fatalf("lower under use = %d %s", status, body)
	}
	checkUIFixture(t, dir, "pending", fixed(body), update)
	if update {
		t.Fatalf("regenerated fixtures under %s; rerun without %s", dir, evaluateFixturesEnv)
	}
}
