package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/proto"
)

type activeModelSync struct {
	transferID string
	key        string
	receiver   *agentmodelsync.Receiver
	canceled   atomic.Bool
	relayDone  chan struct{}
}

func (n *NodeRuntime) reportModelSyncStatus(active *activeModelSync, status proto.ModelFileStatus) {
	if !active.canceled.Load() {
		n.reportModelFile(status)
	}
}

func (n *NodeRuntime) rpcModelSyncListen(params json.RawMessage) (any, error) {
	var p proto.ModelSyncListenParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.TransferID = strings.TrimSpace(p.TransferID)
	p.Key = strings.TrimSpace(p.Key)
	p.TargetPath = strings.TrimSpace(p.TargetPath)
	if p.TransferID == "" || p.Key == "" || p.TargetPath == "" || strings.TrimSpace(p.Token) == "" {
		return nil, errors.New("model sync listen requires transfer_id, key, target_path, and token")
	}
	cleanTarget := filepath.Clean(p.TargetPath)
	if filepath.IsAbs(cleanTarget) || cleanTarget == "." || cleanTarget == ".." || strings.HasPrefix(cleanTarget, ".."+string(filepath.Separator)) {
		return nil, errors.New("model sync target_path must stay inside the managed model directory")
	}
	p.TargetPath = cleanTarget
	// A model larger than the archive limit would be cut off only after its
	// limit's worth had crossed the network, so it is refused before listening.
	if !modelArchiveFits(p.ExpectedFileBytes) {
		return nil, fmt.Errorf("model sync of %d bytes exceeds the %d-byte transfer limit", p.ExpectedFileBytes, modelArchiveHardLimit)
	}
	n.syncMu.Lock()
	if current := n.modelSyncs[p.Key]; current != nil {
		n.syncMu.Unlock()
		return proto.ModelSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	n.syncMu.Unlock()

	bindHost, advertiseHosts, err := agentmodelsync.ListenAddrs(p.BindIP)
	if err != nil {
		return nil, err
	}
	root, ready, err := n.storageRoot(proto.ModelFileTask{StorageID: p.StorageID})
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, errors.New("the target storage is not ready on this node")
	}
	absTarget, err := resolveStorageTarget(root, storageTarget{Rel: p.TargetPath})
	if err != nil {
		return nil, err
	}
	if p.ExpectedFileBytes > 0 {
		if err := n.checkDownloadSpace(p.StorageID, absTarget, p.ExpectedFileBytes); err != nil {
			return nil, err
		}
	}
	started := time.Now()
	active := &activeModelSync{transferID: p.TransferID, key: p.Key}
	n.reportModelSyncStatus(active, proto.ModelFileStatus{
		Key: p.Key, State: proto.MFSyncing, Path: absTarget, BytesTotal: p.ExpectedFileBytes,
		TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
	})
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
		BindHost: bindHost, AdvertiseHosts: advertiseHosts, Token: p.Token,
		MaxBytes: modelArchiveLimit(p.ExpectedFileBytes),
		Receive: func(ctx context.Context, r io.Reader) error {
			return n.receiveModelArchiveAtomic(ctx, absTarget, r)
		},
		Progress: func(done int64) {
			seconds := time.Since(started).Seconds()
			var rate int64
			if seconds > 0 {
				rate = int64(float64(done) / seconds)
			}
			fileDone := done
			if p.ExpectedFileBytes > 0 && fileDone > p.ExpectedFileBytes {
				fileDone = p.ExpectedFileBytes
			}
			n.reportModelSyncStatus(active, proto.ModelFileStatus{
				Key: p.Key, State: proto.MFSyncing, Path: absTarget,
				BytesTotal: p.ExpectedFileBytes, BytesDone: fileDone, TransferBytesDone: done, RateBytesPerS: rate,
				TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
			})
		},
	})
	if err != nil {
		n.reportModelSyncStatus(active, proto.ModelFileStatus{
			Key: p.Key, State: proto.MFFailed, Path: absTarget, Message: boundedModelSyncMessage(err),
			TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
		})
		return nil, err
	}
	active.receiver = receiver
	n.syncMu.Lock()
	if current := n.modelSyncs[p.Key]; current != nil {
		n.syncMu.Unlock()
		receiver.Stop()
		return proto.ModelSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	n.modelSyncs[p.Key] = active
	n.syncMu.Unlock()
	go n.waitModelSync(active, p, absTarget)
	return proto.ModelSyncListenResult{Endpoints: receiver.Endpoints, Fingerprint: receiver.Fingerprint, TransferID: p.TransferID}, nil
}

func (n *NodeRuntime) waitModelSync(active *activeModelSync, p proto.ModelSyncListenParams, absTarget string) {
	defer func() {
		// Clean up before releasing the key so an old task cannot discard a retry.
		n.progress.discard(modelFileProgressKey(p.Key))
		n.syncMu.Lock()
		if n.modelSyncs[p.Key] == active {
			delete(n.modelSyncs, p.Key)
		}
		n.syncMu.Unlock()
	}()
	result, err := active.receiver.Wait(context.Background())
	if active.canceled.Load() {
		return
	}
	if err != nil {
		n.reportModelSyncStatus(active, proto.ModelFileStatus{
			Key: p.Key, State: proto.MFFailed, Path: absTarget, Message: boundedModelSyncMessage(err),
			BytesTotal: p.ExpectedFileBytes, TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
		})
		return
	}
	fileBytes, err := validateModelDir(absTarget)
	if err != nil {
		n.reportModelSyncStatus(active, proto.ModelFileStatus{
			Key: p.Key, State: proto.MFFailed, Path: absTarget, Message: boundedModelSyncMessage(err),
			TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
		})
		return
	}
	n.reportModelSyncStatus(active, proto.ModelFileStatus{
		Key: p.Key, State: proto.MFPresent, Path: absTarget,
		BytesTotal: fileBytes, BytesDone: fileBytes, TransferBytesDone: result.Bytes,
		Message:    fmt.Sprintf("peer sync verified: archive=%d sha256=%s", result.Bytes, result.SHA256),
		TransferID: p.TransferID, Acquisition: proto.ModelAcquirePeer,
	})
}

func (n *NodeRuntime) rpcModelSyncSend(ctx context.Context, params json.RawMessage) (any, error) {
	return n.modelSyncSend(ctx, params, nil)
}

func (n *NodeRuntime) modelSyncSend(ctx context.Context, params json.RawMessage, raw net.Conn) (any, error) {
	var p proto.ModelSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.SourcePath) == "" || strings.TrimSpace(p.Token) == "" || len(p.Endpoints) == 0 || strings.TrimSpace(p.Fingerprint) == "" {
		return nil, errors.New("model sync send parameters are incomplete")
	}
	localIP, err := agentmodelsync.LocalIP(p.SourceIP)
	if err != nil {
		return nil, err
	}
	transport := "direct"
	if raw != nil {
		transport = "relay"
	}
	observe := n.peerTransferPhaseReporter(p.ReportPhase, p.TransferID, transport)
	writeArchive := func(w io.Writer) error {
		return n.StreamModelArchive(ctx, p.SourcePath, agentmodelsync.RateLimitedWriter(ctx, w, p.RateLimitKBs))
	}
	var result agentmodelsync.Result
	if raw != nil {
		result, err = agentmodelsync.SendOnConnWithMagic(ctx, raw, p.Token, p.Fingerprint, "GPUWORKS-MODEL/1", writeArchive, observe)
	} else {
		result, err = agentmodelsync.Send(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, writeArchive, observe)
	}
	if err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ModelSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, err
	}
	return proto.ModelSyncTransferResult{Bytes: result.Bytes, SHA256: result.SHA256}, nil
}

func (n *NodeRuntime) rpcModelSyncProbe(ctx context.Context, params json.RawMessage) (any, error) {
	var p proto.ModelSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Token) == "" || strings.TrimSpace(p.Fingerprint) == "" || len(p.Endpoints) == 0 {
		return nil, errors.New("model sync probe parameters are incomplete")
	}
	localIP, err := agentmodelsync.LocalIP(p.SourceIP)
	if err != nil {
		return nil, err
	}
	if err := agentmodelsync.Probe(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, "GPUWORKS-MODEL/1"); err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ModelSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, fmt.Errorf("source node cannot connect to target direct address: %w", err)
	}
	return map[string]string{"state": "ready"}, nil
}

func (n *NodeRuntime) rpcModelSyncStop(params json.RawMessage) (any, error) {
	var p proto.ModelSyncStopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	n.syncMu.Lock()
	var active *activeModelSync
	if p.Key != "" {
		active = n.modelSyncs[p.Key]
	} else {
		for _, candidate := range n.modelSyncs {
			if candidate.transferID == p.TransferID {
				active = candidate
				break
			}
		}
	}
	n.syncMu.Unlock()
	if active == nil {
		return map[string]string{"status": "not_running"}, nil
	}
	if p.TransferID != "" && active.transferID != p.TransferID {
		return nil, errors.New("model sync transfer id mismatch")
	}
	// Mark intent before closing the connection that wakes waitModelSync.
	// Status sends already in flight may finish; cancellation must not wait for them.
	active.canceled.Store(true)
	active.receiver.Stop()
	return map[string]string{"status": "stopping"}, nil
}

// modelArchiveHardLimit bounds one peer model transfer. It is far above image
// archives' limit because a quantized trillion-parameter checkpoint alone is
// about 1.5 TB.
const modelArchiveHardLimit int64 = 4 << 40

// modelArchiveLimit is the most a transfer may carry: the expected file bytes
// plus 5% and 1 GiB for the archive's own framing, never above the hard limit.
func modelArchiveLimit(expectedFileBytes int64) int64 {
	if expectedFileBytes <= 0 || expectedFileBytes > modelArchiveHardLimit {
		return modelArchiveHardLimit
	}
	return min(expectedFileBytes+expectedFileBytes/20+1<<30, modelArchiveHardLimit)
}

// modelArchiveFits reports whether a model of the expected size fits its
// limit with that headroom. An unknown size (0) is bounded only by the hard
// limit while it streams.
func modelArchiveFits(expectedFileBytes int64) bool {
	return expectedFileBytes <= 0 || (expectedFileBytes <= modelArchiveHardLimit && expectedFileBytes+expectedFileBytes/20+1<<30 <= modelArchiveHardLimit)
}

func boundedModelSyncMessage(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 480 {
		message = message[:480]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}
