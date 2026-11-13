package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (h *Hub) CopyImagePath(ctx context.Context, req proto.ImageSyncCopyRequest) (proto.ImageSyncCopyResult, error) {
	return h.copyImagePath(ctx, req, nil)
}

func (h *Hub) copyImagePath(ctx context.Context, req proto.ImageSyncCopyRequest, onBusy func()) (proto.ImageSyncCopyResult, error) {
	if h.session(strings.TrimSpace(req.SourceNodeUUID)) == nil {
		return proto.ImageSyncCopyResult{}, fmt.Errorf("source node is offline: %s", req.SourceNodeUUID)
	}
	if h.session(strings.TrimSpace(req.TargetNodeUUID)) == nil {
		return proto.ImageSyncCopyResult{}, fmt.Errorf("target node is offline: %s", req.TargetNodeUUID)
	}
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	ctx, finish := h.beginObservedPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: req.TransferID, Kind: "image", Operation: "copy", SourceNodeUUID: req.SourceNodeUUID, TargetNodeUUID: req.TargetNodeUUID, ResourceKey: req.ImageRef})
	result, err := coordinateImageCopy(ctx, req, auth.NewToken(32), h.callNodeForImageSync, false, h.observeArchiveRelay(req.TransferID, h.archiveRelayFor(req.SourceNodeUUID, req.TargetNodeUUID, "image"), onBusy))
	finish(err, result.InProgress)
	return result, err
}

// ProbeImagePath creates a short-lived receiver, performs the real TLS/token
// handshake from the selected source, then tears the receiver down. No image
// bytes are sent and no image ledger row is created.
func (h *Hub) ProbeImagePath(ctx context.Context, imageID uint, targetUUID, sourceUUID string) error {
	_, err := h.ProbeImagePathResult(ctx, imageID, targetUUID, sourceUUID)
	return err
}

func (h *Hub) ProbeImagePathResult(ctx context.Context, imageID uint, targetUUID, sourceUUID string) (proto.ImageSyncCopyResult, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return proto.ImageSyncCopyResult{}, err
	}
	target, err := h.store.NodeByUUID(targetUUID)
	if err != nil {
		return proto.ImageSyncCopyResult{}, err
	}
	sourceNode, err := h.store.NodeByUUID(sourceUUID)
	if err != nil {
		return proto.ImageSyncCopyResult{}, err
	}
	if h.session(sourceUUID) == nil || h.session(targetUUID) == nil {
		return proto.ImageSyncCopyResult{}, errors.New("source or target node is offline")
	}
	sourceRow, err := h.store.ImageNode(image.ID, sourceNode.ID)
	if err != nil {
		return proto.ImageSyncCopyResult{}, err
	}
	if sourceRow.State != store.ImageStatePresent {
		return proto.ImageSyncCopyResult{}, errors.New("selected source node does not have this image")
	}
	path, err := h.storagePath(sourceNode, target)
	if err != nil {
		return proto.ImageSyncCopyResult{}, err
	}
	req := proto.ImageSyncCopyRequest{ImageRef: image.ImageRef, SourceNodeUUID: sourceUUID, TargetNodeUUID: targetUUID, SourceIP: path.SourceIP, TargetIP: path.TargetIP, ExpectedBytes: sourceRow.SizeBytes, AllowRelay: network.PeerRelayAllowed(sourceNode, target)}
	req.TransferID = auth.NewUUID()
	ctx, finish := h.beginObservedPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: req.TransferID, Kind: "image", Operation: "probe", SourceNodeUUID: sourceUUID, TargetNodeUUID: targetUUID, ResourceKey: image.ImageRef})
	result, err := coordinateImageCopy(ctx, req, auth.NewToken(32), h.callNodeForImageSync, true, h.observeArchiveRelay(req.TransferID, h.archiveRelayFor(sourceUUID, targetUUID, "image"), nil))
	finish(err, result.InProgress)
	return result, err
}

type imageSyncCall func(context.Context, string, string, any, bool) (json.RawMessage, error)

func coordinateImageCopy(ctx context.Context, req proto.ImageSyncCopyRequest, token string, call imageSyncCall, probe bool, relays ...archiveRelay) (proto.ImageSyncCopyResult, error) {
	req.TransferID = strings.TrimSpace(req.TransferID)
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	if req.ImageRef == "" || req.SourceNodeUUID == "" || req.TargetNodeUUID == "" || req.SourceNodeUUID == req.TargetNodeUUID {
		return proto.ImageSyncCopyResult{}, errors.New("image sync copy request requires distinct source and target nodes")
	}
	if req.RateLimitKBs < 0 {
		return proto.ImageSyncCopyResult{}, errors.New("image sync rate limit must be non-negative")
	}
	listenRaw, err := call(ctx, req.TargetNodeUUID, proto.RPCImageSyncListen, proto.ImageSyncListenParams{
		TransferID: req.TransferID, ImageRef: req.ImageRef, Token: token,
		BindIP: req.TargetIP, ExpectedBytes: req.ExpectedBytes,
	}, false)
	if err != nil {
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("start target image receiver: %w", err)
	}
	var listen proto.ImageSyncListenResult
	if err := json.Unmarshal(listenRaw, &listen); err != nil {
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("decode target image receiver: %w", err)
	}
	if listen.InProgress {
		if probe {
			return proto.ImageSyncCopyResult{TransferID: listen.TransferID, InProgress: true}, errors.New("target image receiver already has an active transfer")
		}
		return proto.ImageSyncCopyResult{TransferID: listen.TransferID, InProgress: true}, nil
	}
	if len(listen.Endpoints) == 0 || listen.Fingerprint == "" {
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, errors.New("target image receiver returned no endpoint or fingerprint")
	}
	params := proto.ImageSyncSendParams{TransferID: req.TransferID, ImageRef: req.ImageRef, Token: token, Endpoints: listen.Endpoints, Fingerprint: listen.Fingerprint, RateLimitKBs: req.RateLimitKBs, SourceIP: req.SourceIP, ReportPhase: true, AllowRelay: relayEnabled(req.AllowRelay, relays)}
	if probe {
		_, transport, err := sendPeerArchive(ctx, req.SourceNodeUUID, proto.RPCImageSyncProbe, req.TransferID, params, false, req.AllowRelay, modelSyncCall(call), relays)
		stopImageReceiver(call, req.TargetNodeUUID, req.TransferID)
		if err != nil {
			return proto.ImageSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("source node cannot connect to target: %w", err)
		}
		return proto.ImageSyncCopyResult{TransferID: req.TransferID, Transport: transport}, nil
	}
	sendRaw, transport, err := sendPeerArchive(ctx, req.SourceNodeUUID, proto.RPCImageSyncSend, req.TransferID, params, true, req.AllowRelay, modelSyncCall(call), relays)
	if err != nil {
		stopImageReceiver(call, req.TargetNodeUUID, req.TransferID)
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("send peer image archive: %w", err)
	}
	var sent proto.ImageSyncTransferResult
	if err := json.Unmarshal(sendRaw, &sent); err != nil {
		stopImageReceiver(call, req.TargetNodeUUID, req.TransferID)
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("decode peer image confirmation: %w", err)
	}
	if sent.Bytes <= 0 || len(sent.SHA256) != 64 {
		stopImageReceiver(call, req.TargetNodeUUID, req.TransferID)
		return proto.ImageSyncCopyResult{TransferID: req.TransferID}, errors.New("peer image confirmation is incomplete")
	}
	return proto.ImageSyncCopyResult{TransferID: req.TransferID, Bytes: sent.Bytes, SHA256: sent.SHA256, Transport: transport}, nil
}

func stopImageReceiver(call imageSyncCall, target, transferID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = call(ctx, target, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: transferID}, false)
}

func (h *Hub) callNodeForImageSync(ctx context.Context, uuid, method string, params any, long bool) (json.RawMessage, error) {
	s := h.session(uuid)
	if s == nil {
		return nil, errors.New("node not connected: " + uuid)
	}
	if long {
		return s.CallLong(ctx, method, params)
	}
	return s.Call(ctx, method, params)
}
