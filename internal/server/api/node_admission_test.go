package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// siteCap refuses every node once allow inserts were let through, the way
// a site cap set to the fleet's current size would.
func siteCap(allow int64) (store.NodeEnrollmentGuard, *atomic.Int64) {
	var asked atomic.Int64
	return func(c store.FleetCapacity) *messages.ProductError {
		if asked.Add(1) <= allow {
			return nil
		}
		return messages.NewProductError("SITE_NODE_CAP", "the site allows no more nodes", map[string]any{"nodes": c.Nodes})
	}, &asked
}

// The exact wire answer of a refused enrollment: 403, the guard's code and
// params, and its fallback behind the store's sentinel.
const refusedWire = `{"code":"SITE_NODE_CAP","error":"node enrollment refused: the site allows no more nodes","params":{"nodes":0}}`

func assertRefusedWire(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != refusedWire {
		t.Fatalf("%s = %d %s, want 403 %s", name, rec.Code, rec.Body, refusedWire)
	}
}

func assertNoNodes(t *testing.T, st *store.Store) {
	t.Helper()
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("nodes after a refusal = %+v err=%v", nodes, err)
	}
}

// guardProviderDriver records the instances it was asked to rent.
type guardProviderDriver struct {
	apiInstallProviderDriver
	created atomic.Int64
}

func (d *guardProviderDriver) Name() string { return "api-enroll-guard-provider" }

// ListInstances names an instance whose SSH port refuses at once, so the
// install's precheck fails fast instead of timing out.
func (d *guardProviderDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	return []provider.Instance{{ExternalID: "inst-1", Name: "inst", PublicIP: "127.0.0.1", SSHPort: 1, SSHUser: "root"}}, nil
}

func (d *guardProviderDriver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	d.created.Add(1)
	return provider.Instance{ExternalID: "rented"}, nil
}

func guardedProviderHandlers(t *testing.T, guard store.NodeEnrollmentGuard) (*apiHandlers, *store.Store, *guardProviderDriver, uint) {
	t.Helper()
	drv := &guardProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("api-enroll-guard")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "guarded", provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	st.AddNodeEnrollmentGuard(guard)
	return &apiHandlers{d: Deps{Store: st, Providers: svc, NodeInstaller: &fakeInstaller{}}}, st, drv, p.ID
}

// Every way a node is added answers a refused enrollment with 403 and the
// guard's code, before anything is spent: no machine is rented, nothing is
// installed over SSH or on an instance, and no node row is left behind.
func TestNodeEnrollmentRefusalAnswers403BeforeAnythingIsSpent(t *testing.T) {
	guard, _ := siteCap(0)
	h, st, drv, providerID := guardedProviderHandlers(t, guard)

	rec := httptest.NewRecorder()
	h.createNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes", strings.NewReader(`{"name":"a","server_url":"https://gw.example"}`)))
	assertRefusedWire(t, "POST /nodes", rec)

	installer := h.d.NodeInstaller.(*fakeInstaller)
	rec = httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(`{"target":"root@example.com","password":"p","server_url":"https://gw.example"}`)))
	assertRefusedWire(t, "POST /nodes/install", rec)
	if installer.req.Target != "" {
		t.Fatalf("installed over SSH after a refusal: %+v", installer.req)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{"external_id":"inst-1","server_url":"https://gw.example","password":"p"}`)))
	h.installAgentOnInstance(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)})))
	assertRefusedWire(t, "POST /providers/{id}/install", rec)
	if drv.req.ServerURL != "" {
		t.Fatalf("the agent was installed after a refusal: %+v", drv.req)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/instances", strings.NewReader(`{"name":"gpu-box"}`))
	h.createProviderInstance(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)})))
	assertRefusedWire(t, "POST /providers/{id}/instances", rec)
	if drv.created.Load() != 0 {
		t.Fatal("a machine was rented although no node could join")
	}
	assertNoNodes(t, st)
}

// The pre-check is advisory: when another insert takes the last place
// between it and the provider's own insert, the insert's refusal is
// answered the same way.
func TestProviderInstallAnswersARefusalAtTheInsert(t *testing.T) {
	guard, asked := siteCap(1)
	h, st, drv, providerID := guardedProviderHandlers(t, guard)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{"external_id":"inst-1","server_url":"https://gw.example","password":"p"}`)))
	h.installAgentOnInstance(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)})))
	want := `{"code":"SITE_NODE_CAP","error":"create pending node: node enrollment refused: the site allows no more nodes","params":{"nodes":0}}`
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("install refused at the insert = %d %s, want 403 %s", rec.Code, rec.Body, want)
	}
	if asked.Load() != 2 || drv.req.ServerURL != "" {
		t.Fatalf("guard asked %d times, install request %+v", asked.Load(), drv.req)
	}
	assertNoNodes(t, st)
}

// With no guard the same requests succeed as before.
func TestNodeCreationWithoutAGuardIsUnchanged(t *testing.T) {
	h, st, drv, providerID := guardedProviderHandlers(t, nil)
	rec := httptest.NewRecorder()
	h.createNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes", strings.NewReader(`{"name":"a","server_url":"https://gw.example"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /nodes = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/instances", strings.NewReader(`{"name":"gpu-box"}`))
	h.createProviderInstance(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)})))
	if rec.Code != http.StatusCreated || drv.created.Load() != 1 {
		t.Fatalf("POST /providers/{id}/instances = %d %s", rec.Code, rec.Body)
	}
	if nodes, _ := st.ListNodes(); len(nodes) != 1 {
		t.Fatalf("nodes = %d", len(nodes))
	}
}

// A Kubernetes agent the guard refuses gets KUBE_ENROLL_REFUSED with reason
// enrollment_refused, which it treats as final, plus the guard's code and
// params; a node that enrolled before re-enrolls regardless.
func TestKubeEnrollAnswersAGuardRefusal(t *testing.T) {
	rig := newTokenRig(t)
	c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "k", Namespace: "gw-k"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(node string) *httptest.ResponseRecorder {
		return rig.do("", http.MethodPost, "/api/v1/kube/enroll", fmt.Sprintf(`{"token":%q,"cluster_uid":"uid","node_name":%q}`, c.Token, node))
	}
	if rec := enroll("gpu-1"); rec.Code != http.StatusOK {
		t.Fatalf("first node = %d %s", rec.Code, rec.Body)
	}
	guard, _ := siteCap(0)
	rig.st.AddNodeEnrollmentGuard(guard)
	rec := enroll("gpu-2")
	want := `{"code":"KUBE_ENROLL_REFUSED","error":"node enrollment refused: the site allows no more nodes","params":{"nodes":1,"reason":"enrollment_refused","refusal_code":"SITE_NODE_CAP"}}`
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("refused node = %d %s, want %s", rec.Code, rec.Body, want)
	}
	if rec := enroll("gpu-1"); rec.Code != http.StatusOK {
		t.Fatalf("re-enrolling an enrolled node = %d %s", rec.Code, rec.Body)
	}
}

// A cluster's first apply, which brings its nodes, is pre-checked; a later
// apply only updates enrolled agents and is not; removing never is.
func TestKubeApplyPreChecksOnlyAFirstApply(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", fmt.Sprintf(`{"name":"ack","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	uuid := decodeKube[map[string]any](t, rec)["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	guard, _ := siteCap(0)
	rig.st.AddNodeEnrollmentGuard(guard)

	refused := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":false}`)
	assertRefusedWire(t, "first apply", refused)
	if len(rig.api.objects) != 0 {
		t.Fatalf("objects applied after a refusal: %v", rig.api.objects)
	}
	if rec := rig.do(rig.session, http.MethodPost, base+"/remove", `{"dry_run":true}`); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if err := rig.st.DB().Model(&store.KubeCluster{}).Where("uuid = ?", uuid).Update("cluster_uid", "uid").Error; err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":false}`); rec.Code != http.StatusOK || len(rig.api.objects) != 9 {
		t.Fatalf("apply to an enrolled cluster = %d %s", rec.Code, rec.Body)
	}
}

// A held node answers an uncordon with 409 and the holder's code, keeps its
// cordon, and still takes an operator's drain; its view says why it is held
// and that turning it back on is refused.
func TestUncordonOfAHeldNodeIs409WithTheHoldersCode(t *testing.T) {
	h, st, _ := schedulingFixture(t)
	holder := &fixedHolder{source: "maint", held: "drain-a"}
	if err := st.AddSchedulingHolder(holder); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplySchedulingHolds(); err != nil {
		t.Fatal(err)
	}
	status, body := callNode(t, h.setNodeScheduling, http.MethodPut, "drain-a", "/scheduling", `{"scheduling":"schedulable"}`)
	want := `{"code":"MAINT_WINDOW","error":"node is held out of scheduling: maintenance until 06:00","params":{"until":"06:00"}}`
	if status != http.StatusConflict || strings.TrimSpace(body) != want {
		t.Fatalf("uncordon = %d %s, want 409 %s", status, body, want)
	}
	node, err := st.NodeWithGPUs("drain-a")
	if err != nil || node.Scheduling != store.NodeCordoned || node.SchedulingBy != "maint" {
		t.Fatalf("after a refused uncordon: %+v err=%v", node, err)
	}
	view := h.nodeView(node)
	raw, _ := json.Marshal(struct {
		Scheduling    string `json:"scheduling"`
		SchedulingBy  string `json:"scheduling_by"`
		CordonMessage any    `json:"cordon_message"`
		Uncordon      string `json:"uncordon"`
	}{view.Scheduling, view.SchedulingBy, view.CordonMessage, view.Uncordon})
	if string(raw) != `{"scheduling":"cordoned","scheduling_by":"maint","cordon_message":{"code":"MAINT_WINDOW","params":{"until":"06:00"},"message":"maintenance until 06:00"},"uncordon":"refused"}` {
		t.Fatalf("held node view = %s", raw)
	}
	if status, body := callNode(t, h.setNodeScheduling, http.MethodPut, "drain-a", "/scheduling", `{"scheduling":"draining"}`); status != http.StatusOK || !strings.Contains(body, `"scheduling":"draining","scheduling_by":"operator"`) || !strings.Contains(body, `"uncordon":"refused"`) {
		t.Fatalf("drain of a held node = %d %s", status, body)
	}
	if status, _ := callNode(t, h.setNodeScheduling, http.MethodPut, "drain-b", "/scheduling", `{"scheduling":"schedulable"}`); status != http.StatusOK {
		t.Fatalf("uncordon of a node nobody holds = %d", status)
	}
}

// Uncordon says what turning scheduling back on does, so the console
// need not decide: confirm while the platform would cordon again, allowed
// otherwise; and a node with no hold has no cordon_message on the wire.
func TestNodeViewUncordonFollowsThePlatformsReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		node store.Node
		want string
	}{
		{"nothing wrong", store.Node{UUID: "n"}, `"uncordon":"allowed"`},
		{"runtime cannot use the cards", store.Node{UUID: "n", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing}, `"uncordon":"confirm"`},
		{"kubernetes cordoned it", store.Node{UUID: "n", KubeUnschedulable: true}, `"uncordon":"confirm"`},
		{"a card needs a reboot", store.Node{UUID: "n", GPUs: []store.GPU{{HealthCode: accelerator.HealthRebootRequired}}}, `"uncordon":"confirm"`},
	} {
		raw, err := json.Marshal(toNodeDTO(&tc.node))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), tc.want) || strings.Contains(string(raw), "cordon_message") {
			t.Errorf("%s: %s, want %s and no cordon_message", tc.name, raw, tc.want)
		}
	}
}

type fixedHolder struct {
	source store.SchedulingSource
	held   string
}

func (h *fixedHolder) Source() store.SchedulingSource { return h.source }

func (h *fixedHolder) Hold(f store.NodeHoldFacts) store.NodeHold {
	if f.UUID != h.held {
		return store.NodeHold{}
	}
	return store.NodeHold{Code: "MAINT_WINDOW", Fallback: "maintenance until 06:00", Params: map[string]any{"until": "06:00"}}
}

type fixedCardHolder struct {
	source store.SchedulingSource
	ident  string
}

func (h *fixedCardHolder) Source() store.SchedulingSource { return h.source }

func (h *fixedCardHolder) HoldCard(f store.CardHoldFacts) store.CardHold {
	if f.Ident != h.ident {
		return store.CardHold{}
	}
	return store.CardHold{Code: "CARD_CAP", Fallback: "beyond the fleet's card capacity", Params: map[string]any{"cap": 1}}
}

// A held card answers an enable with 409 and the holder's code and still
// takes an operator's stop; the node's view says which card is held, by
// whom and why, and offers only the stop. A card nobody holds carries
// neither field on the wire.
func TestEnablingAHeldCardIs409WithTheHoldersCode(t *testing.T) {
	h, st, _ := schedulingFixture(t)
	ok := true
	cards := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
		{Index: 1, UUID: "GPU-b", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
	}
	if err := st.AddCardSchedulingHolder(&fixedCardHolder{source: "cap", ident: accelerator.CardIdent("GPU-b", "0000:40:00.0")}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("drain-a", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
	status, body := callNode(t, h.setCardScheduling, http.MethodPut, "drain-a", "/gpus/scheduling", `{"pcie_bdf":"0000:40:00.0","scheduling":"schedulable"}`)
	want := `{"code":"CARD_CAP","error":"card is held out of scheduling: beyond the fleet's card capacity","params":{"cap":1}}`
	if status != http.StatusConflict || strings.TrimSpace(body) != want {
		t.Fatalf("enable a held card = %d %s, want 409 %s", status, body, want)
	}
	node, err := st.NodeWithGPUs("drain-a")
	if err != nil {
		t.Fatal(err)
	}
	view := h.nodeView(node)
	type wireCard struct {
		PCIeBDF           string   `json:"pcie_bdf"`
		Schedulable       bool     `json:"schedulable"`
		SchedulingActions []string `json:"scheduling_actions,omitempty"`
		SchedulingHeldBy  string   `json:"scheduling_held_by,omitempty"`
		SchedulingHold    any      `json:"scheduling_hold,omitempty"`
	}
	var got []wireCard
	for _, g := range view.GPUs {
		raw, _ := json.Marshal(g)
		var c wireCard
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `[{"pcie_bdf":"0000:3f:00.0","schedulable":true,"scheduling_actions":["stopped"]},`+
		`{"pcie_bdf":"0000:40:00.0","schedulable":false,"scheduling_actions":["stopped"],"scheduling_held_by":"cap","scheduling_hold":{"code":"CARD_CAP","message":"beyond the fleet's card capacity","params":{"cap":1}}}]` {
		t.Fatalf("cards = %s", raw)
	}
	for _, g := range view.GPUs {
		raw, _ := json.Marshal(g)
		if g.PCIeBDF == "0000:3f:00.0" && (strings.Contains(string(raw), "scheduling_held_by") || strings.Contains(string(raw), "scheduling_hold")) {
			t.Fatalf("a free card carries hold fields: %s", raw)
		}
	}
	if status, body := callNode(t, h.setCardScheduling, http.MethodPut, "drain-a", "/gpus/scheduling", `{"pcie_bdf":"0000:40:00.0","scheduling":"stopped"}`); status != http.StatusOK || !strings.Contains(body, `"scheduling_held_by":"cap"`) {
		t.Fatalf("stop a held card = %d %s", status, body)
	}
}
