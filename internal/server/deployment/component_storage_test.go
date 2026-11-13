package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// componentRig is the save-to rig plus a template component (a text encoder
// the template fetches beside the primary).
func componentRig(t *testing.T) (*saveToRig, templateComponent) {
	t.Helper()
	r := newSaveToRig(t)
	model, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/base", Revision: "abc", ArtifactSubfolder: "text_encoder"})
	if err != nil {
		t.Fatal(err)
	}
	binding := deploy.ComponentBinding{Name: "text-encoder", Role: "auxiliary", StoreModelID: model.ID, WeightBytes: 66_726_510_529}
	return r, templateComponent{binding: binding, model: model}
}

func (r *saveToRig) localHolds(t *testing.T, model *store.Model, node *store.Node) {
	t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: r.st.DefaultLocalStorageID(), NodeID: node.ID, Path: store.SharedModelRelPath(model), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
}

func (r *saveToRig) nfsHolds(t *testing.T, model *store.Model) {
	t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: r.nfs.ID, Path: store.SharedModelRelPath(model), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
}

func (r *saveToRig) plan(t *testing.T, source *dto.ModelSource, component templateComponent, pinned []uint, nodes ...*store.Node) componentPlan {
	t.Helper()
	if len(nodes) == 0 {
		nodes = []*store.Node{r.node}
	}
	saveTo, err := r.h.resolveSaveToStorage(source, r.model, nodes)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := r.h.planComponents(source, r.model, []templateComponent{component}, saveTo, pinned, nodes)
	if err != nil || len(plans) != 1 {
		t.Fatalf("planComponents = %+v, %v", plans, err)
	}
	return plans[0]
}

// How a template component reaches the target nodes, by where the primary and
// the component already are. MiniMax-H3-Image on the H20 server was the
// "primary on the NFS, component nowhere" case: the text encoder went into
// every node's local storage. With no storage named it now follows the
// primary into that NFS, written once; every other case keeps the default
// order of store.ModelSourceOn.
func TestPlanComponentsByWhereTheModelsAre(t *testing.T) {
	type want struct {
		plan    string
		storage bool
		follows bool
		ready   int
	}
	cases := []struct {
		name     string
		setup    func(t *testing.T, r *saveToRig, c templateComponent)
		source   *dto.ModelSource
		pinned   func(r *saveToRig) []uint
		want     want
		writable bool
	}{
		{name: "nothing anywhere, no storage named", writable: true,
			setup: func(*testing.T, *saveToRig, templateComponent) {},
			want:  want{plan: ComponentPlanNodeDownload}},
		{name: "primary on the NFS, component nowhere", writable: true,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			want:  want{plan: ComponentPlanSharedDownload, storage: true, follows: true}},
		{name: "primary on a read-only NFS", writable: false,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			want:  want{plan: ComponentPlanNodeDownload}},
		{name: "both on the NFS", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) { r.nfsHoldsModel(t); r.nfsHolds(t, c.model) },
			want:  want{plan: ComponentPlanReady, ready: 1}},
		{name: "only the primary on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.localHolds(t, r.model, r.node) },
			want:  want{plan: ComponentPlanNodeDownload}},
		{name: "only the component on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) { r.localHolds(t, c.model, r.node) },
			want:  want{plan: ComponentPlanReady, ready: 1}},
		{name: "both on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) {
				r.localHolds(t, r.model, r.node)
				r.localHolds(t, c.model, r.node)
			},
			want: want{plan: ComponentPlanReady, ready: 1}},
		{name: "a local storage named", writable: true,
			setup:  func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			source: &dto.ModelSource{StorageID: 0}, // replaced below with the default local storage
			want:   want{plan: ComponentPlanNodeDownload}},
		{name: "pinned to local storage", writable: true,
			setup:  func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			pinned: func(r *saveToRig) []uint { return []uint{r.st.DefaultLocalStorageID()} },
			want:   want{plan: ComponentPlanNodeDownload}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, component := componentRig(t)
			r.nfsState(t, proto.StorageStateReady, tc.writable)
			tc.setup(t, r, component)
			source := tc.source
			if source != nil && source.StorageID == 0 {
				source = &dto.ModelSource{StorageID: r.st.DefaultLocalStorageID()}
			}
			var pinned []uint
			if tc.pinned != nil {
				pinned = tc.pinned(r)
			}
			got := r.plan(t, source, component, pinned)
			if got.plan != tc.want.plan || (got.storage != nil) != tc.want.storage || got.followsPrimary != tc.want.follows || got.readyNodes != tc.want.ready || got.nodes != 1 {
				t.Fatalf("plan = %s storage=%v follows=%v ready=%d/%d; want %+v", got.plan, got.storage, got.followsPrimary, got.readyNodes, got.nodes, tc.want)
			}
			if got.storage != nil && got.storage.ID != r.nfs.ID {
				t.Fatalf("storage = %s, want %s", got.storage.Name, r.nfs.Name)
			}
		})
	}
}

func TestPlanComponentsWithASharedStorageNamed(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	got := r.plan(t, &dto.ModelSource{StorageID: r.nfs.ID}, component, nil)
	if got.plan != ComponentPlanSharedDownload || got.storage == nil || got.storage.ID != r.nfs.ID || got.followsPrimary {
		t.Fatalf("plan = %+v, want the named NFS, not following the primary", got)
	}
}

// One node of two already holds the component: the create still fetches it
// once into the NFS for the other, and the forecast says how many are ready.
func TestPlanComponentsCountsReadyNodes(t *testing.T) {
	r, component := componentRig(t)
	second, err := r.st.RegisterNode("gpu-b", store.HashToken("b"), "gpu-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*store.Node{r.node, second} {
		if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: r.nfs.ID, State: proto.StorageStateReady, Path: r.nfs.Path, Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	r.nfsHoldsModel(t)
	r.localHolds(t, component.model, r.node)
	got := r.plan(t, nil, component, nil, r.node, second)
	if got.plan != ComponentPlanSharedDownload || !got.followsPrimary || got.readyNodes != 1 || got.nodes != 2 {
		t.Fatalf("plan = %s follows=%v ready=%d/%d", got.plan, got.followsPrimary, got.readyNodes, got.nodes)
	}
}

// A write of the component into the NFS that someone else started is not one
// this deployment's nodes wait for, so the create does not claim it: each
// node fetches the component itself, and the forecast says so.
func TestPlanComponentsLeavesAWriteSomeoneElseStarted(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	r.nfsHoldsModel(t)
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: component.model.ID, StorageID: r.nfs.ID, Path: store.SharedModelRelPath(component.model), State: proto.MFDownloading, WriterNodeID: r.node.ID, DownloadDeploymentID: 99}); err != nil {
		t.Fatal(err)
	}
	if got := r.plan(t, nil, component, nil); got.plan != ComponentPlanNodeDownload || got.storage != nil {
		t.Fatalf("plan = %+v, want node_download", got)
	}
}

// The create acts on the plan: one write owned by the deployment, through a
// writer node, recorded on the deployment's timeline; the deployment's nodes
// wait for it, another deployment's do not.
func TestStartComponentDownloadsWritesOnceAndSaysSo(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	r.nfsHoldsModel(t)
	r.h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { return nil }}
	dep, err := r.st.CreateDeploymentWithRequirements(&store.Deployment{Name: "h3-image", ModelID: r.model.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := r.plan(t, nil, component, nil)
	if err := r.h.startComponentDownloads([]componentPlan{plan}, []*store.Node{r.node}, dep.ID); err != nil {
		t.Fatalf("startComponentDownloads = %v", err)
	}
	row, err := r.st.ModelFileAt(store.FileRef{ModelID: component.model.ID, StorageID: r.nfs.ID})
	if err != nil || row.State != proto.MFPending || row.WriterNodeID != r.node.ID || row.DownloadDeploymentID != dep.ID {
		t.Fatalf("component row = %+v, %v", row, err)
	}
	events, err := r.st.DeploymentEvents(dep.ID, 10)
	if err != nil || len(events) != 1 || events[0].Code != store.EventDeploymentComponentDownload {
		t.Fatalf("events = %+v, %v", events, err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(events[0].Data), &params); err != nil {
		t.Fatal(err)
	}
	if params["component"] != "text-encoder" || params["storage"] != r.nfs.Name || params["node"] != r.node.DisplayName() ||
		params["bytes"] != float64(66_726_510_529) || params["follows_primary"] != true || !strings.Contains(params["model"].(string), "org/base") {
		t.Fatalf("event params = %v", params)
	}
	src, err := r.st.ModelSourceOn(component.model.ID, r.node, store.SourceRules{Deployment: dep.ID})
	if err != nil || src.Action != store.SourceWaitShared || src.Location.Storage.ID != r.nfs.ID {
		t.Fatalf("own deployment source = %+v, %v; want to wait for %s", src, err, r.nfs.Name)
	}
	if src, err := r.st.ModelSourceOn(component.model.ID, r.node, store.SourceRules{Deployment: dep.ID + 1}); err != nil || src.Action != store.SourceAcquire {
		t.Fatalf("another deployment's source = %+v, %v; want to acquire", src, err)
	}
	// Nothing more to write: a second call starts nothing and records nothing.
	if err := r.h.startComponentDownloads([]componentPlan{{templateComponent: component, plan: ComponentPlanReady}}, []*store.Node{r.node}, dep.ID); err != nil {
		t.Fatal(err)
	}
	if events, _ := r.st.DeploymentEvents(dep.ID, 10); len(events) != 1 {
		t.Fatalf("a ready component recorded %d events", len(events))
	}
}

// The forecast's wire form: plan, counts, storage and the size the template
// declares.
func TestAcquisitionComponentsWire(t *testing.T) {
	r, component := componentRig(t)
	encoded, err := json.Marshal(acquisitionComponents([]componentPlan{
		{templateComponent: component, plan: ComponentPlanSharedDownload, nodes: 2, readyNodes: 1, storage: r.nfs, followsPrimary: true},
		{templateComponent: component, plan: ComponentPlanReady, nodes: 2, readyNodes: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"text-encoder","role":"auxiliary","model":"` + component.model.DisplayName() + `","size_bytes":66726510529,"plan":"shared_download","nodes":2,"ready_nodes":1,"storage_id":` + jsonUint(r.nfs.ID) + `,"storage":"team-nfs","follows_primary":true},` +
		`{"name":"text-encoder","role":"auxiliary","model":"` + component.model.DisplayName() + `","size_bytes":66726510529,"plan":"ready","nodes":2,"ready_nodes":2}]`
	if string(encoded) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", encoded, want)
	}
}

func TestTemplateComponentsSkipsThePrimary(t *testing.T) {
	r, component := componentRig(t)
	got, err := r.h.templateComponents([]deploy.ComponentBinding{
		{Name: "image", Role: "primary", StoreModelID: r.model.ID},
		component.binding,
		{Name: "unresolved", Role: "auxiliary"},
	})
	if err != nil || len(got) != 1 || got[0].model.ID != component.model.ID || got[0].binding.Name != "text-encoder" {
		t.Fatalf("templateComponents = %+v, %v", got, err)
	}
	if _, err := r.h.templateComponents([]deploy.ComponentBinding{{Name: "gone", Role: "auxiliary", StoreModelID: 9999}}); err == nil {
		t.Fatal("a component whose model row is gone must fail the create")
	}
}
