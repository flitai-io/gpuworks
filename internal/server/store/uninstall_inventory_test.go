package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

// The inventory runs hand-written SQL over a snapshot, so only the schema the
// migrations actually produce can tell whether its column names are right; a
// hand-made CREATE TABLE fixture repeats whatever names the query assumed.
func migratedInventoryStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func inventoryByID(items []uninstallcheck.Item) map[string]uninstallcheck.Item {
	out := make(map[string]uninstallcheck.Item, len(items))
	for _, item := range items {
		out[item.ID] = item
	}
	return out
}

func TestUninstallInventoryOfFreshMigratedDatabasePasses(t *testing.T) {
	_, path := migratedInventoryStore(t)
	items := inventoryByID(ReadUninstallInventory(context.Background(), path))
	for _, id := range []string{"server.nodes", "server.deployments", "server.instances", "server.provider_instances", "server.cloud_leases"} {
		item, ok := items[id]
		if !ok {
			t.Fatalf("missing %s in %+v", id, items)
		}
		if item.Status != uninstallcheck.Pass {
			t.Errorf("%s = %s (%s); a freshly migrated, empty database must not block uninstall", id, item.Status, item.Summary)
		}
	}
}

func TestUninstallInventoryReportsMigratedInstanceRows(t *testing.T) {
	st, path := migratedInventoryStore(t)
	for _, in := range []Instance{
		{UUID: "instance-running", DeploymentID: 8, NodeID: 7, State: proto.InstRunning, ContainerID: "gpuworks-running", APIPort: 8000, GPUIndexes: "[0,1]"},
		{UUID: "instance-stopped", DeploymentID: 8, NodeID: 7, State: proto.InstStopped, GPUIndexes: "[]"},
	} {
		if err := st.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	item := inventoryByID(ReadUninstallInventory(context.Background(), path))["server.instances"]
	if item.Status != uninstallcheck.Blocked {
		t.Fatalf("server.instances = %s (%s), want blocked by the running instance", item.Status, item.Summary)
	}
	if len(item.Details) != 2 {
		t.Fatalf("details = %q, want both instances", item.Details)
	}
	if !strings.Contains(item.Details[0], `uuid="instance-running"`) || !strings.Contains(item.Details[0], `gpu_indexes="[0,1]"`) || !strings.Contains(item.Details[0], `api_port="8000"`) {
		t.Fatalf("running instance evidence = %q", item.Details[0])
	}
}
