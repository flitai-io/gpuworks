package store

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestLatestDrainFailureIgnoresRetainedGenerationsWithBoundedReads(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("source", HashToken("source"), "source")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.CreateInstance(&Instance{UUID: "source-rank", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	history := make([]Instance, 500)
	for i := range history {
		history[i] = Instance{UUID: fmt.Sprintf("old-%d", i), DeploymentID: dep.ID, NodeID: node.ID + 1, State: proto.InstFailed, Message: "old failed rank", NNodes: 2, DistGroup: "collective", DistAttempt: i + 1}
	}
	if err := st.DB().CreateInBatches(history, 20).Error; err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"latest-follower", "latest-leader"} {
		if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID + 1, State: proto.InstStarting, NNodes: 2, DistGroup: "collective", DistAttempt: 501, Headless: uuid == "latest-follower"}); err != nil {
			t.Fatal(err)
		}
	}
	rows := int64(0)
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:bounded-drain-read", func(db *gorm.DB) { rows += db.RowsAffected }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove("test:bounded-drain-read") })
	got, err := st.LatestDrainFailure(dep.ID, node.ID, source.ID)
	if err != nil || got != "" {
		t.Fatalf("historical failure leaked: %q, %v", got, err)
	}
	if rows > 3 {
		t.Fatalf("read %d rows across retained history", rows)
	}
	if err := st.SetInstanceState("latest-follower", proto.InstFailed, "current follower failed"); err != nil {
		t.Fatal(err)
	}
	rows = 0
	got, err = st.LatestDrainFailure(dep.ID, node.ID, source.ID)
	if err != nil || got != "current follower failed" {
		t.Fatalf("current follower hidden: %q, %v", got, err)
	}
	if rows > 3 {
		t.Fatalf("read %d rows across retained history", rows)
	}
}
