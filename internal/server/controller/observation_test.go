package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// observationHub records pushes; the refresher pushes from its own goroutine.
type observationHub struct {
	modelSyncHub
	pushed chan proto.StateUpdate
}

// RequestImageRefs records the refs as the push the hub would send.
func (h *observationHub) RequestImageRefs(uuid string, refs []string) {
	_ = h.PushToNode(uuid, proto.StateUpdate{ImageRefs: refs})
}

func (h *observationHub) PushToNode(uuid string, update proto.StateUpdate) error {
	_ = h.modelSyncHub.PushToNode(uuid, update)
	if h.pushed != nil {
		h.pushed <- update
	}
	return nil
}

func (h *observationHub) pushes() []proto.StateUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]proto.StateUpdate(nil), h.updates...)
}

type observationFixture struct {
	st       *store.Store
	hub      *observationHub
	ctrl     *Controller
	node     *store.Node
	dep      *store.Deployment
	template uint
}

func tensorfoldContract(rules map[string]string) string {
	contract := map[string]any{"operations": []map[string]any{{"type": "openai.chat", "endpoint": "/v1/chat/completions"}}}
	if rules != nil {
		contract["metrics"] = map[string]any{"map": rules}
	}
	raw, _ := json.Marshal(contract)
	return string(raw)
}

func (f *observationFixture) importTemplate(t *testing.T, contract string, overwrite bool) {
	t.Helper()
	model := store.InferenceModel{ModelID: "acme/tf", Source: "user", Specs: []store.InferenceSpecRow{{
		Name: "general", Engine: "callable", Image: "acme/tf:v1", Quantization: "fp8",
		WeightSources: `[]`, GPUs: `{}`, Args: `[]`, Env: `{}`, APIContract: contract,
	}}}
	if err := f.st.ImportUserInferenceCatalog([]store.InferenceModel{model}, overwrite); err != nil {
		t.Fatal(err)
	}
	stored, err := f.st.InferenceModelByModelID("acme/tf")
	if err != nil {
		t.Fatal(err)
	}
	f.template = stored.ID
}

// newObservationFixture is one vLLM deployment created from a user template,
// scheduled on one online node, so its instance carries a resolved
// observation.
func newObservationFixture(t *testing.T, contract string) *observationFixture {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_observation_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &observationFixture{st: st}
	f.importTemplate(t, contract, false)
	f.node, err = st.RegisterNode("observation-node-"+name, store.HashToken("token"), "observation-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: f.node.ID, Index: 0, UUID: "observation-gpu-" + name, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/observed-" + name, Name: "observed", WeightBytes: 1 << 20,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":512}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: f.node.ID, State: proto.MFPresent, Path: "/models/observed"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{Name: "observed", Engine: "vllm", Framework: "cuda", ImageRef: "catalog/observed:v1", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	f.dep, err = createTestDeployment(t, st, &store.Deployment{
		Name: "observed", ModelID: model.ID, NodeID: f.node.ID, ServedName: "observed", Engine: "vllm", Image: image.ImageRef,
		QuickParams: `{"max_model_len":512}`, State: "active",
		TemplateID: f.template, TemplateSpecSource: "user", TemplateSpecName: "general",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.hub = &observationHub{modelSyncHub: modelSyncHub{online: map[string]bool{f.node.UUID: true}}}
	f.ctrl = New(st, f.hub, nil)
	if err := f.ctrl.reconcileDeployment(context.Background(), f.dep.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *observationFixture) instance(t *testing.T) (store.Instance, proto.InstanceDesiredState) {
	t.Helper()
	instances, err := f.st.InstancesForDeployment(f.dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v, %v", instances, err)
	}
	return instances[0], desiredOf(t, instances[0])
}

func observedSeries(spec proto.InstanceDesiredState, source string) string {
	if spec.Observation == nil {
		return ""
	}
	for _, rule := range spec.Observation.MetricRules {
		if rule.Source == source {
			return rule.Series
		}
	}
	return ""
}

// The launch resolves the observation from the template's current spec: the
// template's own entries lie over the engine's built-in mapping, and the
// legacy fields an older agent reads carry the same mapping.
func TestLaunchStampsTheTemplateObservation(t *testing.T) {
	f := newObservationFixture(t, tensorfoldContract(map[string]string{
		"vllm:num_requests_running": "requests_waiting", // template wins
		"adapter:running":           "requests_running",
	}))
	_, spec := f.instance(t)
	if got := observedSeries(spec, "vllm:num_requests_running"); got != "gpuworks_infer_requests_waiting" {
		t.Fatalf("template entry = %q, observation %+v", got, spec.Observation)
	}
	if got := observedSeries(spec, "adapter:running"); got != "gpuworks_infer_requests_running" {
		t.Fatalf("added entry = %q", got)
	}
	if got := observedSeries(spec, "vllm:num_requests_waiting"); got != "gpuworks_infer_requests_waiting" {
		t.Fatalf("inherited entry = %q", got)
	}
	if spec.MetricPath != "/metrics" || spec.MetricPrefix != "vllm" || spec.MetricMap["adapter:running"] != "gpuworks_infer_requests_running" {
		t.Fatalf("legacy fields = %q %q %v", spec.MetricPath, spec.MetricPrefix, spec.MetricMap)
	}
}

// A template edit reaches the running instance in place: the stored desired
// state changes in the observation alone, the node gets one observation
// update and nothing else, and an unchanged template writes and sends nothing.
func TestRefreshUpdatesTheObservationInPlace(t *testing.T) {
	f := newObservationFixture(t, tensorfoldContract(nil))
	before, launched := f.instance(t)
	if observedSeries(launched, "tensorfold:requests_running") != "" {
		t.Fatal("undeclared mapping present")
	}
	pushedAtLaunch := len(f.hub.pushes())

	f.importTemplate(t, tensorfoldContract(map[string]string{"tensorfold:requests_running": "requests_running"}), true)
	if err := f.ctrl.refreshObservations([]uint{f.template}); err != nil {
		t.Fatal(err)
	}
	after, refreshed := f.instance(t)
	if got := observedSeries(refreshed, "tensorfold:requests_running"); got != "gpuworks_infer_requests_running" {
		t.Fatalf("refreshed observation = %+v", refreshed.Observation)
	}
	// Nothing about how the instance runs changed.
	launchOnly := func(spec proto.InstanceDesiredState) proto.InstanceDesiredState {
		spec.Observation, spec.MetricPath, spec.MetricPrefix, spec.MetricMap = nil, "", "", nil
		return spec
	}
	if !reflect.DeepEqual(launchOnly(launched), launchOnly(refreshed)) {
		t.Fatalf("launch fields changed:\n%+v\n%+v", launchOnly(launched), launchOnly(refreshed))
	}
	if after.State != before.State || after.NodeID != before.NodeID {
		t.Fatalf("instance row changed beyond its spec: %+v -> %+v", before, after)
	}

	pushes := f.hub.pushes()[pushedAtLaunch:]
	if len(pushes) != 1 || len(pushes[0].Observations) != 1 || len(pushes[0].Instances) != 0 || len(pushes[0].RemovedInstances) != 0 {
		t.Fatalf("pushes = %+v", pushes)
	}
	update := pushes[0].Observations[0]
	if update.UUID != after.UUID || !reflect.DeepEqual(update.Observation, refreshed.Observation) ||
		update.MetricPrefix != refreshed.MetricPrefix || !reflect.DeepEqual(update.MetricMap, refreshed.MetricMap) {
		t.Fatalf("update = %+v", update)
	}

	// A second pass over an unchanged template is a no-op.
	if err := f.ctrl.refreshObservations(nil); err != nil {
		t.Fatal(err)
	}
	if got := len(f.hub.pushes()) - pushedAtLaunch; got != 1 {
		t.Fatalf("unchanged refresh pushed: %d updates", got)
	}
	unchanged, _ := f.instance(t)
	if unchanged.DesiredStateJSON != after.DesiredStateJSON {
		t.Fatal("unchanged refresh rewrote the spec")
	}
}

// An offline node is not pushed to; it picks the stored observation up at
// its next state.sync. A headless rank is not observed and stays as it is.
func TestRefreshStoresForOfflineNodesAndSkipsHeadlessRanks(t *testing.T) {
	f := newObservationFixture(t, tensorfoldContract(nil))
	f.hub.online[f.node.UUID] = false
	headlessSpec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "headless-rank", Headless: true})
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "headless-rank", DeploymentID: f.dep.ID, NodeID: f.node.ID, State: proto.InstRunning, DesiredStateJSON: string(headlessSpec)}); err != nil {
		t.Fatal(err)
	}
	pushedBefore := len(f.hub.pushes())

	f.importTemplate(t, tensorfoldContract(map[string]string{"tensorfold:requests_running": "requests_running"}), true)
	if err := f.ctrl.refreshObservations([]uint{f.template}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.hub.pushes()); got != pushedBefore {
		t.Fatalf("pushed to an offline node: %+v", f.hub.pushes()[pushedBefore:])
	}
	instances, err := f.st.InstancesForDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range instances {
		spec := desiredOf(t, instance)
		if instance.UUID == "headless-rank" {
			if string(headlessSpec) != instance.DesiredStateJSON {
				t.Fatalf("headless rank rewritten: %s", instance.DesiredStateJSON)
			}
			continue
		}
		if observedSeries(spec, "tensorfold:requests_running") == "" {
			t.Fatalf("offline instance not stored: %+v", spec.Observation)
		}
	}

	// A refresh scoped to another template leaves this deployment alone.
	f.importTemplate(t, tensorfoldContract(nil), true)
	if err := f.ctrl.refreshObservations([]uint{f.template + 100}); err != nil {
		t.Fatal(err)
	}
	instances, err = f.st.InstancesForDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range instances {
		if instance.UUID != "headless-rank" && observedSeries(desiredOf(t, instance), "tensorfold:requests_running") == "" {
			t.Fatal("a refresh for another template changed this deployment")
		}
	}
}

// A deployment whose spec has since been deleted keeps the observation its
// snapshot's api contract declares; one with neither gets the engine default.
func TestObservationFallsBackToTheSnapshotContract(t *testing.T) {
	f := newObservationFixture(t, tensorfoldContract(nil))
	dep := *f.dep
	dep.TemplateSpecName = "deleted"
	snapshot, _ := json.Marshal(map[string]any{"api": json.RawMessage(tensorfoldContract(map[string]string{"snap:running": "requests_running"}))})
	dep.CatalogSelectionSnapshot = string(snapshot)
	observation := f.ctrl.observationFor(&dep)
	if len(observation.Instance.MetricRules) == 0 || observation.LegacyMap["snap:running"] != "gpuworks_infer_requests_running" {
		t.Fatalf("snapshot fallback = %+v", observation)
	}
	dep.CatalogSelectionSnapshot = ""
	if got := f.ctrl.observationFor(&dep); got.LegacyMap["snap:running"] != "" || got.LegacyMap["vllm:num_requests_running"] == "" {
		t.Fatalf("default = %+v", got.LegacyMap)
	}
	dep.CatalogSelectionSnapshot = "{"
	if got := f.ctrl.observationFor(&dep); got.LegacyMap["vllm:num_requests_running"] == "" {
		t.Fatalf("corrupt snapshot = %+v", got.LegacyMap)
	}
}

func TestObservationRefreshCoalescesRequests(t *testing.T) {
	r := newObservationRefresh()
	if _, pending := r.take(); pending {
		t.Fatal("pending without a request")
	}
	r.request("3")
	r.request("4")
	r.request("3")
	r.request("not-a-row")
	templates, pending := r.take()
	sort.Slice(templates, func(i, j int) bool { return templates[i] < templates[j] })
	if !pending || !reflect.DeepEqual(templates, []uint{3, 4}) {
		t.Fatalf("take = %v %v", templates, pending)
	}
	// "Every template" absorbs any row request made alongside it.
	r.request("5")
	r.request(store.InferenceTemplatesAll)
	r.request("6")
	if templates, pending := r.take(); !pending || templates != nil {
		t.Fatalf("take after all = %v %v", templates, pending)
	}
	if _, pending := r.take(); pending {
		t.Fatal("scope not cleared")
	}
	// The signal holds at most one wake-up however many requests arrived.
	if len(r.signal) != 1 {
		t.Fatalf("signal depth = %d", len(r.signal))
	}
}

// End to end through the bus: a catalog import reaches the running instance's
// agent without anyone calling the refresher.
func TestCatalogImportRefreshesThroughTheEventLoop(t *testing.T) {
	f := newObservationFixture(t, tensorfoldContract(nil))
	f.hub.pushed = make(chan proto.StateUpdate, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.ctrl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	f.importTemplate(t, tensorfoldContract(map[string]string{"tensorfold:requests_running": "requests_running"}), true)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case update := <-f.hub.pushed:
			if len(update.Observations) == 1 && update.Observations[0].MetricMap["tensorfold:requests_running"] != "" {
				return
			}
		case <-deadline:
			_, spec := f.instance(t)
			t.Fatalf("no observation update pushed; stored observation %+v (template %s)", spec.Observation, strconv.FormatUint(uint64(f.template), 10))
		}
	}
}
