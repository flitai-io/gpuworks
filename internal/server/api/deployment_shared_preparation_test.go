package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentPreparationFollowsSharedStorageDownload(t *testing.T) {
	for _, kind := range []string{proto.StorageNFS, proto.StorageSharedDir, proto.StorageNFSCache} {
		t.Run(kind, func(t *testing.T) {
			st := modelEvictionStore(t, "shared-preparation-"+kind)
			model, node := modelEvictionFixture(t, st)
			if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
				t.Fatal(err)
			}
			storage, err := st.CreateStorage(&store.Storage{Name: "shared", Type: kind, Enabled: true, AllNodes: true, Path: "/shared", Export: "fixture:/models", CachePath: "/cache"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: proto.StorageStateReady, Path: "/shared", CachePath: "/cache", Writable: true}}}); err != nil {
				t.Fatal(err)
			}
			file := &store.ModelFile{ModelID: model.ID, StorageID: storage.ID, WriterNodeID: node.ID, Path: "model", State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, SizeBytes: 0, Downloaded: 40, RateBPS: 10, Message: "weights 2/5"}
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if kind == proto.StorageNFSCache {
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID, Path: "/cache/old", State: proto.MFFailed, Acquisition: proto.ModelAcquireCopy, TransferID: "stale"}); err != nil {
					t.Fatal(err)
				}
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "shared-download", ModelID: model.ID, NodeID: node.ID, SaveToStorageID: storage.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetDeploymentRequirements(dep.ID, map[string][]string{"gpuworks.ai/storage": {fmt.Sprint(storage.ID)}}, store.PlacementSpread); err != nil {
				t.Fatal(err)
			}
			prep := newTestPreparation()
			handler := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
			assertProgress := func(state, phase string, known bool) dto.ModelNodePresence {
				t.Helper()
				detail := handler.deploymentDTO(dep, false)
				list, err := deployment.NewListRows(st, prep, []store.Deployment{*dep}, nil)
				if err != nil {
					t.Fatal(err)
				}
				shown, recorded := deployment.ModelProgress(list, dep)
				if len(shown) != 1 || len(recorded) != 1 || !reflect.DeepEqual(shown, detail.ModelProgress) {
					t.Fatalf("list=%+v recorded=%+v detail=%+v", shown, recorded, detail.ModelProgress)
				}
				item := shown[0]
				if item.State != state || item.Downloaded != file.Downloaded || item.SizeBytes != file.SizeBytes || item.Message != file.Message || item.Preparation.Phase != phase || item.Preparation.TotalKnown != known {
					t.Fatalf("wrong shared progress: %+v view=%+v", item, item.Preparation)
				}
				for _, response := range []dto.Deployment{detail, handler.deploymentDTOFrom(list, dep, false)} {
					wire, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					var decoded map[string]any
					if err := json.Unmarshal(wire, &decoded); err != nil {
						t.Fatal(err)
					}
					progress := decoded["model_progress"].([]any)[0].(map[string]any)
					for key, want := range map[string]any{"node_uuid": node.UUID, "storage_id": float64(storage.ID), "state": state, "downloaded": float64(file.Downloaded), "size_bytes": float64(file.SizeBytes), "message": file.Message} {
						if progress[key] != want {
							t.Errorf("wire %s=%#v, want %#v", key, progress[key], want)
						}
					}
				}
				return item
			}
			assertProgress(proto.MFDownloading, "downloading", false)
			file.SizeBytes = 100
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if item := assertProgress(proto.MFDownloading, "downloading", true); item.RateBps != 10 || item.Progress != 0.4 || item.Preparation.Reason != "" {
				t.Fatalf("download=%+v view=%+v", item, item.Preparation)
			}
			file.State, file.Message = proto.MFFailed, "download interrupted"
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if item := assertProgress(proto.MFFailed, "waiting", true); item.RateBps != 0 || item.Preparation.Reason != "MODEL_PREPARATION_FAILED" || item.Preparation.Retryable {
				t.Fatalf("failed=%+v view=%+v", item, item.Preparation)
			}
			file.State, file.SizeBytes, file.Downloaded = proto.MFPresent, 100, 100
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if kind == proto.StorageNFSCache {
				// The shared download is complete: the node now reports its own
				// cache copy, without reusing a shared transfer observation.
				cacheFile, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID, Path: "/cache/model", State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: "new-cache", SizeBytes: 100})
				if err != nil {
					t.Fatal(err)
				}
				prep.record(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(cacheFile.ID), State: proto.MFDownloading, TransferID: "stale", BytesDone: 99, BytesTotal: 100, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseCopying, TotalKnown: true}})
				progress := handler.deploymentDTO(dep, false).ModelProgress
				if len(progress) != 1 || progress[0].State != proto.MFDownloading || progress[0].Downloaded != 0 || progress[0].Preparation.TotalKnown {
					t.Fatalf("cache copy borrowed upstream progress: %+v", progress)
				}
				prep.record(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(cacheFile.ID), State: proto.MFDownloading, TransferID: "new-cache", BytesDone: 20, BytesTotal: 100, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseCopying, TotalKnown: true}})
				progress = handler.deploymentDTO(dep, false).ModelProgress
				if len(progress) != 1 || progress[0].Downloaded != 20 || progress[0].Preparation.Phase != proto.ModelPhaseCopying {
					t.Fatalf("cache copy lost its own progress: %+v", progress)
				}
			} else {
				assertProgress(proto.MFPresent, "ready", false)
			}
		})
	}
}

// An inactive shared row is not a transfer to wait for. The scheduler's
// expired refusal must still fail the deployment, as it did before shared
// transfer progress was exposed.
func TestSharedModelStatesPreserveSchedulingAndFailurePriority(t *testing.T) {
	for _, state := range []string{proto.MFAbsent, proto.MFMissing, proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk} {
		t.Run(state, func(t *testing.T) {
			st := modelEvictionStore(t, "inactive-shared")
			model, node := modelEvictionFixture(t, st)
			storage, err := st.CreateStorage(&store.Storage{Name: "shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/shared", Export: "fixture:/models"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			file, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, WriterNodeID: node.ID, Path: "model", State: proto.MFRegistered, Acquisition: proto.ModelAcquireRegistered})
			if err != nil {
				t.Fatal(err)
			}
			if state == proto.MFAbsent {
				err = st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), State: proto.MFAbsent, Acquisition: proto.ModelAcquireRegistered})
			} else if state == proto.MFMissing {
				if err := st.SetModelFileState(file.ID, proto.MFPresent, ""); err != nil {
					t.Fatal(err)
				}
				_, err = st.ApplyStorageScan(storage.ID, store.ScanSighting{Covers: func(string) bool { return true }, Gone: func(string) bool { return true }})
			} else {
				err = st.SetModelFileState(file.ID, state, "shared transfer failed")
			}
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := st.ModelFileByID(file.ID)
			if err != nil || persisted.State != state {
				t.Fatalf("fixture state=%+v err=%v", persisted, err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "inactive-shared", ModelID: model.ID, NodeID: node.ID, SaveToStorageID: storage.ID})
			if err != nil {
				t.Fatal(err)
			}
			st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "missing_model"})
			if err := st.DB().Model(&store.InstanceEvent{}).Where("deployment_id = ?", dep.ID).Update("created_at", time.Now().Add(-48*time.Hour)).Error; err != nil {
				t.Fatal(err)
			}
			prep := newTestPreparation()
			handler := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
			list, err := deployment.NewListRows(st, prep, []store.Deployment{*dep}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, recorded := deployment.ModelProgress(list, dep)
			if state == proto.MFAbsent || state == proto.MFMissing {
				if len(recorded) != 0 {
					t.Errorf("inactive shared row is counted as a transfer: %+v", recorded)
				}
			} else if len(recorded) != 1 || recorded[0].State != state || deployment.DeriveRuntimeState(store.DeployActive, nil, recorded, false) != "failed" {
				t.Errorf("shared transfer failure lost priority: %+v", recorded)
			}
			for _, response := range []dto.Deployment{handler.deploymentDTO(dep, false), handler.deploymentDTOFrom(list, dep, false)} {
				if response.RuntimeState != "failed" {
					t.Errorf("inactive shared %s hides expired scheduling refusal: runtime=%s progress=%+v", state, response.RuntimeState, response.ModelProgress)
				}
			}
		})
	}
}
