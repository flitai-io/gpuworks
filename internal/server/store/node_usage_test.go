package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// NodeDiskUsage sums, per node, the model bytes in the states that occupy
// disk and the present image bytes - the figures the storage usage page
// computed with two queries per node.
func TestNodeDiskUsageSumsPerNodeInOneRead(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	a, err := s.RegisterNode("usage-a", HashToken("t"), "usage-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RegisterNode("usage-b", HashToken("t"), "usage-b")
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range []struct {
		node  uint
		state string
		bytes int64
	}{
		{a.ID, proto.MFPresent, 100}, {a.ID, proto.MFDownloading, 20}, {a.ID, proto.MFRegistered, 3},
		{a.ID, proto.MFAbsent, 1000}, {a.ID, proto.MFPending, 1000},
		{b.ID, proto.MFSyncing, 7}, {b.ID, proto.MFFailed, 5},
		{0, proto.MFPresent, 9999}, // a shared storage's row belongs to no node
	} {
		model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "org/usage", Revision: string(rune('a' + i)), Name: "usage"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: f.node, State: f.state, Downloaded: f.bytes, Path: "/m"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range []ImageNode{
		{NodeID: a.ID, State: ImageStatePresent, SizeBytes: 40},
		{NodeID: a.ID, State: ImageStatePulling, SizeBytes: 4000},
		{NodeID: b.ID, State: ImageStatePresent, SizeBytes: 60},
	} {
		r.ImageVersionID = uint(i + 1)
		if err := s.db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}

	models, images, err := s.NodeDiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if models[a.ID] != 123 || models[b.ID] != 12 || len(models) != 2 {
		t.Fatalf("model bytes = %v, want node a 123 and node b 12 only", models)
	}
	if images[a.ID] != 40 || images[b.ID] != 60 || len(images) != 2 {
		t.Fatalf("image bytes = %v, want node a 40 and node b 60", images)
	}
}
