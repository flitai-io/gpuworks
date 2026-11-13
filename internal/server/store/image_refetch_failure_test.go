package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// refetchFailedRow is a node that holds sha256:old and whose forced re-fetch
// from a URL failed, written the way the hub and the agent write it.
func refetchFailedRow(t *testing.T) (*Store, *Image, *Node) {
	t.Helper()
	st, err := Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, _ := st.UpsertImage(&Image{ImageRef: "example/refetch:1", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-refetch", HashToken("t"), "refetch")
	ok := true
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{{ImageRef: image.ImageRef, Present: true, ImageID: "sha256:old", SizeBytes: 300}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "url", TransferID: "refetch-1", SizeBytes: 20, Message: "downloading image archive"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{ImageRef: image.ImageRef, State: ImageStateFailed, TransferID: "refetch-1", Message: "load image archive: unexpected EOF"}); err != nil {
		t.Fatal(err)
	}
	return st, image, node
}

func inventory(t *testing.T, st *Store, image *Image, node *Node, presence proto.ImagePresence) *ImageNode {
	t.Helper()
	ok := true
	presence.ImageRef = image.ImageRef
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{presence}}); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// A forced re-fetch that fails leaves the old image where it was. The node is
// still usable (present), and the failure stays readable until the next try
// instead of vanishing at the next inventory a few seconds later. It is marked
// with a code, because a message alone could as well be a success's; the
// failed attempt's source is not where the held image came from and goes.
func TestFailedForcedRefetchStaysVisibleOverTheImageItMeantToReplace(t *testing.T) {
	st, image, node := refetchFailedRow(t)
	for i := 0; i < 2; i++ {
		row := inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:old", SizeBytes: 300})
		if row.State != ImageStatePresent || row.ImageDigestID != "sha256:old" {
			t.Fatalf("inventory %d: state %q digest %q; the image never left", i, row.State, row.ImageDigestID)
		}
		if row.Message != "load image archive: unexpected EOF" || row.TransferErrorCode != "IMAGE_REFETCH_FAILED" || row.LastTransferID != "refetch-1" {
			t.Fatalf("inventory %d: message %q code %q last %q; the failed re-fetch must stay visible", i, row.Message, row.TransferErrorCode, row.LastTransferID)
		}
		if row.Source != "" || row.SourceNodeID != 0 {
			t.Fatalf("inventory %d: source %q node %d; the failed attempt did not bring the held image", i, row.Source, row.SourceNodeID)
		}
	}
	// The next attempt is what replaces the record.
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "url", TransferID: "refetch-2", Message: "downloading image archive"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{ImageRef: image.ImageRef, State: ImageStatePresent, TransferID: "refetch-2", ImageID: "sha256:new", Message: "image archive loaded"}); err != nil {
		t.Fatal(err)
	}
	if row := inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:new"}); row.State != ImageStatePresent || row.Message != "image archive loaded" || row.TransferErrorCode != "" || row.Source != "url" || row.ImageDigestID != "sha256:new" {
		t.Fatalf("after a successful retry: %+v", row)
	}
}

// A report that does not identify the image cannot show that another one
// arrived, so it does not erase the failure either.
func TestUnidentifiedPresenceKeepsAFailedRefetchsReason(t *testing.T) {
	st, image, node := refetchFailedRow(t)
	row := inventory(t, st, image, node, proto.ImagePresence{Present: true})
	if row.State != ImageStatePresent || row.Message != "load image archive: unexpected EOF" || row.TransferErrorCode != "IMAGE_REFETCH_FAILED" || row.ImageDigestID != "sha256:old" {
		t.Fatalf("row = state %q message %q code %q digest %q", row.State, row.Message, row.TransferErrorCode, row.ImageDigestID)
	}
}

// A different image means it came another way after the failure (a replica
// pulled it, an operator loaded it): the failed attempt no longer describes
// the row.
func TestADifferentImageAfterAFailedRefetchClearsTheFailure(t *testing.T) {
	st, image, node := refetchFailedRow(t)
	row := inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:other"})
	if row.State != ImageStatePresent || row.Message != "" || row.Source != "" || row.TransferredBytes != 0 || row.ImageDigestID != "sha256:other" {
		t.Fatalf("row = %+v", row)
	}
}

// Once marked, the row is present, so a different image arriving later (a
// replica pulled the tag's new content) must still retire the failure.
func TestADifferentImageAfterAMarkedRefetchFailureClearsIt(t *testing.T) {
	st, image, node := refetchFailedRow(t)
	inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:old"})
	row := inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:other"})
	if row.State != ImageStatePresent || row.Message != "" || row.TransferErrorCode != "" || row.ImageDigestID != "sha256:other" {
		t.Fatalf("row = state %q message %q code %q digest %q", row.State, row.Message, row.TransferErrorCode, row.ImageDigestID)
	}
}

// A cancelled forced peer re-sync keeps its own, more precise code.
func TestACancelledRefetchKeepsItsCancelCode(t *testing.T) {
	st, err := Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, _ := st.UpsertImage(&Image{ImageRef: "example/refetch:cancel", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-refetch-cancel", HashToken("t"), "refetch-cancel")
	present := proto.ImagePresence{Present: true, ImageID: "sha256:old"}
	inventory(t, st, image, node, present)
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "peer", SourceNodeID: 3, TransferID: "resync-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelImageNodeTransferIfActive(image.ID, node.ID, "resync-1", "the transfer was cancelled", "PEER_TRANSFER_CANCELED"); err != nil {
		t.Fatal(err)
	}
	row := inventory(t, st, image, node, present)
	if row.State != ImageStatePresent || row.TransferErrorCode != "PEER_TRANSFER_CANCELED" || row.Message != "the transfer was cancelled" || row.Source != "" {
		t.Fatalf("row = state %q code %q message %q source %q", row.State, row.TransferErrorCode, row.Message, row.Source)
	}
}

// The mark says the node kept its image, so it goes with the image.
func TestAMarkedRefetchFailureGoesWhenTheImageDoes(t *testing.T) {
	st, image, node := refetchFailedRow(t)
	if row := inventory(t, st, image, node, proto.ImagePresence{Present: true, ImageID: "sha256:old"}); row.TransferErrorCode != "IMAGE_REFETCH_FAILED" {
		t.Fatalf("not marked: %+v", row)
	}
	row := inventory(t, st, image, node, proto.ImagePresence{Present: false})
	if row.State != ImageStateAbsent || row.TransferErrorCode != "" || row.Message != "" || row.ImageDigestID != "" {
		t.Fatalf("row = state %q code %q message %q digest %q", row.State, row.TransferErrorCode, row.Message, row.ImageDigestID)
	}
}
