package controller

import (
	"context"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncAdoptionAndTerminalEventsKeepTheSameOwner(t *testing.T) {
	st, _, src, dst, mf := setupModelSyncController(t)
	key := store.ModelFileKey(mf.ID)
	for i := 0; i < 30; i++ {
		if err := st.DB().Model(&store.ModelFile{}).Where("id = ?", mf.ID).Updates(map[string]any{"state": proto.MFFailed, "transfer_id": "prior"}).Error; err != nil {
			t.Fatal(err)
		}
		hub := &modelSyncHub{copyResult: &proto.ModelSyncCopyResult{InProgress: true, TransferID: "adopted"}}
		c := New(st, hub, nil)
		ctx, cancel := context.WithCancel(context.Background())
		run := &modelSyncRun{transferID: "requested", cancel: cancel}
		c.modelSyncs[key] = run
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				c.releaseTerminalModelSync(key)
			}
		}()
		go func() {
			defer wg.Done()
			c.runModelSync(ctx, key, run, peerSource{node: src, path: "/models/Qwen3", sizeBytes: 12}, dst, mf, netplan.StoragePath{})
		}()
		wg.Wait()
		current, err := st.ModelFileByID(mf.ID)
		if err != nil || current.State != proto.MFSyncing || current.TransferID != "adopted" || c.modelSyncs[key] != run || ctx.Err() != nil {
			t.Fatalf("adoption lost ownership: row=%+v err=%v registered=%v context=%v", current, err, c.modelSyncs[key] == run, ctx.Err())
		}
		c.clearModelSync(key, run)
	}
}

func TestModelSyncLateAdoptionCannotOverwriteReplacement(t *testing.T) {
	st, model, src, dst, mf := setupModelSyncController(t)
	key := store.ModelFileKey(mf.ID)
	if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID, "replacement", 12); err != nil {
		t.Fatal(err)
	}
	hub := &modelSyncHub{copyResult: &proto.ModelSyncCopyResult{InProgress: true, TransferID: "stale-receiver"}}
	c := New(st, hub, nil)
	oldCtx, oldCancel := context.WithCancel(context.Background())
	oldCancel()
	replacementCtx, replacementCancel := context.WithCancel(context.Background())
	defer replacementCancel()
	oldRun := &modelSyncRun{transferID: "old-request", cancel: oldCancel}
	replacement := &modelSyncRun{transferID: "replacement", cancel: replacementCancel}
	c.modelSyncs[key] = replacement
	// An already buffered InProgress reply may arrive after the old run was
	// released and a replacement acquired the same model-file slot.
	c.runModelSync(oldCtx, key, oldRun, peerSource{node: src, path: "/models/Qwen3", sizeBytes: 12}, dst, mf, netplan.StoragePath{})
	current, err := st.ModelFileByID(mf.ID)
	if err != nil || current.State != proto.MFSyncing || current.TransferID != "replacement" || c.modelSyncs[key] != replacement || replacementCtx.Err() != nil {
		t.Fatalf("late receiver reply overwrote replacement: row=%+v err=%v registered=%v context=%v", current, err, c.modelSyncs[key] == replacement, replacementCtx.Err())
	}
	c.clearModelSync(key, replacement)
}
