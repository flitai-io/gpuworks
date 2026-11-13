package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

// This uses real agents, two independent yamux lanes, pinned TLS and tar
// receivers. The rate is deliberately small enough for several seconds of
// concurrent pressure while control and an ordinary tunnel keep making progress.
func TestPeerRelayRealTLSLoadPreservesControlAndTunnelLatency(t *testing.T) {
	h := newTestHub(t)
	policy := prepobs.DefaultPeerRelayPolicy()
	policy.RateLimitBytesPerSecond = 8 << 20
	if err := h.SetPeerRelayPolicy(policy); err != nil {
		t.Fatal(err)
	}
	rigs := []*peerRelayRig{newPeerRelayRigOnHub(t, h, nil, "agent_dial"), newPeerRelayRigOnHub(t, h, nil, "server_dial")}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 2)
	start := time.Now()
	for i, r := range rigs {
		id := fmt.Sprintf("load-%d", i)
		method, params := r.prepare(t, "model", id)
		if err := os.WriteFile(filepath.Join(r.sourceRoot, "source", "model.safetensors"), make([]byte, 16<<20), 0644); err != nil {
			t.Fatal(err)
		}
		owner, finish := h.beginPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: id, Kind: "model", Operation: "copy", SourceNodeUUID: r.source.NodeUUID, TargetNodeUUID: r.target.NodeUUID})
		relay := h.archiveRelayFor(r.source.NodeUUID, r.target.NodeUUID, "model")
		go func() {
			raw, err := relay(owner, method, id, params)
			if err == nil {
				var result proto.ModelSyncTransferResult
				err = json.Unmarshal(raw, &result)
				if err == nil && (result.Bytes < 16<<20 || len(result.SHA256) != 64) {
					err = fmt.Errorf("unconfirmed archive: %s", raw)
				}
			}
			if err == nil {
				finish("peer_confirmed", "")
			} else {
				finish("failed", "transfer_failed")
			}
			done <- err
		}()
	}
	waitFor(t, 5*time.Second, func() bool {
		s := h.PeerTransferSnapshot()
		return s.ActiveCount == 2 && s.Totals.SourceToTargetBytes > 1<<20
	}, "two real TLS relays under byte pressure")
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { echo.Close(); <-echoDone })
	var maxControl, maxTunnel time.Duration
	for i := 0; i < 6; i++ {
		r := rigs[i%len(rigs)]
		tick := time.Now()
		rpcCtx, rpcCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := r.source.call(rpcCtx, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: "unrelated-latency"})
		rpcCancel()
		if err != nil {
			t.Fatalf("control blocked during relay: %v", err)
		}
		maxControl = max(maxControl, time.Since(tick))
		tick = time.Now()
		tunnelCtx, tunnelCancel := context.WithTimeout(ctx, 2*time.Second)
		tunnel, err := h.OpenTunnel(tunnelCtx, r.target.NodeUUID, echo.Addr().String())
		if err != nil {
			tunnelCancel()
			t.Fatal(err)
		}
		tunnel.SetDeadline(time.Now().Add(2 * time.Second))
		payload := []byte("latency")
		_, err = tunnel.Write(payload)
		if err == nil {
			_, err = io.ReadFull(tunnel, make([]byte, len(payload)))
		}
		tunnel.Close()
		tunnelCancel()
		if err != nil {
			t.Fatalf("tunnel starved during relay: %v", err)
		}
		maxTunnel = max(maxTunnel, time.Since(tick))
		time.Sleep(200 * time.Millisecond)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	snapshot := h.PeerTransferSnapshot()
	bound := policy.BurstBytes + int64(elapsed)*policy.RateLimitBytesPerSecond/int64(time.Second)
	if snapshot.Totals.SourceToTargetBytes+snapshot.Totals.TargetToSourceBytes > bound {
		t.Fatalf("real ciphertext exceeded aggregate envelope: bytes=%d bound=%d", snapshot.Totals.SourceToTargetBytes+snapshot.Totals.TargetToSourceBytes, bound)
	}
	if elapsed < 3*time.Second {
		t.Fatalf("load did not exercise sustained throttling: %s", elapsed)
	}
	if maxControl > time.Second || maxTunnel > time.Second {
		t.Fatalf("latency under 8MiB/s load: control=%s tunnel=%s", maxControl, maxTunnel)
	}
	if snapshot.ActiveCount != 0 || snapshot.WaitingCount != 0 || snapshot.Totals.PeerConfirmed != 2 {
		t.Fatalf("load leaked or falsely confirmed: %+v", snapshot)
	}
	for _, r := range rigs {
		if r.source.CtrlDataPlaneStreamCount() != 0 || r.target.CtrlDataPlaneStreamCount() != 0 {
			t.Fatal("bulk bytes used control lane")
		}
	}
	t.Logf("ciphertext forward=%d reverse=%d aggregate_limit=%d burst=%d elapsed=%s max_control=%s max_tunnel=%s", snapshot.Totals.SourceToTargetBytes, snapshot.Totals.TargetToSourceBytes, policy.RateLimitBytesPerSecond, policy.BurstBytes, elapsed, maxControl, maxTunnel)
}
