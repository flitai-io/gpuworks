package controller

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeletingServiceWithdrawsOnlyItsUnneededDownload(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{true: "shared", false: "local"}[shared], func(t *testing.T) {
			f := newStorageControllerFixture(t)
			storage := f.nfs
			if !shared {
				storage, _ = f.st.StorageByID(f.st.DefaultLocalStorageID())
			}
			dep := f.deployment(t, storage)
			ref := store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID}
			if shared {
				f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
				ref.StorageID = f.nfs.ID
				ref.NodeID = 0
			}
			row, err := f.st.PrepareModelDownload(ref, "Qwen/model")
			if err != nil {
				t.Fatal(err)
			}
			if shared {
				if err := f.st.SetModelFileWriter(row.ID, f.node.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.st.SetDownloadDeployment(row.ID, row.TransferID, dep.ID); err != nil {
				t.Fatal(err)
			}
			other := f.deployment(t, storage)
			if err := f.st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
				t.Fatal(err)
			}
			if err := f.c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := f.st.ModelFileByID(row.ID)
			if got.State != proto.MFPending || got.DownloadDeploymentID != other.ID {
				t.Fatalf("another service lost download: %+v", got)
			}
			if err := f.st.SetDeploymentState(other.ID, store.DeployDeleting); err != nil {
				t.Fatal(err)
			}
			if err := f.c.reconcileDeployment(context.Background(), other.ID); err != nil {
				t.Fatal(err)
			}
			got, _ = f.st.ModelFileByID(row.ID)
			if got.State != proto.MFCanceled || got.TransferID != row.TransferID {
				t.Fatalf("deleted service left download running: %+v", got)
			}
			updates := f.hub.updates[f.node.UUID]
			if len(updates) == 0 || len(updates[len(updates)-1].ModelFiles) == 0 || updates[len(updates)-1].ModelFiles[0].DesiredState == "present" {
				t.Fatalf("writer did not receive withdrawal: %+v", updates)
			}
		})
	}
}

func TestDeletingServicePreservesIndependentDownload(t *testing.T) {
	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.nfs)
	row, err := f.st.PrepareModelDownload(store.FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}, "Qwen/model")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetModelFileWriter(row.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if err := f.c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.ModelFileByID(row.ID)
	if got.State != proto.MFPending {
		t.Fatalf("independent download canceled: %+v", got)
	}
}

func TestDeletingServiceDoesNotHandOffToUnrelatedSameModelLocation(t *testing.T) {
	f := newStorageControllerFixture(t)
	local, _ := f.st.StorageByID(f.st.DefaultLocalStorageID())
	owner := f.deployment(t, local)
	row, err := f.st.PrepareModelDownload(store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID}, "Qwen/model")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetDownloadDeployment(row.ID, row.TransferID, owner.ID); err != nil {
		t.Fatal(err)
	}
	// The other service already has its model on another node; it does not
	// need this unfinished download even though its ModelID is identical.
	other := f.deployment(t, local)
	if err := f.st.DB().Model(other).Updates(map[string]any{"node_id": f.other.ID, "node_ids": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, NodeID: f.other.ID, State: proto.MFPresent, Path: "/models/qwen"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetDeploymentState(owner.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if err := f.c.reconcileDeployment(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.ModelFileByID(row.ID)
	if got.State != proto.MFCanceled {
		t.Fatalf("unrelated service inherited orphan download: %+v", got)
	}
}

func TestDeletingServiceWithdrawsFailedDownloadBeforeReconnect(t *testing.T) {
	for _, state := range []string{proto.MFFailed, proto.MFInsufficientDisk} {
		t.Run(state, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			local, _ := f.st.StorageByID(f.st.DefaultLocalStorageID())
			owner := f.deployment(t, local)
			row, err := f.st.PrepareModelDownload(store.FileRef{ModelID: f.model.ID, NodeID: f.node.ID}, "Qwen/model")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.SetDownloadDeployment(row.ID, row.TransferID, owner.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(row.ID), State: state, TransferID: row.TransferID, Message: "temporary failure"}); err != nil {
				t.Fatal(err)
			}
			if err := f.st.SetDeploymentState(owner.ID, store.DeployDeleting); err != nil {
				t.Fatal(err)
			}
			if err := f.c.reconcileDeployment(context.Background(), owner.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := f.st.ModelFileByID(row.ID)
			if got.State != proto.MFCanceled {
				t.Fatalf("failed owned download survived deletion: %+v", got)
			}
			ds, err := f.st.DesiredStateForNode(f.node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ds.ModelFiles) != 1 || ds.ModelFiles[0].DesiredState == "present" {
				t.Fatalf("reconnect revives orphan: %+v", ds.ModelFiles)
			}
		})
	}
}

func TestLocalServiceDownloadRecordsItsInitiatingService(t *testing.T) {
	f := newStorageControllerFixture(t)
	local, _ := f.st.StorageByID(f.st.DefaultLocalStorageID())
	owner := f.deployment(t, local)
	if err := f.c.acquireLocalModel(context.Background(), f.node, owner, local, f.model, nil, nil); err != nil {
		t.Fatal(err)
	}
	row, err := f.st.ModelFileAt(store.FileRef{ModelID: f.model.ID, StorageID: local.ID, NodeID: f.node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if row.Acquisition != proto.ModelAcquireDownload || row.DownloadDeploymentID != owner.ID {
		t.Fatalf("service download lost owner: %+v", row)
	}
}
