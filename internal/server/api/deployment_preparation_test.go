package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentPreparationUsesActualNodeComponentsAndChosenStorage(t *testing.T) {
	st := modelEvictionStore(t, "preparation-dependencies")
	primary, legacy := modelEvictionFixture(t, st)
	actual, err := st.RegisterNode("actual-node", store.HashToken("actual-token"), "actual-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(actual.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	component, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "component", Name: "component"})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := st.CreateStorage(&store.Storage{Name: "chosen", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/chosen"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Ready copies on the default storage must not hide the pinned dependency.
	for _, id := range []uint{primary.ID, component.ID} {
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: id, NodeID: actual.ID, State: proto.MFPresent, Path: "/wrong"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: primary.ID, NodeID: actual.ID, StorageID: storage.ID, State: proto.MFPresent, Path: "/chosen/primary"}); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PrepareModelCopy(store.FileRef{ModelID: component.ID, StorageID: storage.ID, NodeID: actual.ID}, "/chosen/component")
	if err != nil {
		t.Fatal(err)
	}
	report := proto.ModelFileStatus{Key: store.ModelFileKey(pending.ID), State: proto.MFDownloading, TransferID: pending.TransferID, Acquisition: proto.ModelAcquireCopy, BytesTotal: 100, BytesDone: 40, RateBytesPerS: 10, Preparation: &proto.ModelPreparation{Attempt: "a", Phase: proto.ModelPhaseCopying, TotalKnown: true}}
	prep := newTestPreparation()
	prep.record(actual.ID, report)
	if err := st.UpdateModelFileStatus(actual.ID, report); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "dependency", ModelID: primary.ID, NodeID: legacy.ID, CatalogSelectionSnapshot: fmt.Sprintf(`{"components":[{"name":"encoder","store_model_id":%d}]}`, component.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentRequirements(dep.ID, map[string][]string{"gpuworks.ai/storage": {fmt.Sprint(storage.ID)}}, store.PlacementSpread); err != nil {
		t.Fatal(err)
	}
	prep.nodes[dep.ID] = []uint{actual.ID}
	handler := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
	detail := handler.deploymentDTO(dep, false)
	if len(detail.ModelProgress) != 2 {
		t.Fatalf("dependencies=%+v", detail.ModelProgress)
	}
	for _, row := range detail.ModelProgress {
		if row.NodeUUID != actual.UUID || row.StorageID != storage.ID || row.Path == "/wrong" {
			t.Fatalf("wrong dependency: %+v", row)
		}
	}
	summary := deploymentPreparationSummary(dep, detail.ModelProgress)
	if summary == nil || summary.ModelID != component.ID || summary.Downloaded != 40 || summary.Preparation.Phase != "copying" {
		t.Fatalf("summary=%+v", summary)
	}
	list, err := deployment.NewListRows(st, prep, []store.Deployment{*dep}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := deployment.ModelProgress(list, dep); !reflect.DeepEqual(got, detail.ModelProgress) {
		t.Fatalf("list=%+v detail=%+v", got, detail.ModelProgress)
	}
	wire, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip["model_id"] != float64(component.ID) || roundTrip["storage_id"] != float64(storage.ID) || roundTrip["downloaded"] != float64(40) {
		t.Fatalf("wire=%s", wire)
	}
}

// A model with no row yet on a node is shown as waiting, but only recorded
// rows reach the runtime state: a refused schedule with nothing being
// prepared stays a failure instead of reading as pending forever.
func TestDeploymentProgressWithoutRowsLeavesRuntimeStateToScheduling(t *testing.T) {
	st := modelEvictionStore(t, "preparation-no-rows")
	model, node := modelEvictionFixture(t, st)
	dep, err := st.CreateDeployment(&store.Deployment{Name: "unrecorded", ModelID: model.ID, NodeID: node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ModelFilesForNode(node.ID); len(rows) != 0 {
		t.Fatalf("fixture has rows: %+v", rows)
	}
	prep := newTestPreparation()
	lookup := deployment.NewStoreRows(st, prep)
	shown, recorded := deployment.ModelProgress(lookup, dep)
	if len(shown) != 1 || shown[0].ModelID != model.ID || shown[0].Preparation == nil || shown[0].Preparation.Phase != "waiting" {
		t.Fatalf("shown=%+v", shown)
	}
	if len(recorded) != 0 {
		t.Fatalf("an absent row was recorded: %+v", recorded)
	}
	if got := deployment.DeriveRuntimeState(store.DeployActive, nil, recorded, true); got != "failed" {
		t.Fatalf("refused schedule without rows reads %q", got)
	}
	ref := store.FileRef{ModelID: model.ID, NodeID: node.ID, StorageID: shown[0].StorageID}
	prep.phases[ref] = "evicting"
	if shown, _ := deployment.ModelProgress(lookup, dep); shown[0].Preparation.Phase != "evicting" {
		t.Fatalf("eviction phase not shown before the row exists: %+v", shown[0].Preparation)
	}
}
