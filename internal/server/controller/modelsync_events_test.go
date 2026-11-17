package controller

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncNonterminalEventKeepsAdmissionAlive(t *testing.T) {
	st, _, _, _, mf := setupModelSyncController(t)
	for _, state := range []string{proto.MFPending, proto.MFDownloading, proto.MFSyncing} {
		t.Run(state, func(t *testing.T) {
			if err := st.DB().Model(&store.ModelFile{}).Where("id = ?", mf.ID).Updates(map[string]any{"state": state, "transfer_id": "starting"}).Error; err != nil {
				t.Fatal(err)
			}
			c := New(st, &modelSyncHub{}, nil)
			ctx, cancel := context.WithCancel(context.WithoutCancel(context.Background()))
			defer cancel()
			key := store.ModelFileKey(mf.ID)
			run := &modelSyncRun{transferID: "starting", cancel: cancel}
			// Admission installs this run before BeginModelSync changes pending to
			// syncing. A queued preparation event may be handled in that window.
			c.syncs.register(key, run)
			c.releaseTerminalModelSync(key)
			if ctx.Err() != nil || c.syncs.current(key) != run {
				t.Fatalf("%s event interrupted admission: context=%v registered=%v", state, ctx.Err(), c.syncs.current(key) == run)
			}
		})
	}
}

func TestModelSyncOldTerminalEventCannotCancelNewAdmission(t *testing.T) {
	st, _, _, _, mf := setupModelSyncController(t)
	for _, oldID := range []string{"previous", ""} {
		t.Run("previous_id_"+oldID, func(t *testing.T) {
			if err := st.DB().Model(&store.ModelFile{}).Where("id = ?", mf.ID).Updates(map[string]any{"state": proto.MFFailed, "transfer_id": oldID}).Error; err != nil {
				t.Fatal(err)
			}
			c := New(st, &modelSyncHub{}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			key := store.ModelFileKey(mf.ID)
			run := &modelSyncRun{transferID: "replacement", cancel: cancel}
			// A new attempt owns the map while its BeginModelSync write is still
			// pending. The old terminal row is not a result of this new attempt.
			c.syncs.register(key, run)
			c.releaseTerminalModelSync(key)
			if ctx.Err() != nil || c.syncs.current(key) != run {
				t.Fatalf("old terminal row interrupted replacement: context=%v registered=%v", ctx.Err(), c.syncs.current(key) == run)
			}
		})
	}
}

func TestModelSyncTerminalEventReleasesOnlyItsAttempt(t *testing.T) {
	st, _, _, _, mf := setupModelSyncController(t)
	for _, state := range []string{proto.MFPresent, proto.MFRegistered, proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk, proto.MFAbsent, proto.MFMissing, proto.MFDeleting} {
		t.Run(state, func(t *testing.T) {
			if err := st.DB().Model(&store.ModelFile{}).Where("id = ?", mf.ID).Updates(map[string]any{"state": state, "transfer_id": "owned"}).Error; err != nil {
				t.Fatal(err)
			}
			c := New(st, &modelSyncHub{}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			key := store.ModelFileKey(mf.ID)
			c.syncs.register(key, &modelSyncRun{transferID: "owned", cancel: cancel})
			c.releaseTerminalModelSync(key)
			if c.syncs.current(key) != nil {
				t.Fatal("terminal attempt retained its registration")
			}
			if ready := proto.ModelFileStateReady(state); ready && ctx.Err() != nil {
				t.Fatalf("verified completion canceled the source confirmation: %v", ctx.Err())
			} else if !ready && ctx.Err() != context.Canceled {
				t.Fatalf("failed/withdrawn attempt was not canceled: %v", ctx.Err())
			}
		})
	}
}
