package planning

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateModelDiskRefusalNamesTheAcquisitionTarget(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	f.outOfTeam.PathUsage = `[{"kind":"models","path":"/opt/gpuworks/models","available_bytes":1048576,"total_bytes":4294967296}]`
	if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
		t.Fatal(err)
	}
	out := f.evaluate(f.outOfTeam, nil)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("expected model disk refusal: %+v", out.Result)
	}
	storage, err := f.st.StorageByID(f.st.DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"required_bytes": int64(1073741824), "available_bytes": int64(1048576),
		"node": f.outOfTeam.DisplayName(), "node_uuid": f.outOfTeam.UUID,
		"model": "q05", "model_id": f.model.ID,
		"storage": storage.Name, "storage_id": storage.ID, "storage_type": "local",
		"path": "/opt/gpuworks/models/modelscope/Qwen_Qwen2.5-0.5B-Instruct-main",
	}
	if !reflect.DeepEqual(out.Result.Message.Params, want) {
		t.Fatalf("disk refusal params=%#v, want %#v", out.Result.Message.Params, want)
	}
	if len(out.Result.Explain.Filters) != 1 || !reflect.DeepEqual(out.Result.Explain.Filters[0], out.Result.Message) {
		t.Fatalf("filter explanation drops disk context: %+v", out.Result.Explain)
	}
}

func TestEvaluateRuntimeDiskRefusalDoesNotSuggestModelRegistration(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	f.outOfTeam.RuntimeDriver = "docker"
	f.outOfTeam.PathUsage = `[{"kind":"docker","path":"/var/lib/docker","available_bytes":1048576,"total_bytes":4294967296}]`
	out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", RuntimeImageBytes: 1073741824}).Evaluate(f.st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientRuntimeDisk {
		t.Fatalf("expected runtime disk refusal: %+v", out.Result)
	}
	want := map[string]any{
		"required_bytes": int64(1073741824), "available_bytes": int64(1048576),
		"node": f.outOfTeam.DisplayName(), "node_uuid": f.outOfTeam.UUID,
		"path": "/var/lib/docker",
	}
	if !reflect.DeepEqual(out.Result.Message.Params, want) {
		t.Fatalf("runtime refusal params=%#v, want %#v", out.Result.Message.Params, want)
	}
}

func TestEvaluateCacheDiskRefusalKeepsTheCacheStorage(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	if err := f.st.SetStoragePolicy(store.StoragePolicy{ReserveFreeBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
		t.Fatal(err)
	}
	component, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/encoder", WeightBytes: 2 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := f.st.CreateStorage(&store.Storage{Name: "encoder-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Path: "/mnt/encoder", Export: "cache:/encoder", CachePath: "/cache/encoder"}, []store.StorageNode{{NodeID: f.outOfTeam.ID, CachePath: "/node/cache/encoder"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: component.ID, StorageID: cache.ID, Path: "org/encoder", State: proto.MFPresent, SizeBytes: 2 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ApplyStorageStatus(f.outOfTeam.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: cache.ID, State: proto.StorageStateReady, Path: "/mnt/encoder", CachePath: "/node/cache/encoder"}}}); err != nil {
		t.Fatal(err)
	}
	paths, err := json.Marshal([]proto.PathUsage{
		{Kind: "models", Path: "/opt/gpuworks/models", AvailableBytes: 1 << 20, TotalBytes: 4 << 30},
		{Kind: "storage_cache", StorageID: cache.ID, Path: "/node/cache/encoder", AvailableBytes: 1 << 20, TotalBytes: 4 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.outOfTeam.PathUsage = string(paths)
	out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", ComponentArtifacts: []Artifact{{ModelID: component.ID, StorageBytes: 2 << 30}}}).Evaluate(f.st)
	params := out.Result.Message.Params
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("expected cache disk refusal: %+v", out.Result)
	}
	if params["storage_id"] != cache.ID || params["storage_type"] != proto.StorageNFSCache || params["path"] != "/node/cache/encoder" {
		t.Fatalf("cache failure misidentified its target: %#v", params)
	}
	if _, present := params["model_id"]; present {
		t.Fatalf("cache refusal incorrectly offers to register the primary model locally: %#v", params)
	}
	if params["available_bytes"] != int64(1<<20) || params["required_bytes"] != int64(2214592512) || params["need_disk_bytes"] != int64(2213543936) || params["model_bytes"] != int64(2<<30) {
		t.Fatalf("cache failure lost capacity or legacy facts: %#v", params)
	}
}

func TestEvaluateCompositeDiskRefusalDoesNotInventOneModelTarget(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
		t.Fatal(err)
	}
	out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", ComponentWeightBytes: 2 << 30}).Evaluate(f.st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("expected disk refusal: %+v", out.Result)
	}
	params := out.Result.Message.Params
	if params["required_bytes"] != int64(3<<30) {
		t.Fatalf("composite capacity=%#v", params)
	}
	for _, key := range []string{"available_bytes", "model_id", "storage_id", "storage_type", "path"} {
		if _, present := params[key]; present {
			t.Fatalf("aggregate component demand misidentified as one known target: %#v", params)
		}
	}
}

func TestEvaluateDiskRefusalUsesPartialCopyAndNodeStorageOverride(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
		t.Fatal(err)
	}
	storage, err := f.st.CreateStorage(&store.Storage{Name: "scratch", Type: proto.StorageLocal, Enabled: true, Path: "/configured/models"}, []store.StorageNode{{NodeID: f.outOfTeam.ID, Path: "/node/model-disk"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: storage.ID, NodeID: f.outOfTeam.ID, Path: "legacy/q05", State: proto.MFDownloading, SizeBytes: 1 << 30, Downloaded: 768 << 20}); err != nil {
		t.Fatal(err)
	}
	paths, err := json.Marshal([]proto.PathUsage{
		{Kind: "models", Path: "/opt/gpuworks/models", AvailableBytes: 3 << 30, TotalBytes: 4 << 30},
		{Kind: "storage", StorageID: storage.ID, Path: "/node/model-disk", AvailableBytes: 64 << 20, TotalBytes: 4 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.outOfTeam.PathUsage = string(paths)
	out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", ModelRules: store.SourceRules{SaveTo: storage.ID}}).Evaluate(f.st)
	params := out.Result.Message.Params
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("expected selected storage disk refusal: %+v", out.Result)
	}
	if params["path"] != "/node/model-disk/legacy/q05" || params["required_bytes"] != int64(268435456) || params["available_bytes"] != int64(67108864) || params["storage_id"] != storage.ID {
		t.Fatalf("partial copy refused with wrong path or capacity: %#v", params)
	}
}

func TestEvaluateRuntimeDiskRefusalDoesNotGuessAnUnreportedPath(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	f.outOfTeam.RuntimeDriver = "docker"
	f.outOfTeam.PathUsage = `[{"kind":"docker","available_bytes":1048576,"total_bytes":4294967296},{"kind":"docker","path":"/different-runtime","available_bytes":2097152,"total_bytes":4294967296}]`
	out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", RuntimeImageBytes: 1073741824}).Evaluate(f.st)
	params := out.Result.Message.Params
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientRuntimeDisk || params["available_bytes"] != int64(1048576) {
		t.Fatalf("runtime disk context: %+v", out.Result)
	}
	for _, key := range []string{"path", "model_id", "storage_type"} {
		if _, present := params[key]; present {
			t.Fatalf("runtime context invented %s: %#v", key, params)
		}
	}
}

func TestEvaluateDiskRefusalDoesNotAttributeAnotherDisksCapacity(t *testing.T) {
	for _, stalePath := range []string{"", "/previous-disk"} {
		name := "unreported-storage"
		if stalePath != "" {
			name = "storage-path-changed"
		}
		t.Run(name, func(t *testing.T) {
			f := newStorageAdmissionFixture(t)
			if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
				t.Fatal(err)
			}
			storage, err := f.st.CreateStorage(&store.Storage{Name: "large-disk", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/large-disk"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			paths := []proto.PathUsage{{Kind: "models", Path: "/opt/gpuworks/models", AvailableBytes: 1 << 20, TotalBytes: 4 << 30}}
			if stalePath != "" {
				paths = append(paths, proto.PathUsage{Kind: "storage", StorageID: storage.ID, Path: stalePath, AvailableBytes: 1 << 20, TotalBytes: 4 << 30})
			}
			encoded, err := json.Marshal(paths)
			if err != nil {
				t.Fatal(err)
			}
			f.outOfTeam.PathUsage = string(encoded)
			out := (Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", ModelRules: store.SourceRules{SaveTo: storage.ID}}).Evaluate(f.st)
			if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
				t.Fatalf("existing disk guard changed: %+v", out.Result)
			}
			params := out.Result.Message.Params
			if params["required_bytes"] != int64(1073741824) || params["node_uuid"] != f.outOfTeam.UUID {
				t.Fatalf("known request facts lost: %#v", params)
			}
			for _, key := range []string{"available_bytes", "model", "model_id", "storage", "storage_id", "storage_type", "path"} {
				if _, present := params[key]; present {
					t.Fatalf("unobserved target falsely attributed %s: %#v", key, params)
				}
			}
		})
	}
}
