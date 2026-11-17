package hub

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// loadImageArchiveURL asks a node to download an image archive published over
// plain HTTP and load it under the catalog reference. The server only
// coordinates: it learns the archive's size so the node can refuse a download
// its disk cannot hold, then hands the address over. The bytes never pass
// through here.
//
// Progress and the terminal state arrive through status.image_node exactly as
// they do for a registry pull, so the ledger row, the detail endpoint and the
// UI need nothing new.
func (h *Hub) loadImageArchiveURL(ctx context.Context, image *store.Image, target *store.Node, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
	archiveURL, err := imagecatalog.NormalizeArchiveURL(fetch.ArchiveURL)
	if err != nil {
		return nil, err
	}
	if row, rowErr := h.store.ImageNode(image.ID, target.ID); rowErr == nil {
		if imageRowActive(row) && !(fetch.Force && row.State == store.ImageStatePresent) {
			return row, nil
		}
	} else if !errors.Is(rowErr, store.ErrNotFound) {
		return nil, rowErr
	}
	// Best effort: a mirror that answers HEAD lets the node run its disk
	// admission check and show a real progress bar. One that does not still
	// gets the download, just without either.
	sizeBytes := int64(0)
	client := imagecatalog.ArchiveHTTPClient()
	client.Transport = h.outboundTransport()
	defer client.CloseIdleConnections()
	if size, headErr := imagecatalog.ArchiveURLSize(ctx, client, archiveURL); headErr == nil {
		sizeBytes = size
	} else {
		h.log.Debug("image archive size unavailable", "image", image.ImageRef, "host", imagecatalog.ArchiveURLHost(archiveURL), "err", headErr)
	}
	transferID := auth.NewUUID()
	host := imagecatalog.ArchiveURLHost(archiveURL)
	source, message := imagecatalog.FetchSourceURL, "downloading image archive from "+host
	if fetch.Relay {
		source, message = imagecatalog.FetchSourceURLRelay, "relaying image archive from "+host
	}
	row, started, err := h.beginImageFetch(ctx, &store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID,
		State: store.ImageStateSyncing, Source: source,
		SizeBytes: sizeBytes, TransferID: transferID,
		Message: message,
	}, fetch.Force)
	if err != nil || !started {
		return row, err
	}
	if fetch.Relay {
		h.relayImageArchiveURL(ctx, image, target, archiveURL, transferID, sizeBytes)
		return row, nil
	}
	if _, callErr := h.CallNode(ctx, target.UUID, proto.RPCImageLoadURL, proto.ImageLoadURLParams{
		CanonicalRef: image.ImageRef, ArchiveURL: archiveURL, SizeBytes: sizeBytes, TransferID: transferID,
	}); callErr != nil {
		message := "image archive download could not be started: " + callErr.Error()
		if errors.Is(callErr, proto.ErrRPCUnknownMethod) {
			message = "the target node is running an older agent that cannot download an image archive from a URL; upgrade the agent first"
		}
		_ = h.store.FailImageTransferIfActive(image.ID, target.ID, transferID, boundedPullMessage(message))
		return nil, errors.New(message)
	}
	// The node downloads the archive itself, so the row only moves while the
	// agent reports. Watch for those reports to stop.
	h.watchDirectImageTransfer(image.ID, target.ID)
	return row, nil
}

// relayImageArchiveURL streams the archive through the management server into
// the node's load stream, for a node that cannot reach the mirror itself. The
// ledger row is the same one a peer transfer uses, driven by the same
// trackImageTransfer state machine; only the source of the bytes differs, and
// that is what ImageNode.Source records.
func (h *Hub) relayImageArchiveURL(ctx context.Context, image *store.Image, target *store.Node, archiveURL, transferID string, sizeBytes int64) {
	h.trackImageTransfer(ctx, image.ID, target.ID, transferID, sizeBytes, func(copyCtx context.Context, progress func(int64)) (int64, error) {
		return h.CopyImageArchiveURL(copyCtx, archiveURL, target.UUID, image.ImageRef, transferID, progress)
	})
}
