package controller

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// go test ./internal/server/controller -run TestCopyModelFileRejectsOccupiedSharedPath -count=1
func TestCopyModelFileRejectsOccupiedSharedPath(t *testing.T) {
	for _, peer := range []bool{false, true} {
		name := "same-node"
		if peer {
			name = "peer"
		}
		t.Run(name, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			writer := f.node
			if peer {
				writer = f.other
			}
			f.ready(t, writer, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/models", Writable: true})
			local := f.st.DefaultLocalStorageID()
			if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: local, NodeID: f.node.ID, Path: "/local/model", State: proto.MFPresent, SizeBytes: 15}); err != nil {
				t.Fatal(err)
			}
			owner, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: f.model.SourceID})
			if err != nil {
				t.Fatal(err)
			}
			path := store.SharedModelRelPath(f.model)
			occupied, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: owner.ID, StorageID: f.nfs.ID, Path: path, State: proto.MFPresent, SizeBytes: 15})
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.c.Transfers().Copy(context.Background(), f.model.ID,
				store.FileRef{StorageID: local, NodeID: f.node.ID}, store.FileRef{StorageID: f.nfs.ID}, -1)
			var conflict *store.ModelFilePathOccupiedError
			if !errors.As(err, &conflict) || conflict.Path != path || conflict.ModelID != owner.ID {
				t.Fatalf("copy error=%v", err)
			}
			if row, err := f.st.ModelFileAt(store.FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}); err != store.ErrNotFound {
				t.Fatalf("copy left row=%+v err=%v", row, err)
			}
			if row, err := f.st.ModelFileByID(occupied.ID); err != nil || row.State != proto.MFPresent || row.SizeBytes != 15 {
				t.Fatalf("owner changed: %+v err=%v", row, err)
			}
			if len(f.hub.calls) != 0 {
				t.Fatalf("conflict dispatched node operations: %v", f.hub.calls)
			}
		})
	}
}
