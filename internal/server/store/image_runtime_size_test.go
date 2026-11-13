package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestImageRuntimeSizeLifecycle(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	image, err := st.UpsertImage(&Image{ImageRef: "example/rolling:latest", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("runtime-size", HashToken("size"), "runtime-size")
	if err != nil {
		t.Fatal(err)
	}
	write := func(raw string) {
		t.Helper()
		var report proto.ImagePresence
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(report)
		var roundtrip proto.ImagePresence
		if err := json.Unmarshal(encoded, &roundtrip); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Images: []proto.ImagePresence{roundtrip}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(state string, size *int64) {
		t.Helper()
		row, err := st.ImageNode(image.ID, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != state || row.SizeBytes != 128 || (row.RuntimeSizeBytes == nil) != (size == nil) || size != nil && *row.RuntimeSizeBytes != *size {
			t.Fatalf("row=%+v want state=%s size=%v", row, state, size)
		}
	}
	known := int64(8192)
	zero := int64(0)
	measure := func() {
		write(`{"image_ref":"example/rolling:latest","present":true,"size_bytes":128,"runtime_size_bytes":8192}`)
		check(ImageStatePresent, &known)
	}
	measure()
	write(`{"image_ref":"example/rolling:latest","present":true,"runtime_size_bytes":0}`)
	check(ImageStatePresent, &zero)
	measure()
	write(`{"image_ref":"example/rolling:latest","present":true}`)
	check(ImageStatePresent, nil)
	measure()
	write(`{"image_ref":"example/rolling:latest","present":false}`)
	check(ImageStateAbsent, nil)
	measure()
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, SizeBytes: 128, TransferID: "refresh"}); err != nil {
		t.Fatal(err)
	}
	check(ImageStateSyncing, nil)
	write(`{"image_ref":"example/rolling:latest","present":true,"runtime_size_bytes":8192}`)
	check(ImageStateSyncing, nil)
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "refresh", SizeBytes: 128, ImageID: "sha256:new"}); err != nil {
		t.Fatal(err)
	}
	check(ImageStatePresent, nil)
	measure()
	// A completed refresh with an unknown size invalidates even a measurement
	// arriving between the start and terminal frame; archive bytes stay intact.
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, SizeBytes: 128, ImageID: "sha256:newer"}); err != nil {
		t.Fatal(err)
	}
	check(ImageStatePresent, nil)
	measure()
	write(`{"image_ref":"example/rolling:latest","present":true,"runtime_size_bytes":-1}`)
	check(ImageStatePresent, nil)
}
