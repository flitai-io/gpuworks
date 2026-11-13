package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImageLoadPathDispatchesToTargetWithoutReadingServerFile(t *testing.T) {
	requests := make(chan proto.ImageLoadPathParams, 1)
	h := newArchiveURLHub(t, "file:hub_local_image?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string                    `json:"method"`
			Params proto.ImageLoadPathParams `json:"params"`
		}
		_ = json.Unmarshal(env.D, &req)
		if req.Method != proto.RPCImageLoadPath {
			return `{"error":"unexpected RPC"}`
		}
		requests <- req.Params
		return `{"result":{"state":"syncing"}}`
	})
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceLocal, ArchivePath: "/only/on/target/image.tar"}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.ArchivePath != fetch.ArchivePath || got.CanonicalRef != h.image.ImageRef || got.TransferID == "" || got.TransferID != row.TransferID || row.Source != imagecatalog.FetchSourceLocal {
		t.Fatalf("dispatch = %+v, row = %+v", got, row)
	}
	// A duplicate click must leave the existing attempt in charge.
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil || again.TransferID != row.TransferID || len(requests) != 0 {
		t.Fatalf("duplicate request = %+v, %v", again, err)
	}
}

func TestImageLoadPathOldAgentLeavesRetryableFailure(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_local_old_agent?mode=memory&cache=shared", func(proto.Envelope) string {
		return `{"error":"unknown rpc method: image.load_path"}`
	})
	_, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceLocal, ArchivePath: "/data/image.tar"})
	if err == nil || !strings.Contains(err.Error(), "upgrade the agent") {
		t.Fatalf("expected upgrade message, got %v", err)
	}
	row, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || row.State != store.ImageStateFailed || row.Source != imagecatalog.FetchSourceLocal {
		t.Fatalf("failed import = %+v, %v", row, err)
	}
}
