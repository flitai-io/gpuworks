package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A status report that leaves a settled row as it is does not rewrite it; a
// change of state or size does, and a transfer in flight is still touched so
// the stall watch sees it alive. Several images reconcile in one report, and
// an unknown ref is skipped.
func TestImagePresenceWritesOnlyWhatChanged(t *testing.T) {
	st, err := Open("file:store_image_presence_changed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settled, _ := st.UpsertImage(&Image{ImageRef: "example/settled:1", Engine: "vllm", Enabled: true})
	syncing, _ := st.UpsertImage(&Image{ImageRef: "example/syncing:1", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-presence-changed", HashToken("t"), "presence-changed")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: syncing.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "live"}); err != nil {
		t.Fatal(err)
	}
	ok := true
	report := func(images ...proto.ImagePresence) {
		t.Helper()
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: images}); err != nil {
			t.Fatal(err)
		}
	}
	row := func(image *Image) *ImageNode {
		t.Helper()
		got, err := st.ImageNode(image.ID, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	present := proto.ImagePresence{ImageRef: settled.ImageRef, Present: true, ImageID: "sha256:settled", SizeBytes: 100}
	report(present, proto.ImagePresence{ImageRef: syncing.ImageRef, Present: true, ImageID: "sha256:while-syncing"}, proto.ImagePresence{ImageRef: "example/unknown:1", Present: true})
	first, firstSync := row(settled), row(syncing)
	if first.State != ImageStatePresent || first.ImageDigestID != "sha256:settled" || first.SizeBytes != 100 || firstSync.State != ImageStateSyncing || firstSync.ImageDigestID != "" {
		t.Fatalf("after first report: settled=%+v syncing=%+v", first, firstSync)
	}

	time.Sleep(20 * time.Millisecond)
	report(present, proto.ImagePresence{ImageRef: syncing.ImageRef})
	if again := row(settled); !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("an unchanged report rewrote the settled row: %v -> %v", first.UpdatedAt, again.UpdatedAt)
	}
	if again := row(syncing); !again.UpdatedAt.After(firstSync.UpdatedAt) || again.State != ImageStateSyncing {
		t.Fatalf("a transfer in flight was not touched: %+v (before %v)", again, firstSync.UpdatedAt)
	}

	report(proto.ImagePresence{ImageRef: settled.ImageRef, Present: true, SizeBytes: 200})
	if got := row(settled); got.SizeBytes != 200 || got.ImageDigestID != "sha256:settled" {
		t.Fatalf("a report without an id rewrote the digest: %+v", got)
	}
	report(proto.ImagePresence{ImageRef: settled.ImageRef, Present: true, ImageID: "sha256:rolled", SizeBytes: 200})
	if got := row(settled); got.ImageDigestID != "sha256:rolled" {
		t.Fatalf("identity change not written: %+v", got)
	}
	report(proto.ImagePresence{ImageRef: settled.ImageRef, Present: false})
	if got := row(settled); got.State != ImageStateAbsent || got.ImageDigestID != "" {
		t.Fatalf("absence did not clear state and identity: %+v", got)
	}
}

// A failed copy whose image then arrives another way (the replica pulled it
// itself) becomes present without the failed attempt's source and reason; a
// failed row the runtime still lacks keeps them, so the failure stays visible.
func TestImagePresenceClearsAFailedTransferOnceTheImageArrives(t *testing.T) {
	st, err := Open("file:store_image_presence_failed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	image, _ := st.UpsertImage(&Image{ImageRef: "example/copied:1", Engine: "vllm", Enabled: true})
	source, _ := st.RegisterNode("node-presence-source", HashToken("s"), "presence-source")
	node, _ := st.RegisterNode("node-presence-failed", HashToken("t"), "presence-failed")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateFailed, Source: "peer", SourceNodeID: source.ID,
		TransferredBytes: 4096, Message: "send peer image archive: connection refused", ImageDigestID: "sha256:failed-copy"}); err != nil {
		t.Fatal(err)
	}
	ok := true
	report := func(present bool) *ImageNode {
		t.Helper()
		presence := proto.ImagePresence{ImageRef: image.ImageRef, Present: present, SizeBytes: 300}
		if present {
			presence.ImageID = "sha256:arrived"
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{presence}}); err != nil {
			t.Fatal(err)
		}
		got, err := st.ImageNode(image.ID, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := report(false); got.State != ImageStateFailed || got.Source != "peer" || got.Message == "" || got.ImageDigestID != "" {
		t.Fatalf("a failure the runtime confirms stays visible: %+v", got)
	}
	got := report(true)
	if got.State != ImageStatePresent || got.Source != "" || got.SourceNodeID != 0 || got.Message != "" || got.TransferredBytes != 0 || got.SizeBytes != 300 || got.ImageDigestID != "sha256:arrived" {
		t.Fatalf("present after a failed copy: %+v", got)
	}
}
