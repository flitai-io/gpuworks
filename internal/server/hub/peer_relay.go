package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// archiveRelayFor captures both current sessions. A reconnection must obtain new
// receiver credentials rather than silently moving an authorized transfer.
func (h *Hub) archiveRelayFor(sourceUUID, targetUUID, kind string) archiveRelay {
	source, target := h.session(sourceUUID), h.session(targetUUID)
	if sourceUUID == targetUUID || !peerRelaySessionReady(source) || !peerRelaySessionReady(target) {
		return nil
	}
	return func(ctx context.Context, method, transferID string, params any) (json.RawMessage, error) {
		h.mu.Lock()
		if h.sessions[sourceUUID] != source || h.sessions[targetUUID] != target || !peerRelaySessionReady(source) || !peerRelaySessionReady(target) {
			h.mu.Unlock()
			return nil, errors.New("peer relay sessions changed or data lane unavailable")
		}
		h.mu.Unlock()
		lease, err := h.peerManager().acquire(ctx, transferID, sourceUUID, targetUUID, peerRelayUserRate(params))
		if err != nil {
			return nil, err
		}
		defer lease.release()
		return relayPeerArchive(lease.ctx, source, target, kind, method, transferID, limitPeerRelay(params), lease)
	}
}

func peerRelaySessionReady(s *Session) bool {
	return s != nil && s.HasCap(proto.CapPeerRelayV1) && s.DataLaneState() == "attached"
}

func limitPeerRelay(params any) any {
	switch p := params.(type) {
	case proto.ModelSyncSendParams:
		p.RateLimitKBs = 0
		return p
	case proto.ImageSyncSendParams:
		p.RateLimitKBs = 0
		return p
	default:
		return params
	}
}

// peerRelayUserRate retains a lower user cap on the server. The source is
// unthrottled only for relay, so encrypted bytes are charged exactly once.
func peerRelayUserRate(params any) int64 {
	var rate int
	switch p := params.(type) {
	case proto.ModelSyncSendParams:
		rate = p.RateLimitKBs
	case proto.ImageSyncSendParams:
		rate = p.RateLimitKBs
	}
	if rate <= 0 {
		return 0
	}
	if int64(rate) > int64(^uint64(0)>>1)/1024 {
		return 0
	}
	return int64(rate) * 1024
}

func relayPeerArchive(ctx context.Context, source, target *Session, kind, method, transferID string, params any, lease *peerRelayLease) (json.RawMessage, error) {
	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()
	targetStream, err := target.openStream(relayCtx, proto.StreamHeader{Kind: proto.StreamPeerRelayReceive, Target: kind, Ref: transferID})
	if err != nil {
		return nil, err
	}
	defer targetStream.Close()
	// A target data failure must release server resources even if its source
	// producer is blocked and control is still healthy.
	watcherDone := make(chan struct{})
	lane := targetStream.(*laneStream).ysess
	go func() {
		defer close(watcherDone)
		select {
		case <-lane.CloseChan():
			cancelRelay()
		case <-relayCtx.Done():
		}
	}()
	defer func() { cancelRelay(); <-watcherDone }()
	stop := context.AfterFunc(relayCtx, func() { targetStream.Close() })
	defer stop()
	if err := readPeerRelayReady(relayCtx, targetStream); err != nil {
		return nil, err
	}
	var bridge sync.WaitGroup
	raw, err := source.CallOnDataStream(relayCtx, proto.StreamPeerRelaySend, method, params, func(sourceStream net.Conn) error {
		bridge.Add(2)
		copyTo := func(dst, src net.Conn, forward bool) {
			defer bridge.Done()
			_, copyErr := lease.copy(dst, src, forward)
			if copyErr != nil {
				sourceStream.Close()
				targetStream.Close()
				return
			}
			if forward {
				lease.sourceDrained()
			}
			if half, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			}
		}
		go copyTo(targetStream, sourceStream, true)
		go copyTo(sourceStream, targetStream, false)
		return nil
	})
	// Source's ctrl RPC carries the receiver's authenticated archive confirmation.
	// EOF alone never confirms a transfer. Close and join both pumps on all paths.
	targetStream.Close()
	bridge.Wait()
	// Wait for the receiver attachment to be released on the remote agent, not
	// merely for local pumps to stop. A probe can then safely reuse its receiver.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, releaseErr := target.Call(releaseCtx, proto.RPCPeerRelayWaitClosed, proto.PeerRelayWaitClosedParams{Kind: kind, TransferID: transferID})
	if err != nil {
		if context.Cause(ctx) != nil {
			return nil, context.Cause(ctx)
		}
		return nil, err
	}
	if releaseErr != nil {
		return nil, fmt.Errorf("peer receiver release: %w", releaseErr)
	}
	return raw, nil
}

func readPeerRelayReady(ctx context.Context, raw net.Conn) error {
	setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	stop := context.AfterFunc(setupCtx, func() { raw.Close() })
	defer func() { stop(); cancel() }()
	// Read exactly one bounded line, preserving the following raw TLS stream.
	line := make([]byte, 0, 128)
	var b [1]byte
	for len(line) < 4096 {
		if _, err := io.ReadFull(raw, b[:]); err != nil {
			return fmt.Errorf("peer receiver readiness: %w", err)
		}
		if b[0] == '\n' {
			var ready proto.PeerRelayReady
			if err := json.Unmarshal(line, &ready); err != nil {
				return err
			}
			if ready.Error != "" {
				return errors.New(ready.Error)
			}
			return nil
		}
		line = append(line, b[0])
	}
	return errors.New("peer receiver readiness exceeds frame limit")
}
