package controller

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCopyIntoDisabledStorageRefused(t *testing.T) {
	for _, kind := range []string{proto.StorageLocal, proto.StorageNFS, proto.StorageNFSCache, proto.StorageSharedDir} {
		t.Run(kind, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			target, err := f.st.CreateStorage(&store.Storage{Name: "disabled-target", Type: kind, Enabled: false, AllNodes: true, Path: "/target", Export: "nfs:/target", CachePath: "/cache-target"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: f.nfs.Path, Writable: true}, proto.StorageStatus{ID: target.ID, State: proto.StorageStateReady, Path: target.Path, CachePath: target.CachePath, Writable: true})
			if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, State: proto.MFPresent, Path: "model"}); err != nil {
				t.Fatal(err)
			}
			dst := store.FileRef{StorageID: target.ID}
			if kind == proto.StorageLocal {
				dst.NodeID = f.node.ID
			}
			if _, err := f.c.Transfers().Copy(t.Context(), f.model.ID, store.FileRef{StorageID: f.nfs.ID}, dst, -1); !errors.Is(err, store.ErrStorageDisabled) {
				t.Fatalf("copy into disabled %s: %v", kind, err)
			}
			if len(f.hub.calls) > 0 || len(f.hub.updates) > 0 {
				t.Fatal("disabled copy reached agent")
			}
		})
	}
}
