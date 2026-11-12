package hub

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func TestPeerRelayIdleTimeoutJoinsProducerAndReleasesAdmission(t *testing.T) {
	driver := &relayGateDriver{Driver: recording.New(filepath.Join(t.TempDir(), "source.json")), started: make(chan struct{}), canceled: make(chan struct{})}
	r := newPeerRelayRig(t, driver)
	policy := prepobs.DefaultPeerRelayPolicy()
	policy.NoProgressTimeout = time.Second
	if err := r.h.SetPeerRelayPolicy(policy); err != nil {
		t.Fatal(err)
	}
	method, params := r.prepare(t, "image", "idle-producer")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner, finish := r.h.beginPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: "idle-producer", Operation: "copy", Kind: "image", SourceNodeUUID: r.source.NodeUUID, TargetNodeUUID: r.target.NodeUUID})
	defer finish("failed", "no_progress_timeout")
	relay := r.h.archiveRelayFor(r.source.NodeUUID, r.target.NodeUUID, "image")
	_, err := relay(owner, method, "idle-producer", params)
	if err == nil || !strings.Contains(err.Error(), "no_progress_timeout") {
		t.Fatalf("idle cause lost: %v", err)
	}
	select {
	case <-driver.canceled:
	case <-ctx.Done():
		t.Fatal("idle timeout left producer running")
	}
	view := r.h.PeerTransferSnapshotFor("idle-producer")
	if view.ActiveCount != 0 || view.WaitingCount != 0 || len(view.Active) != 1 || view.Active[0].Reason != "no_progress_timeout" {
		t.Fatalf("idle cleanup=%+v", view)
	}
	replacement, err := r.h.peerManager().acquire(ctx, "replacement", r.source.NodeUUID, r.target.NodeUUID, 0)
	if err != nil {
		t.Fatalf("idle lease leaked per-node admission: %v", err)
	}
	replacement.release()
}

// slowLoadDriver consumes the archive and then holds the load, the way a
// large docker load or containerd import finishes well after the last byte.
type slowLoadDriver struct {
	*recording.Driver
	hold time.Duration
}

func (d slowLoadDriver) LoadImageRef(ctx context.Context, image string, r io.Reader) (string, error) {
	ref, err := d.Driver.LoadImageRef(ctx, image, r)
	if err != nil {
		return ref, err
	}
	select {
	case <-time.After(d.hold):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return ref, nil
}

func (d slowLoadDriver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	_, err := d.LoadImageRef(ctx, image, r)
	return err
}

// Once the source has sent the whole archive, a receiver still finishing it
// is bounded by the confirmation deadline, not the no-progress deadline. The
// source half-closes its relay stream so the server sees that EOF before the
// acknowledgement; without the half-close this relay was reclaimed as stalled.
func TestPeerRelaySlowReceiverIsBoundByConfirmationDeadline(t *testing.T) {
	for _, mode := range []string{"agent_dial", "server_dial"} {
		t.Run(mode, func(t *testing.T) {
			r := newPeerRelayRigWithTarget(t, newTestHub(t), nil, func(d *recording.Driver) runtime.Driver {
				return slowLoadDriver{Driver: d, hold: 3 * time.Second}
			}, mode)
			policy := prepobs.DefaultPeerRelayPolicy()
			policy.NoProgressTimeout = time.Second
			policy.ConfirmationTimeout = time.Minute
			if err := r.h.SetPeerRelayPolicy(policy); err != nil {
				t.Fatal(err)
			}
			method, params := r.prepare(t, "image", "slow-receiver")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			owner, finish := r.h.beginPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: "slow-receiver", Operation: "copy", Kind: "image", SourceNodeUUID: r.source.NodeUUID, TargetNodeUUID: r.target.NodeUUID})
			defer finish("peer_confirmed", "")
			relay := r.h.archiveRelayFor(r.source.NodeUUID, r.target.NodeUUID, "image")
			if _, err := relay(owner, method, "slow-receiver", params); err != nil {
				t.Fatalf("a receiver finishing within the confirmation deadline failed: %v", err)
			}
		})
	}
}
