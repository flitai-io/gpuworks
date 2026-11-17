package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A manifest written before items were checks still lists, each old state
// read as the check it was.
func TestBundleReadsAManifestWrittenWithItemStates(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dir: dir}
	m.SetBundleDir(dir)
	old := `{"id":"20261101-120000-abcd1234","requested_by":"admin","created_at":"2026-11-01T12:00:00Z","from":"2026-11-01T11:00:00Z",` +
		`"to":"2026-11-01T12:00:00Z","deployment_ids":[],"instance_uuids":[],"node_uuids":["n1","n2","n3"],"doctor":true,"server_version":"v1",` +
		`"state":"ready","size_bytes":10,"items":[` +
		`{"name":"server/gpuworks-server.log","state":"collected","bytes":100},` +
		`{"name":"nodes/n1/agent.log","state":"collected","bytes":24,"truncated":true},` +
		`{"name":"nodes/n2","state":"skipped","reason":"offline","bytes":0},` +
		`{"name":"nodes/n3/agent.log","state":"skipped","reason":"unsupported","bytes":0},` +
		`{"name":"db","state":"failed","reason":"database is locked","bytes":0}]}`
	if err := os.WriteFile(filepath.Join(dir, "20261101-120000-abcd1234.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	list := m.Bundles()
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	want := []BundleItem{
		{Path: "server/gpuworks-server.log", Status: proto.CheckPass, Bytes: 100},
		{Path: "nodes/n1/agent.log", Status: proto.CheckWarn, Code: messages.CheckDiagnosticsItemTruncated, Bytes: 24},
		{Path: "nodes/n2", Status: proto.CheckSkip, Code: messages.CheckDiagnosticsNodeOffline},
		{Path: "nodes/n3/agent.log", Status: proto.CheckSkip, Code: messages.CheckDiagnosticsAgentUnsupported},
		{Path: "db", Status: proto.CheckFail, Code: messages.CheckDiagnosticsItemReadFailed, Evidence: "database is locked"},
	}
	got := list[0].Items
	if len(got) != len(want) {
		t.Fatalf("items = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[0].ID() != BundleItemServerLog || got[2].ID() != BundleItemNode || got[4].ID() != BundleItemRecords {
		t.Fatalf("ids = %s %s %s", got[0].ID(), got[2].ID(), got[4].ID())
	}
}

// The manifest is written in the check shape, with no item state words, and
// reads back as written.
func TestBundleItemManifestShapeRoundTrips(t *testing.T) {
	items := []BundleItem{
		collectedItem("instances/i1.log", 5, false),
		collectedItem("nodes/n1/agent.log", 9, true),
		skippedItem("nodes/n2", messages.CheckDiagnosticsNodeOffline),
		failedItem("nodes/n3", messages.CheckDiagnosticsItemReadFailed, os.ErrDeadlineExceeded, 0),
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"path":"instances/i1.log","status":"pass","bytes":5},` +
		`{"path":"nodes/n1/agent.log","status":"warn","code":"DIAGNOSTICS_ITEM_TRUNCATED","bytes":9},` +
		`{"path":"nodes/n2","status":"skip","code":"DIAGNOSTICS_NODE_OFFLINE","bytes":0},` +
		`{"path":"nodes/n3","status":"fail","code":"DIAGNOSTICS_ITEM_READ_FAILED","evidence":"i/o timeout","bytes":0}]`
	if string(raw) != want {
		t.Fatalf("manifest items =\n%s\nwant\n%s", raw, want)
	}
	var back []BundleItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if back[i] != items[i] {
			t.Fatalf("round trip %d = %+v, want %+v", i, back[i], items[i])
		}
	}
	for _, it := range items[1:] {
		if it.Message() == "" {
			t.Errorf("%s has no English fallback", it.Code)
		}
	}
	if node := items[2].Node(); node != "n2" || items[0].Node() != "" {
		t.Fatalf("node = %q", node)
	}
}

func TestBundleItemIDs(t *testing.T) {
	for path, want := range map[string]string{
		"server/disk.json": BundleItemServerDisk, "server/gpuworks-server.log": BundleItemServerLog,
		"debug/s1/session.json": BundleItemDebugFile, "nodes/n1": BundleItemNode, "nodes/n1/agent.log": BundleItemNodeAgentLog,
		"nodes/n1/doctor.json": BundleItemNodeDoctor, "nodes/n1/debug/s1.log": BundleItemNodeDebugFile,
		"instances/i1.log": BundleItemInstanceLog, "db": BundleItemRecords, "db/nodes.json": BundleItemRecord,
	} {
		if got := (BundleItem{Path: path}).ID(); got != want {
			t.Errorf("ID(%s) = %s, want %s", path, got, want)
		}
	}
}
