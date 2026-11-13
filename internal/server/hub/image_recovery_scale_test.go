package hub

import (
	"fmt"
	"log/slog"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImageReconnectDoesNotScanUnrelatedCatalog(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("reconnect", store.HashToken("token"), "reconnect")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := st.UpsertImage(&store.Image{ImageRef: fmt.Sprintf("unrelated:%d", i), Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	queries := 0
	if err := st.DB().Callback().Query().After("gorm:query").Register("audit_reconnect_queries", func(tx *gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Query().Remove("audit_reconnect_queries")
	New(st, slog.Default()).resumeDirectImageTransfers(node.UUID)
	if queries > 3 {
		t.Fatalf("reconnect with no transfer issued %d queries; unrelated catalog must not amplify a node connection", queries)
	}
}
