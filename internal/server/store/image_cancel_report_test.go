package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func cancelReportStore(t *testing.T) (*Store, uint, uint) {
	t.Helper()
	s, err := Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	image, err := s.UpsertImage(&Image{ImageRef: "quay.io/team/runner:cancel-report", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.RegisterNode("cancel-report", HashToken("cancel-report"), "cancel-report")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImageNode(&ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "peer", SourceNodeID: 7, TransferID: "attempt-1", RateLimitKiB: 1}); err != nil {
		t.Fatal(err)
	}
	return s, image.ID, node.ID
}

const killedLoad = "load image archive: docker load: signal: killed"

// The stop kills the receiver's docker load, which reports that failure after
// the row is already cancelled. The operator's reason is the outcome.
func TestLateReceiverFailureDoesNotReplaceAPeerCancel(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if changed, err := s.CancelImageNodeTransferIfActive(imageID, nodeID, "attempt-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	for _, late := range []proto.ImageNodeStatus{
		{State: ImageStateFailed, TransferID: "attempt-1", Message: killedLoad},
		{State: ImageStateSyncing, TransferID: "attempt-1", TransferredBytes: 4096},
		{State: ImageStateFailed, Message: killedLoad},
	} {
		if err := s.ApplyImageNodeStatus(imageID, nodeID, late); err != nil {
			t.Fatal(err)
		}
		row, err := s.ImageNode(imageID, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != ImageStateFailed || row.Message != "the transfer was cancelled" || row.TransferErrorCode != "PEER_TRANSFER_CANCELED" || row.TransferID != "" || row.LastTransferID != "attempt-1" {
			t.Fatalf("after late %+v: row = state %q message %q code %q transfer %q last %q", late, row.State, row.Message, row.TransferErrorCode, row.TransferID, row.LastTransferID)
		}
	}
}

// The load can finish in the instant before the stop lands. The image is on
// the node then, and the row must say so rather than keep a cancel nobody
// can act on.
func TestPeerCancelRacingCompletionKeepsTheArrivedImage(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if _, err := s.CancelImageNodeTransferIfActive(imageID, nodeID, "attempt-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImageNodeStatus(imageID, nodeID, proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "attempt-1", ImageID: "sha256:arrived", Message: "image archive loaded"}); err != nil {
		t.Fatal(err)
	}
	row, err := s.ImageNode(imageID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStatePresent || row.TransferErrorCode != "" || row.ImageDigestID != "sha256:arrived" {
		t.Fatalf("row = state %q code %q digest %q", row.State, row.TransferErrorCode, row.ImageDigestID)
	}
}

// The guard belongs to operator cancels only. When the server fails a relay
// it cannot see why the target's load failed, and the node's own report is
// the better reason.
func TestReceiverFailureStillRefinesAServerSideRelayFailure(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if err := s.FailImageSyncIfActive(imageID, nodeID, "attempt-1", "image archive transferred, but target docker load did not confirm completion"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImageNodeStatus(imageID, nodeID, proto.ImageNodeStatus{State: ImageStateFailed, TransferID: "attempt-1", Message: "load image archive: no space left on device"}); err != nil {
		t.Fatal(err)
	}
	row, err := s.ImageNode(imageID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateFailed || row.Message != "load image archive: no space left on device" {
		t.Fatalf("row = state %q message %q", row.State, row.Message)
	}
}

// A retry is a new attempt: it starts without the previous cancel's code, so
// its own failure is reported.
func TestRetryAfterPeerCancelReportsItsOwnFailure(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if _, err := s.CancelImageNodeTransferIfActive(imageID, nodeID, "attempt-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImageNode(&ImageNode{ImageVersionID: imageID, NodeID: nodeID, State: ImageStateSyncing, Source: "peer", SourceNodeID: 7, TransferID: "attempt-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImageNodeStatus(imageID, nodeID, proto.ImageNodeStatus{State: ImageStateFailed, TransferID: "attempt-2", Message: "load image archive: unexpected EOF"}); err != nil {
		t.Fatal(err)
	}
	row, err := s.ImageNode(imageID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateFailed || row.Message != "load image archive: unexpected EOF" || row.TransferErrorCode != "" {
		t.Fatalf("row = state %q message %q code %q", row.State, row.Message, row.TransferErrorCode)
	}
}

// An inventory that finds the image after a cancel (it arrived another way)
// clears the cancel with the rest of the failed attempt's evidence.
func TestInventoryPresenceAfterPeerCancelClearsTheCancelCode(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if _, err := s.CancelImageNodeTransferIfActive(imageID, nodeID, "attempt-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil {
		t.Fatal(err)
	}
	if err := reconcileImagePresence(s.db, nodeID, []proto.ImagePresence{{ImageRef: "quay.io/team/runner:cancel-report", Present: true, ImageID: "sha256:other-way"}}); err != nil {
		t.Fatal(err)
	}
	row, err := s.ImageNode(imageID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStatePresent || row.TransferErrorCode != "" || row.Message != "" {
		t.Fatalf("row = state %q code %q message %q", row.State, row.TransferErrorCode, row.Message)
	}
}

// Removing the image from the node after a cancel ends the cancel too: the
// row reports the removal, not "Peer transfer canceled", and the cancel guard
// no longer holds the node's reports back.
func TestRemovingTheImageAfterAPeerCancelClearsTheCancelCode(t *testing.T) {
	s, imageID, nodeID := cancelReportStore(t)
	if _, err := s.CancelImageNodeTransferIfActive(imageID, nodeID, "attempt-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkImageNodeAbsent(imageID, nodeID, "image deleted from this node"); err != nil {
		t.Fatal(err)
	}
	row, err := s.ImageNode(imageID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateAbsent || row.TransferErrorCode != "" || row.Message != "image deleted from this node" {
		t.Fatalf("row = state %q code %q message %q", row.State, row.TransferErrorCode, row.Message)
	}
}
