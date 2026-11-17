package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

type fakeSources struct {
	mu        sync.Mutex
	nodeTar   map[string][]byte // uuid -> tar; nil entry = an agent that closes at once
	block     chan struct{}
	logFile   string
	dirs      []ServerDir
	requested [][]string
}

func (f *fakeSources) OpenNodeFiles(_ context.Context, uuid string, sessions []string) (io.ReadCloser, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.requested = append(f.requested, append([]string{uuid}, sessions...))
	f.mu.Unlock()
	raw, ok := f.nodeTar[uuid]
	if !ok {
		return nil, errors.New("node not connected")
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (f *fakeSources) InstanceLogs(_ context.Context, uuid string, from, to time.Time, w io.Writer, max int64) (bool, error) {
	_, err := io.WriteString(w, "engine started "+uuid+"\n")
	return false, err
}

func (f *fakeSources) InstanceNodes(uuids []string) ([]string, error) { return []string{"n-run"}, nil }
func (f *fakeSources) DeploymentInstances(ids []uint) ([]string, error) {
	return []string{"inst-1"}, nil
}

func (f *fakeSources) Snapshot(_ context.Context, scope SnapshotScope) (map[string]any, error) {
	return map[string]any{
		"nodes":    []map[string]any{{"UUID": "n-run", "Token": "nodetoken-0123456789", "Hostname": "gpu-01"}},
		"settings": map[string]string{"registry_auth": `{"user":"u","password":"hunter2-long-secret"}`, "orphan_cleanup": "true"},
	}, nil
}

func (f *fakeSources) ServerLogFiles() []string { return []string{f.logFile} }
func (f *fakeSources) ServerDirs() []ServerDir  { return f.dirs }
func (f *fakeSources) Version() string          { return "v-test" }

func tarOf(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name[strings.IndexByte(h.Name, '/')+1:]] = string(b)
	}
}

func waitBundle(t *testing.T, m *Manager, id string) Bundle {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, ok := m.Bundle(id); ok && b.State != "running" {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatal("collection did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBundleCollectsTheWindowRedactedAndSaysWhatItSkipped(t *testing.T) {
	m, nodes, _ := newManager(t)
	nodes.online["n-old"] = true
	nodes.online["n-pick"] = false
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	in := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	before := now.Add(-3 * time.Hour).Format(time.RFC3339Nano)
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "gpuworks-server.log")
	_ = os.WriteFile(logFile, []byte("time="+before+" level=INFO msg=old\ntime="+in+" level=WARN msg=\"in window\"\n\tstack continuation\n"), 0o600)
	dataDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dataDir, "gpuworks.db-wal"), make([]byte, 4096), 0o600)
	src := &fakeSources{logFile: logFile, dirs: []ServerDir{{Kind: "data", Path: dataDir}}, nodeTar: map[string][]byte{
		"n-run": tarOf(t, map[string]string{
			"agent.log":    "time=" + before + " level=INFO msg=early\ntime=" + in + " level=INFO msg=\"image present\" image=x\n",
			"debug/s1.log": "time=" + in + " level=DEBUG msg=\"runtime command\"\n",
		}),
		"n-old": nil, // an agent that predates the read: the stream closes empty
	}}
	m.SetSources(src)
	// A debug session inside the window.
	sessionDir := filepath.Join(m.DebugDir(), "s1")
	_ = os.MkdirAll(filepath.Join(sessionDir, "requests"), 0o700)
	ended := now.Add(-20 * time.Minute)
	raw, _ := json.Marshal(DebugSession{ID: "s1", StartedAt: now.Add(-25 * time.Minute), Until: now, EndedAt: &ended})
	_ = os.WriteFile(filepath.Join(sessionDir, "session.json"), raw, 0o600)
	_ = os.WriteFile(filepath.Join(sessionDir, "requests", "7-r1.jsonl"), []byte(`{"t":"request"}`+"\n"), 0o600)

	b, err := m.StartBundle(context.Background(), BundleRequest{DeploymentIDs: []uint{7}, NodeUUIDs: []string{"n-run", "n-old", "n-pick"}, RequestedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !b.From.Equal(now.Add(-time.Hour)) || !b.To.Equal(now) || b.State != "running" || len(b.Instances) != 1 {
		t.Fatalf("started = %+v", b)
	}
	done := waitBundle(t, m, b.ID)
	if done.State != "ready" || done.SizeBytes == 0 {
		t.Fatalf("finished = %+v", done)
	}
	items := map[string]BundleItem{}
	for _, it := range done.Items {
		items[it.Path] = it
	}
	if got := items["nodes/n-pick"]; got.Status != proto.CheckSkip || got.Code != messages.CheckDiagnosticsNodeOffline || got.ID() != BundleItemNode {
		t.Fatalf("offline node = %+v", got)
	}
	if got := items["nodes/n-old/agent.log"]; got.Status != proto.CheckSkip || got.Code != messages.CheckDiagnosticsAgentUnsupported || got.ID() != BundleItemNodeAgentLog {
		t.Fatalf("old agent = %+v", got)
	}
	if got := items["nodes/n-run/agent.log"]; got.Status != proto.CheckPass || got.Code != "" || got.Bytes == 0 {
		t.Fatalf("agent log = %+v", got)
	}
	for _, it := range done.Items {
		if !proto.ValidCheckStatus(it.Status) || (it.Status != proto.CheckPass && it.Code == "") {
			t.Errorf("item %s has status %q code %q", it.Path, it.Status, it.Code)
		}
	}

	files := readArchive(t, m.bundleArchive(b.ID))
	if got := files["server/gpuworks-server.log"]; strings.Contains(got, "msg=old") || !strings.Contains(got, `msg="in window"`) || !strings.Contains(got, "stack continuation") {
		t.Fatalf("server log = %q", got)
	}
	if got := files["nodes/n-run/agent.log"]; strings.Contains(got, "early") || !strings.Contains(got, "image present") {
		t.Fatalf("agent log = %q", got)
	}
	if !strings.Contains(files["nodes/n-run/debug/s1.log"], "runtime command") || files["debug/s1/requests/7-r1.jsonl"] == "" || files["debug/s1/session.json"] == "" {
		t.Fatalf("debug session files missing: %v", keys(files))
	}
	var disk ServerDisk
	if err := json.Unmarshal([]byte(files["server/disk.json"]), &disk); err != nil || len(disk.Dirs) != 1 || disk.Dirs[0].Path != dataDir ||
		len(disk.Dirs[0].Entries) != 1 || disk.Dirs[0].Entries[0] != (ServerDirEntry{Name: "gpuworks.db-wal", Bytes: 4096}) {
		t.Fatalf("server/disk.json = %s", files["server/disk.json"])
	}
	if !strings.Contains(files["instances/inst-1.log"], "engine started inst-1") {
		t.Fatalf("instance log = %q", files["instances/inst-1.log"])
	}
	if src.requested[0][1] != "s1" {
		t.Fatalf("nodes are asked for the window's sessions: %v", src.requested)
	}
	dbNodes, settings := files["db/nodes.json"], files["db/settings.json"]
	if strings.Contains(dbNodes, "0123456789") || !strings.Contains(dbNodes, `"nodetoke***"`) || !strings.Contains(dbNodes, "gpu-01") {
		t.Fatalf("nodes.json = %s", dbNodes)
	}
	if strings.Contains(settings, "hunter2-long-secret") || !strings.Contains(settings, `hunter2-***`) || !strings.Contains(settings, `"orphan_cleanup": "true"`) {
		t.Fatalf("settings.json = %s", settings)
	}
	var manifest Bundle
	if json.Unmarshal([]byte(files["manifest.json"]), &manifest) != nil || manifest.Version != "v-test" || manifest.RequestedBy != "admin" || len(manifest.Items) == 0 {
		t.Fatalf("manifest = %s", files["manifest.json"])
	}
	if list := m.Bundles(); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("list = %+v", list)
	}
	if f, _, err := m.OpenBundle(b.ID); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if _, _, err := m.OpenBundle("../etc"); err == nil {
		t.Fatal("opened an id outside the store")
	}
}

func TestBundleRefusesOutOfRangeAndRunsOneAtATime(t *testing.T) {
	m, _, _ := newManager(t)
	src := &fakeSources{block: make(chan struct{}), nodeTar: map[string][]byte{"n-run": tarOf(t, map[string]string{"agent.log": ""})}}
	m.SetSources(src)
	now := time.Now()
	for _, req := range []BundleRequest{
		{From: now, To: now.Add(-time.Minute)},
		{From: now.Add(-8 * 24 * time.Hour), To: now},
	} {
		if _, err := m.StartBundle(context.Background(), req); productCode(err) != "DIAGNOSTICS_BUNDLE_INVALID" {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if _, err := m.StartBundle(context.Background(), BundleRequest{NodeUUIDs: []string{"ghost"}}); productCode(err) != "DIAGNOSTICS_SCOPE_UNKNOWN" {
		t.Fatal(err)
	}
	b, err := m.StartBundle(context.Background(), BundleRequest{NodeUUIDs: []string{"n-run"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartBundle(context.Background(), BundleRequest{}); productCode(err) != "DIAGNOSTICS_BUNDLE_RUNNING" {
		t.Fatalf("second = %v", err)
	}
	close(src.block)
	if done := waitBundle(t, m, b.ID); done.State != "ready" {
		t.Fatalf("done = %+v", done)
	}
	again, err := m.StartBundle(context.Background(), BundleRequest{})
	if err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
	waitBundle(t, m, again.ID)
}

func TestFilterLogWindowStopsAtItsBound(t *testing.T) {
	in := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var src strings.Builder
	for i := 0; i < 100; i++ {
		src.WriteString("time=" + in.Format(time.RFC3339Nano) + " level=INFO msg=line\n")
	}
	var out bytes.Buffer
	n, truncated, err := filterLogWindow(strings.NewReader(src.String()), &out, in.Add(-time.Minute), in.Add(time.Minute), 500)
	if err != nil || !truncated || n > 500 || int64(out.Len()) != n {
		t.Fatalf("n=%d truncated=%v err=%v", n, truncated, err)
	}
}

func TestRedactKeepsAPrefixOfCredentialsOnly(t *testing.T) {
	got, err := Redact(map[string]any{"APIKey": "gw-abcdef1234567890", "Name": "prod", "nested": map[string]any{"password": "short"}, "PromptTokens": 12})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `{"APIKey":"gw-abcde***","Name":"prod","PromptTokens":12,"nested":{"password":"***"}}` {
		t.Fatalf("redacted = %s", raw)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
