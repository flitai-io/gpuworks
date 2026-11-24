package hub

import (
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRuntimeImageCopyTransfer(t *testing.T) {
	requests := make(chan proto.RuntimeImageCopyRequest, 2)
	h := newArchiveURLHub(t, "file:hub_runtime_copy?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string
			Params proto.RuntimeImageCopyRequest
		}
		if err := json.Unmarshal(env.D, &req); err != nil || req.Method != proto.RPCImageRuntimeCopy {
			return `{"error":"unexpected RPC"}`
		}
		requests <- req.Params
		return `{"result":{"state":"syncing"}}`
	})
	p := proto.RuntimeImageSelection{Runtime: "nerdctl", Namespace: "default", TargetNamespace: "k8s.io", Ref: h.image.ImageRef, Identity: "selected-image"}
	fetch := imagecatalog.FetchRequest{RuntimeImage: &p}
	row, err := h.hub.PullImage(t.Context(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	started := <-requests
	if started.RuntimeImageSelection != p || started.TransferID == "" || started.TransferID != row.TransferID || row.State != store.ImageStateSyncing || row.Source != proto.RuntimeImageCopySource {
		t.Fatalf("dispatch %+v / %+v", started, row)
	}
	_, err = h.hub.PullImage(t.Context(), h.image.ID, h.node.UUID, fetch)
	var busy *messages.ProductError
	if !errors.As(err, &busy) || busy.Code() != proto.ErrorLocalImageBusy || len(requests) != 0 {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	status := proto.ImageNodeStatus{ImageRef: p.Ref, Source: row.Source, State: "present", TransferID: started.TransferID, ImageID: "config-id"}
	if err = h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, status); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || got.State != store.ImageStatePresent || got.ImageDigestID != status.ImageID || got.TransferID != "" {
		t.Fatal("completion", got, err)
	}
}
