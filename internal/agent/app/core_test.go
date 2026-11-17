package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const reconnectObservationContainerID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type reconnectObservationDriver struct {
	imagePullTestDriver
	mu          sync.Mutex
	inspectCall int
	running     bool
	started     chan struct{}
	release     chan struct{}
}

func (d *reconnectObservationDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	d.running = true
	d.mu.Unlock()
	return reconnectObservationContainerID, nil
}

func (d *reconnectObservationDriver) Status(context.Context, string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return runtime.Status{Exists: d.running, Running: d.running}, nil
}

func (d *reconnectObservationDriver) InspectWorkload(context.Context, string) (runtime.WorkloadInspection, error) {
	d.mu.Lock()
	d.inspectCall++
	call := d.inspectCall
	d.mu.Unlock()
	if call == 1 {
		close(d.started)
		<-d.release
	}
	path := "fresh-process"
	if call == 1 {
		path = "stale-process"
	}
	return runtime.WorkloadInspection{
		ContainerID: reconnectObservationContainerID,
		ImageID:     "sha256:test",
		Path:        path,
		Args:        []string{"serve", "--token", "connection-secret"},
	}, nil
}

func TestNodeRuntimeStateAndEmissionLifecycle(t *testing.T) {
	n := newNodeRuntime(slog.Default(), &imagePullTestDriver{}, t.TempDir(), "", metricsOnlyProber{}, nil)
	if n.RuntimeDriverName() != "image-pull-test" {
		t.Fatalf("runtime driver = %q", n.RuntimeDriverName())
	}
	if got := n.resolveModelPath("weights"); got != filepath.Join(n.modelsDir, "weights") {
		t.Fatalf("relative model path = %q", got)
	}
	absolute := filepath.Join(t.TempDir(), "absolute")
	if got := n.resolveModelPath(absolute); got != absolute {
		t.Fatalf("absolute model path = %q", got)
	}
	n.setNetwork(proto.NetworkConfig{ServingIP: "10.40.0.9", HTTPProxy: "http://proxy"})
	if got := n.network(); got.ServingIP != "10.40.0.9" || got.HTTPProxy != "http://proxy" {
		t.Fatalf("network config = %+v", got)
	}

	n.imageRefs = []string{"repo/image:tag"}
	images, paths := n.StatusSnapshot(context.Background(), nil)
	if len(images) != 1 || images[0].ImageRef != "repo/image:tag" || images[0].Present {
		t.Fatalf("image snapshot = %+v", images)
	}
	if len(paths) != 3 || paths[0].Kind != "models" {
		t.Fatalf("path snapshot = %+v", paths)
	}

	var kinds []string
	n.Bind(func(kind string, _ any) error { kinds = append(kinds, kind); return nil })
	n.reportInstance(proto.InstanceStatus{UUID: "i1"})
	n.reportModelFile(proto.ModelFileStatus{Key: "m1"})
	n.reportImageNode(proto.ImageNodeStatus{ImageRef: "repo/image:tag"})
	if len(kinds) != 3 || kinds[0] != proto.TStatusInstance || kinds[2] != proto.TStatusImageNode {
		t.Fatalf("emitted kinds = %v", kinds)
	}
	n.Unbind()
	n.reportInstance(proto.InstanceStatus{UUID: "offline"})
	n.reportInstance(proto.InstanceStatus{UUID: "old-observation", RuntimeObservation: &proto.InstanceRuntimeObservation{}})
	if len(kinds) != 3 {
		t.Fatalf("offline emit unexpectedly sent: %v", kinds)
	}

	// Benchmark status is queued while offline and flushed on the next bind.
	n.reportBenchmark(proto.BenchmarkStatus{ID: 7, State: "done"})
	flushed := make(chan proto.BenchmarkStatus, 1)
	var reboundKinds []string
	n.Bind(func(kind string, body any) error {
		reboundKinds = append(reboundKinds, kind)
		if kind == proto.TStatusBenchmark {
			flushed <- body.(proto.BenchmarkStatus)
		}
		return nil
	})
	select {
	case got := <-flushed:
		if got.ID != 7 || got.State != "done" {
			t.Fatalf("flushed benchmark = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("queued benchmark was not flushed")
	}
	if len(reboundKinds) != 1 || reboundKinds[0] != proto.TStatusBenchmark {
		t.Fatalf("old runtime observation crossed into the rebound session: %v", reboundKinds)
	}
}

func TestRuntimeObservationStaysOnCapturedConnectionAndReconnectQueuesFreshInspect(t *testing.T) {
	driver := &reconnectObservationDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	n := newNodeRuntime(slog.Default(), driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	oldObservations := make(chan proto.InstanceStatus, 1)
	newObservations := make(chan proto.InstanceStatus, 1)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TStatusInstance {
			st := body.(proto.InstanceStatus)
			if st.RuntimeObservation != nil {
				oldObservations <- st
			}
		}
		return nil
	})
	spec := proto.InstanceDesiredState{UUID: "reconnect-observation", Image: "example.test/model:tag", DesiredState: "running", Headless: true, StartTimeout: 1}
	n.OnStateSync(context.Background(), proto.DesiredState{Instances: []proto.InstanceDesiredState{spec}})
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("old connection inspection did not start")
	}

	n.Unbind()
	n.Bind(func(kind string, body any) error {
		if kind == proto.TStatusInstance {
			st := body.(proto.InstanceStatus)
			if st.RuntimeObservation != nil {
				newObservations <- st
			}
		}
		return nil
	})
	// This sync arrives while the old reconcile is in flight. It must queue a
	// fresh adoption/inspect instead of being discarded by the in-flight guard.
	n.OnStateSync(context.Background(), proto.DesiredState{Instances: []proto.InstanceDesiredState{spec}})
	close(driver.release)

	select {
	case stale := <-oldObservations:
		t.Fatalf("old connection unexpectedly received observation: %+v", stale)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case got := <-newObservations:
		if got.RuntimeObservation.Container.ContainerID != reconnectObservationContainerID {
			t.Fatalf("new connection observation=%+v", got.RuntimeObservation)
		}
		if got.RuntimeObservation.Container.Args[2] != "<redacted>" {
			t.Fatalf("new connection args=%q", got.RuntimeObservation.Container.Args)
		}
		if got.RuntimeObservation.Container.Path != "fresh-process" {
			t.Fatalf("stale inspection crossed into new connection: %+v", got.RuntimeObservation.Container)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fresh observation was not sent on the new connection")
	}
	driver.mu.Lock()
	inspectCalls := driver.inspectCall
	driver.mu.Unlock()
	if inspectCalls != 2 {
		t.Fatalf("inspect calls=%d, want old and fresh connection attempts", inspectCalls)
	}
}

func TestRuntimeObservationBlockedSendDoesNotBlockRebindOrUseNewSender(t *testing.T) {
	n := &NodeRuntime{log: slog.Default()}
	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	n.Bind(func(string, any) error {
		close(oldEntered)
		<-releaseOld
		return nil
	})
	oldEpoch := n.captureObservationConnection()
	oldDone := make(chan bool, 1)
	go func() {
		oldDone <- n.reportRuntimeObservation(oldEpoch, proto.InstanceStatus{UUID: "old", RuntimeObservation: &proto.InstanceRuntimeObservation{}})
	}()
	<-oldEntered

	newSent := make(chan proto.InstanceStatus, 1)
	rebound := make(chan struct{})
	go func() {
		n.Bind(func(_ string, body any) error {
			newSent <- body.(proto.InstanceStatus)
			return nil
		})
		close(rebound)
	}()
	select {
	case <-rebound:
	case <-time.After(time.Second):
		t.Fatal("blocked old sender held sendMu across network I/O")
	}
	close(releaseOld)
	if !<-oldDone {
		t.Fatal("accepted old sender unexpectedly failed")
	}
	select {
	case got := <-newSent:
		t.Fatalf("old observation was redirected to new sender: %+v", got)
	default:
	}

	newEpoch := n.captureObservationConnection()
	if !n.reportRuntimeObservation(newEpoch, proto.InstanceStatus{UUID: "new", RuntimeObservation: &proto.InstanceRuntimeObservation{}}) {
		t.Fatal("current connection rejected its observation")
	}
	select {
	case got := <-newSent:
		if got.UUID != "new" {
			t.Fatalf("new sender got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("new sender did not receive current observation")
	}
}

func TestNodeRuntimeStateSyncAndRPCValidation(t *testing.T) {
	n := newNodeRuntime(slog.Default(), nil, t.TempDir(), "none", metricsOnlyProber{}, nil)
	root := filepath.Join(t.TempDir(), "models")
	n.OnStateSync(context.Background(), proto.DesiredState{ModelsRoot: root, ImageRefs: []string{"a:b"}, Network: proto.NetworkConfig{HTTPProxy: "http://proxy:3128"}})
	// The network config belongs to the node, not to the engine: a node that has
	// not installed a container runtime yet still needs it for downloads and
	// model sync.
	if n.ModelsRoot() != root || n.network().HTTPProxy != "http://proxy:3128" {
		t.Fatalf("state sync root/network = %q/%+v", n.ModelsRoot(), n.network())
	}
	images, _ := n.StatusSnapshot(context.Background(), nil)
	if len(images) != 0 { // no runtime, so image presence cannot be checked
		t.Fatalf("images with no runtime = %+v", images)
	}
	if !n.OnStateUpdate(context.Background(), proto.StateUpdate{ModelsRoot: root + "-next", ImageRefs: []string{"c:d"}}) {
		t.Fatal("changed image refs did not request a refresh")
	}
	if n.OnStateUpdate(context.Background(), proto.StateUpdate{ImageRefs: []string{"c:d"}}) {
		t.Fatal("unchanged image refs requested another refresh")
	}
	if n.OnStateUpdate(context.Background(), proto.StateUpdate{ImageRefs: []string{"c:d", "c:d"}}) {
		t.Fatal("equivalent image ref set requested another refresh")
	}
	if n.ModelsRoot() != root+"-next" {
		t.Fatalf("state update root = %q", n.ModelsRoot())
	}
	if _, err := n.OnRPC(context.Background(), "unknown.method", nil); !errors.Is(err, proto.ErrRPCUnknownMethod) || err.Error() != "unknown rpc method: unknown.method" {
		t.Fatalf("unknown RPC error = %v", err)
	}
	for _, raw := range []json.RawMessage{nil, []byte(`{}`), []byte(`{"url":""}`)} {
		if _, err := n.OnRPC(context.Background(), proto.RPCBenchProbe, raw); err == nil {
			t.Fatalf("invalid bench probe %s unexpectedly succeeded", raw)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"tiny"}]}`)
	}))
	defer server.Close()
	got, err := n.OnRPC(context.Background(), proto.RPCBenchProbe, json.RawMessage(`{"url":"`+server.URL+`","api_key":"key"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustJSONBytes(t, got)), "tiny") {
		t.Fatalf("bench probe response = %v", got)
	}

	file := filepath.Join(root, "config.json")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	listed, err := n.OnRPC(context.Background(), proto.RPCFSList, json.RawMessage(`{"path":"`+root+`"}`))
	if err != nil || !strings.Contains(string(mustJSONBytes(t, listed)), "config.json") {
		t.Fatalf("fs.list = %v err=%v", listed, err)
	}
	read, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":"`+file+`","max_bytes":3}`))
	if err != nil || !strings.Contains(string(mustJSONBytes(t, read)), `"truncated":true`) {
		t.Fatalf("fs.read_file = %v err=%v", read, err)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":""}`)); err == nil {
		t.Fatal("fs.read_file accepted empty path")
	}
	// An absent file is an error unless the caller says the file is optional,
	// and then it is a plain answer the caller can tell from "could not look".
	absent := filepath.Join(root, "absent.json")
	if _, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":"`+absent+`"}`)); err == nil {
		t.Fatal("fs.read_file answered an absent file without missing_ok")
	}
	missing, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":"`+absent+`","missing_ok":true}`))
	if err != nil || string(mustJSONBytes(t, missing)) != `{"missing":true,"path":"`+absent+`"}` {
		t.Fatalf("fs.read_file missing_ok = %s err=%v", mustJSONBytes(t, missing), err)
	}
	present, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":"`+file+`","missing_ok":true}`))
	if err != nil || !strings.Contains(string(mustJSONBytes(t, present)), `"content":"abcdef"`) || strings.Contains(string(mustJSONBytes(t, present)), "missing") {
		t.Fatalf("fs.read_file missing_ok on a present file = %s err=%v", mustJSONBytes(t, present), err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, json.RawMessage(`{"path":"`+filepath.Join(root, "dir.json")+`","missing_ok":true}`)); err == nil {
		t.Fatal("missing_ok turned a directory into an absent file")
	}
}

func TestNodeRuntimeStatusStreamsAndPartitionFacts(t *testing.T) {
	prober := &gpu.FixedProber{VendorName: "nvidia"}
	n := newNodeRuntime(slog.Default(), &imagePullTestDriver{}, t.TempDir(), "", prober, nil)
	// A synthetic prober describes cards that do not exist, so asking the
	// host's libcuda about them would test the host rather than the fixture.
	n.startCardSmoke(context.Background())
	if n.cardSmoke != nil {
		t.Fatal("a synthetic NVIDIA prober started the host runtime check")
	}
	if got := n.PartitionAssets(context.Background()); got == nil {
		t.Fatal("nvidia partition assets returned nil")
	}
	_ = n.PartitionProbes(context.Background(), nil, nil)
	var out strings.Builder
	if err := n.StreamLogs(context.Background(), "instance-1", false, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if out.String() != "" { // the fixture driver returns an empty log stream
		t.Fatalf("unexpected logs %q", out.String())
	}
	noRuntime := newNodeRuntime(slog.Default(), nil, t.TempDir(), "none", prober, nil)
	if err := noRuntime.StreamLogs(context.Background(), "instance-1", false, io.Discard); err == nil {
		t.Fatal("StreamLogs without runtime unexpectedly succeeded")
	}
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAgentHelpersAndServerDialCancellation(t *testing.T) {
	if connectMode("server_dial") != "server_dial" || connectMode("anything") != "agent_dial" {
		t.Fatal("connectMode normalization failed")
	}
	b := newBackoff(time.Millisecond, 3*time.Millisecond)
	if b.next() != time.Millisecond || b.next() != 2*time.Millisecond || b.next() != 3*time.Millisecond || b.next() != 3*time.Millisecond {
		t.Fatalf("backoff sequence incorrect: current=%s", b.cur)
	}
	b.reset()
	if b.next() != time.Millisecond {
		t.Fatal("backoff reset did not restore base")
	}

	ctx, cancel := context.WithCancel(context.Background())
	a := newTestAgent(t, Config{ServerURL: "http://127.0.0.1:1", Token: "node-token", ForceDriver: "none", ConnectMode: "server_dial", ListenAddr: "127.0.0.1:0", Prober: metricsOnlyProber{}})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("server_dial cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server_dial agent did not stop after cancellation")
	}
}

func TestPartitionAssetFingerprintIsOrderIndependent(t *testing.T) {
	a := []proto.AssetReport{{Backend: "b", Root: "/z", Files: []string{"2", "1"}}, {Backend: "a", Root: "/a", Files: []string{"x"}}}
	b := []proto.AssetReport{{Backend: "a", Root: "/a", Files: []string{"x"}}, {Backend: "b", Root: "/z", Files: []string{"1", "2"}}}
	if partitionAssetFingerprint(a) != partitionAssetFingerprint(b) {
		t.Fatal("asset fingerprint changed with report/file ordering")
	}
}

func TestNodeRuntimeListsContainersThroughTheSelectedDriver(t *testing.T) {
	driver := &imagePullTestDriver{containers: []string{"gpuworks-11111111-222", "gpuworks-stale"}}
	n := newNodeRuntime(slog.Default(), driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	names, err := n.listContainers(context.Background())
	if err != nil || !slices.Equal(names, driver.containers) {
		t.Fatalf("listContainers = %v, err=%v; want the driver's list", names, err)
	}

	// A node without a usable runtime must say so instead of reporting an empty
	// container list: "no containers" is what an orphan sweep reads as success.
	none := newNodeRuntime(slog.Default(), nil, t.TempDir(), "none", metricsOnlyProber{}, nil)
	if _, err := none.listContainers(context.Background()); err == nil {
		t.Fatal("a node with no container runtime reported a successful empty listing")
	}

	// The same failure reaches the state-sync path, which is the only place
	// orphan cleanup runs: it must be logged rather than dropped, otherwise a
	// node reclaims nothing and looks healthy. The sweep is opt-in, so the
	// sync has to enable it first.
	var logged bytes.Buffer
	failing := &imagePullTestDriver{listErr: errors.New("udocker runtime is unavailable")}
	sync := newNodeRuntime(slog.New(slog.NewTextHandler(&logged, nil)), failing, t.TempDir(), "", metricsOnlyProber{}, nil)
	sync.OnStateSync(context.Background(), proto.DesiredState{OrphanCleanup: true})
	if !strings.Contains(logged.String(), "orphan container cleanup failed") {
		t.Fatalf("state sync swallowed the cleanup failure:\n%s", logged.String())
	}
}
