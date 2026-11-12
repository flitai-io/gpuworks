package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCanceledSharedDownloadCannotChangeWriterWithoutStopAcknowledgement(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	row, err := r.st.PrepareModelDownload(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}, "org/model")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CancelModelDownload(row.ID, row.TransferID, "canceled"); err != nil {
		t.Fatal(err)
	}
	// A disconnected writer has not acknowledged the persisted cancellation.
	r.h.d.CallNode = func(_ context.Context, _, _ string, _ any) (json.RawMessage, error) {
		return nil, errors.New("old writer offline")
	}
	if _, err := r.h.StartSharedDownloadOn(r.model, r.nfs, ""); err == nil || KindOf(err) != KindConflict {
		t.Fatalf("unacknowledged writer replaced: kind=%d err=%v", KindOf(err), err)
	}
	got, _ := r.st.ModelFileByID(row.ID)
	if got.State != proto.MFCanceled || got.TransferID != row.TransferID {
		t.Fatalf("failed handoff changed row: %+v", got)
	}
	r.h.d.CallNode = func(_ context.Context, uuid, method string, p any) (json.RawMessage, error) {
		args := p.(proto.ModelCopyStopParams)
		if uuid != r.node.UUID || method != proto.RPCModelDownloadStop || args.Key != store.ModelFileKey(row.ID) || args.TransferID != row.TransferID {
			t.Fatalf("stop request=%s %s %+v", uuid, method, args)
		}
		return json.RawMessage(`{"stopped":true}`), nil
	}
	if _, err := r.h.StartSharedDownloadOn(r.model, r.nfs, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = r.st.ModelFileByID(row.ID)
	if got.State != proto.MFPending || got.TransferID == row.TransferID {
		t.Fatalf("acknowledged retry did not replace intent: %+v", got)
	}
}
