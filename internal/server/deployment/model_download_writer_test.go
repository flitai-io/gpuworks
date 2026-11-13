package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Both requests observe the canceled row before either stop acknowledgement
// returns. Only one may claim the next shared transfer and its writer.
func TestSharedDownloadConcurrentRetryClaimsWriterOnce(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	other, err := r.st.RegisterNode("other-writer", store.HashToken("other"), "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.ApplyStorageStatus(other.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: proto.StorageStateReady, Writable: true, Path: r.nfs.Path}}}); err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}
	row, err := r.st.PrepareModelDownload(ref, store.SharedModelRelPath(r.model))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CancelModelDownload(row.ID, row.TransferID, "canceled"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	r.h.d.CallNode = func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCModelDownloadStop {
			return nil, fmt.Errorf("unexpected RPC %s", method)
		}
		entered <- struct{}{}
		select {
		case <-release:
			return json.RawMessage(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Each request answers whether it started the download or was refused
	// as a conflict.
	results := make(chan string, 2)
	for _, uuid := range []string{r.node.UUID, other.UUID} {
		go func(uuid string) {
			writer, err := r.h.StartSharedDownloadOn(r.model, r.nfs, uuid)
			switch {
			case err == nil && writer != nil:
				results <- "started"
			case KindOf(err) == KindConflict:
				results <- "conflict"
			default:
				results <- fmt.Sprintf("writer=%v err=%v", writer, err)
			}
		}(uuid)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not observe same canceled transfer")
		}
	}
	close(release)
	first, second := <-results, <-results
	if !((first == "started" && second == "conflict") || (second == "started" && first == "conflict")) {
		t.Fatalf("competing shared writers accepted: %s, %s", first, second)
	}
}
