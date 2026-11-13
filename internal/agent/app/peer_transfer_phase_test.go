package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPeerTransferPhaseStaysWithOriginatingConnection(t *testing.T) {
	node := &NodeRuntime{}
	var oldFrames, newFrames []proto.PeerTransferPhase
	node.Bind(func(kind string, body any) error {
		if kind != proto.TPeerTransferPhase {
			t.Fatalf("kind=%s", kind)
		}
		oldFrames = append(oldFrames, body.(proto.PeerTransferPhase))
		return nil
	})
	report := node.peerTransferPhaseReporter(true, "attempt", "relay")
	report("connected")
	report("invented")
	node.Unbind()
	node.Bind(func(_ string, body any) error {
		newFrames = append(newFrames, body.(proto.PeerTransferPhase))
		return nil
	})
	report("confirming")
	node.peerTransferPhaseReporter(false, "legacy", "direct")("connected")
	if len(oldFrames) != 1 || len(newFrames) != 0 || oldFrames[0] != (proto.PeerTransferPhase{TransferID: "attempt", Transport: "relay", Phase: "connected"}) {
		t.Fatalf("old=%+v new=%+v", oldFrames, newFrames)
	}
}
