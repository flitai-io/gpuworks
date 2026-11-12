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
)

type archiveRelay func(context.Context, string, string, any) (json.RawMessage, error)

func coordinateModelCopyProbe(ctx context.Context, req proto.ModelSyncCopyRequest, token string, call modelSyncCall, relays ...archiveRelay) (proto.ModelSyncCopyResult, error) {
	req.TransferID = strings.TrimSpace(req.TransferID)
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	// Long for the same reason as coordinateModelCopy's listen.
	listenRaw, err := call(ctx, req.TargetNodeUUID, proto.RPCModelSyncListen, proto.ModelSyncListenParams{TransferID: req.TransferID, Key: req.Key, TargetPath: req.TargetPath, Token: token, BindIP: req.TargetIP, ExpectedFileBytes: req.ExpectedFileBytes, StorageID: req.TargetStorageID}, true)
	if err != nil {
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("start target model receiver: %w", err)
	}
	var listen proto.ModelSyncListenResult
	if err := json.Unmarshal(listenRaw, &listen); err != nil {
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, err
	}
	if listen.InProgress {
		return proto.ModelSyncCopyResult{TransferID: listen.TransferID, InProgress: true}, errors.New("target model receiver already has an active transfer")
	}
	if len(listen.Endpoints) == 0 || listen.Fingerprint == "" {
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, errors.New("target model receiver returned no endpoint or fingerprint")
	}
	_, transport, err := sendPeerArchive(ctx, req.SourceNodeUUID, proto.RPCModelSyncProbe, req.TransferID, proto.ModelSyncSendParams{TransferID: req.TransferID, Token: token, Endpoints: listen.Endpoints, Fingerprint: listen.Fingerprint, SourceIP: req.SourceIP, ReportPhase: true, AllowRelay: relayEnabled(req.AllowRelay, relays)}, false, req.AllowRelay, call, relays)
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = call(stopCtx, req.TargetNodeUUID, proto.RPCModelSyncStop, proto.ModelSyncStopParams{TransferID: req.TransferID, Key: req.Key}, false)
	if err != nil {
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("source node cannot connect to target: %w", err)
	}
	return proto.ModelSyncCopyResult{TransferID: req.TransferID, Transport: transport}, nil
}

type modelSyncCall func(context.Context, string, string, any, bool) (json.RawMessage, error)

func coordinateModelCopy(ctx context.Context, req proto.ModelSyncCopyRequest, token string, call modelSyncCall, relays ...archiveRelay) (proto.ModelSyncCopyResult, error) {
	req.TransferID = strings.TrimSpace(req.TransferID)
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	if req.Key == "" || req.SourceNodeUUID == "" || req.SourcePath == "" || req.TargetNodeUUID == "" || req.TargetPath == "" {
		return proto.ModelSyncCopyResult{}, errors.New("model sync copy request is incomplete")
	}
	if token == "" {
		return proto.ModelSyncCopyResult{}, errors.New("generate model sync token")
	}
	// stop releases the target's receiver for this transfer. It is safe to call
	// when no receiver exists (the agent answers "not_running") and when the
	// listen call itself failed: the reply can be lost after the agent has
	// already started receiving, and an orphan receiver is what makes every
	// retry answer InProgress until the receiver's 10-minute TTL expires,
	// leaving the model file in syncing with nothing downloaded (todo T23).
	stop := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = call(cleanupCtx, req.TargetNodeUUID, proto.RPCModelSyncStop, proto.ModelSyncStopParams{
			TransferID: req.TransferID, Key: req.Key,
		}, false)
	}
	// Listen is long like send: before it answers, the target resolves its
	// storage and checks free space, each bounded by the agent's own 30-second
	// storage timeout, so on slow storage the reply arrives after the ordinary
	// management deadline and the server would give up on a receiver that then
	// starts anyway.
	listenRaw, err := call(ctx, req.TargetNodeUUID, proto.RPCModelSyncListen, proto.ModelSyncListenParams{
		TransferID: req.TransferID, Key: req.Key, TargetPath: req.TargetPath, Token: token,
		BindIP: req.TargetIP, ExpectedFileBytes: req.ExpectedFileBytes, StorageID: req.TargetStorageID,
	}, true)
	if err != nil {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("start target model receiver: %w", err)
	}
	var listen proto.ModelSyncListenResult
	if err := json.Unmarshal(listenRaw, &listen); err != nil {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("decode target model receiver: %w", err)
	}
	// An in-progress answer belongs to an earlier request that owns that
	// receiver; stopping it here would cancel a transfer already running.
	if listen.InProgress {
		if listen.TransferID != "" {
			req.TransferID = listen.TransferID
		}
		return proto.ModelSyncCopyResult{TransferID: req.TransferID, InProgress: true}, nil
	}
	if len(listen.Endpoints) == 0 || listen.Fingerprint == "" {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, errors.New("target model receiver returned no endpoint or fingerprint")
	}
	sendRaw, transport, err := sendPeerArchive(ctx, req.SourceNodeUUID, proto.RPCModelSyncSend, req.TransferID, proto.ModelSyncSendParams{
		TransferID: req.TransferID, SourcePath: req.SourcePath, Token: token,
		Endpoints: listen.Endpoints, Fingerprint: listen.Fingerprint,
		RateLimitKBs: req.RateLimitKBs, SourceIP: req.SourceIP, ReportPhase: true, AllowRelay: relayEnabled(req.AllowRelay, relays),
	}, true, req.AllowRelay, call, relays)
	if err != nil {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("send peer model archive: %w", err)
	}
	var sent proto.ModelSyncTransferResult
	if err := json.Unmarshal(sendRaw, &sent); err != nil {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, fmt.Errorf("decode peer model confirmation: %w", err)
	}
	if sent.Bytes <= 0 || len(sent.SHA256) != 64 {
		stop()
		return proto.ModelSyncCopyResult{TransferID: req.TransferID}, errors.New("peer model confirmation is incomplete")
	}
	return proto.ModelSyncCopyResult{TransferID: req.TransferID, Bytes: sent.Bytes, SHA256: sent.SHA256, Transport: transport}, nil
}
