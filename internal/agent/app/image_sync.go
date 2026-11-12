package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	imageSyncMagic             = "GPUWORKS-IMAGE/1"
	imageArchiveMaxBytes int64 = 500 << 30
)

type activeImageSync struct {
	transferID string
	imageRef   string
	receiver   *agentmodelsync.Receiver
	reported   bool
	relayDone  chan struct{}
}

type readWriteReader struct{ io.Reader }

func (readWriteReader) Write(p []byte) (int, error) {
	return 0, errors.New("image receiver is read-only")
}

func (n *NodeRuntime) rpcImageSyncListen(params json.RawMessage) (any, error) {
	var p proto.ImageSyncListenParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.TransferID, p.ImageRef, p.Token = strings.TrimSpace(p.TransferID), strings.TrimSpace(p.ImageRef), strings.TrimSpace(p.Token)
	if p.TransferID == "" || p.ImageRef == "" || p.Token == "" {
		return nil, errors.New("image sync listen requires transfer_id, image_ref, and token")
	}
	if p.ExpectedBytes < 0 || p.ExpectedBytes > imageArchiveMaxBytes {
		return nil, fmt.Errorf("image archive exceeds %d-byte limit", imageArchiveMaxBytes)
	}
	d := n.runtimeDriver()
	if _, ok := d.(runtime.ImageArchiver); !ok {
		return nil, fmt.Errorf("runtime %s does not support direct image save/load", n.RuntimeDriverName())
	}
	if p.ExpectedBytes > 0 {
		if err := checkModelDownloadSpace("/", p.ExpectedBytes); err != nil {
			return nil, fmt.Errorf("target disk precheck failed: %w", err)
		}
	}
	n.imageSyncMu.Lock()
	if current := n.imageSyncs[p.ImageRef]; current != nil {
		n.imageSyncMu.Unlock()
		return proto.ImageSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	n.imageSyncMu.Unlock()
	bindHost, advertise, err := agentmodelsync.ListenAddrs(p.BindIP)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	active := &activeImageSync{transferID: p.TransferID, imageRef: p.ImageRef}
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
		BindHost: bindHost, AdvertiseHosts: advertise, Token: p.Token, Magic: imageSyncMagic,
		MaxBytes: imageArchiveMaxBytes,
		Receive: func(ctx context.Context, r io.Reader) error {
			return n.streamImage(ctx, p.ImageRef, p.TransferID, readWriteReader{Reader: r}, true, func() {
				n.imageSyncMu.Lock()
				active.reported = true
				if n.imageSyncs[p.ImageRef] == active {
					delete(n.imageSyncs, p.ImageRef)
				}
				n.imageSyncMu.Unlock()
			})
		},
		Progress: func(done int64) {
			// The receiver flushes its final byte count after Receive returns,
			// which is after StreamImage already reported the terminal state.
			// A late "syncing" frame would reopen a finished row.
			n.imageSyncMu.Lock()
			reported := active.reported
			n.imageSyncMu.Unlock()
			if reported {
				return
			}
			seconds := time.Since(started).Seconds()
			rate := int64(0)
			if seconds > 0 {
				rate = int64(float64(done) / seconds)
			}
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.ImageRef, State: "syncing", SizeBytes: p.ExpectedBytes, TransferredBytes: done, RateBPS: rate, TransferID: p.TransferID, Source: "peer"})
		},
	})
	if err != nil {
		return nil, err
	}
	active.receiver = receiver
	n.imageSyncMu.Lock()
	if current := n.imageSyncs[p.ImageRef]; current != nil {
		n.imageSyncMu.Unlock()
		receiver.Stop()
		return proto.ImageSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	n.imageSyncs[p.ImageRef] = active
	n.imageSyncMu.Unlock()
	go n.waitImageSync(active, p)
	return proto.ImageSyncListenResult{Endpoints: receiver.Endpoints, Fingerprint: receiver.Fingerprint, TransferID: p.TransferID}, nil
}

func (n *NodeRuntime) waitImageSync(active *activeImageSync, p proto.ImageSyncListenParams) {
	_, err := active.receiver.Wait(context.Background())
	n.imageSyncMu.Lock()
	if n.imageSyncs[p.ImageRef] == active {
		delete(n.imageSyncs, p.ImageRef)
	}
	n.imageSyncMu.Unlock()
	n.imageSyncMu.Lock()
	reported := active.reported
	n.imageSyncMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) && !reported {
		n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.ImageRef, State: "failed", TransferID: p.TransferID, Source: "peer", Message: err.Error()})
	}
}

func (n *NodeRuntime) rpcImageSyncSend(ctx context.Context, params json.RawMessage) (any, error) {
	return n.imageSyncSend(ctx, params, nil)
}

func (n *NodeRuntime) imageSyncSend(ctx context.Context, params json.RawMessage, raw net.Conn) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.TransferID = strings.TrimSpace(p.TransferID)
	p.ImageRef, p.Token, p.Fingerprint = strings.TrimSpace(p.ImageRef), strings.TrimSpace(p.Token), strings.TrimSpace(p.Fingerprint)
	if p.ImageRef == "" || p.Token == "" || p.Fingerprint == "" || len(p.Endpoints) == 0 {
		return nil, errors.New("image sync send parameters are incomplete")
	}
	if p.RateLimitKBs < 0 {
		return nil, errors.New("image sync rate limit must be non-negative")
	}
	if p.TransferID == "" {
		return nil, errors.New("image sync send requires transfer_id")
	}
	ctx, cancel := context.WithCancel(ctx)
	n.imageSyncMu.Lock()
	if n.imageSyncSends == nil {
		n.imageSyncSends = make(map[string]context.CancelFunc)
	}
	if _, exists := n.imageSyncSends[p.TransferID]; exists {
		n.imageSyncMu.Unlock()
		cancel()
		return nil, errors.New("image sync sender already owns this transfer_id")
	}
	n.imageSyncSends[p.TransferID] = cancel
	n.imageSyncMu.Unlock()
	defer func() {
		cancel()
		n.imageSyncMu.Lock()
		delete(n.imageSyncSends, p.TransferID)
		n.imageSyncMu.Unlock()
	}()
	d, ok := n.runtimeDriver().(runtime.ImageArchiver)
	if !ok {
		return nil, fmt.Errorf("runtime %s does not support direct image save/load", n.RuntimeDriverName())
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
		return d.SaveImage(ctx, p.ImageRef, agentmodelsync.RateLimitedWriter(ctx, w, p.RateLimitKBs))
	}
	var result agentmodelsync.Result
	if raw != nil {
		result, err = agentmodelsync.SendOnConnWithMagic(ctx, raw, p.Token, p.Fingerprint, imageSyncMagic, writeArchive, observe)
	} else {
		result, err = agentmodelsync.SendWithMagic(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, imageSyncMagic, writeArchive, observe)
	}
	if err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ImageSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, err
	}
	return proto.ImageSyncTransferResult{Bytes: result.Bytes, SHA256: result.SHA256}, nil
}

func (n *NodeRuntime) rpcImageSyncProbe(ctx context.Context, params json.RawMessage) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Token) == "" || strings.TrimSpace(p.Fingerprint) == "" || len(p.Endpoints) == 0 {
		return nil, errors.New("image sync probe parameters are incomplete")
	}
	localIP, err := agentmodelsync.LocalIP(p.SourceIP)
	if err != nil {
		return nil, err
	}
	if err := agentmodelsync.Probe(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, imageSyncMagic); err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ImageSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, fmt.Errorf("source node cannot connect to target direct address: %w", err)
	}
	return map[string]string{"state": "ready"}, nil
}

func (n *NodeRuntime) rpcImageSyncStop(_ context.Context, params json.RawMessage) (any, error) {
	var p proto.ImageSyncStopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.TransferID) == "" {
		return nil, errors.New("image sync stop requires transfer_id")
	}
	n.imageSyncMu.Lock()
	if cancel := n.imageSyncSends[p.TransferID]; cancel != nil {
		n.imageSyncMu.Unlock()
		cancel()
		return map[string]string{"status": "stopping"}, nil
	}
	var active *activeImageSync
	for _, candidate := range n.imageSyncs {
		if p.TransferID == "" || candidate.transferID == p.TransferID {
			active = candidate
			break
		}
	}
	n.imageSyncMu.Unlock()
	if active == nil {
		return map[string]string{"status": "not_running"}, nil
	}
	n.imageSyncMu.Lock()
	active.reported = true
	n.imageSyncMu.Unlock()
	active.receiver.Stop()
	return map[string]string{"status": "stopping"}, nil
}
