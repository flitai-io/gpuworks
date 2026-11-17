package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	oldContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newContainerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldImageID     = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	newImageID     = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

type delayedObservationDriver struct {
	headlessTestDriver
	mu        sync.Mutex
	started   chan struct{}
	release   chan struct{}
	inspect   int
	runs      int
	running   bool
	imageRefs []string
}

func (d *delayedObservationDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs++
	d.running = true
	if d.runs == 1 {
		return oldContainerID, nil
	}
	return newContainerID, nil
}

func (d *delayedObservationDriver) Status(context.Context, string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return runtime.Status{Exists: d.running, Running: d.running}, nil
}

func (d *delayedObservationDriver) Stop(context.Context, string) error { return nil }
func (d *delayedObservationDriver) Remove(context.Context, string) error {
	d.mu.Lock()
	d.running = false
	d.mu.Unlock()
	return nil
}

func (d *delayedObservationDriver) InspectWorkload(context.Context, string) (runtime.WorkloadInspection, error) {
	d.mu.Lock()
	d.inspect++
	call := d.inspect
	d.mu.Unlock()
	if call == 1 {
		close(d.started)
		<-d.release
	}
	id, image := newContainerID, newImageID
	if call == 1 {
		id, image = oldContainerID, oldImageID
	}
	return runtime.WorkloadInspection{
		ContainerID: id, ImageID: image, Path: "python",
		Args: []string{"/bin/sh", "-c", "cat <<'EOF'\n{\"api_token\":\"argv-wire-secret\"}\nEOF"},
		Env:  map[string]string{"HF_TOKEN": "env-wire-secret", "CONFIG": `{"api_token":"json-wire-secret"}`, "LOG_LEVEL": "info"},
	}, nil
}

func (d *delayedObservationDriver) InspectImage(_ context.Context, ref string) (runtime.ImageInspection, error) {
	d.mu.Lock()
	d.imageRefs = append(d.imageRefs, ref)
	d.mu.Unlock()
	return runtime.ImageInspection{ID: ref, Env: map[string]string{"REGISTRY_TOKEN": "secret"}}, nil
}

func TestDesiredRemovalDropsDelayedInspectBeforeSameUUIDRecreate(t *testing.T) {
	driver := &delayedObservationDriver{
		headlessTestDriver: headlessTestDriver{name: "docker"},
		started:            make(chan struct{}), release: make(chan struct{}),
	}
	reported := make(chan proto.InstanceStatus, 64)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, nil, t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.headlessStableChecks = 1
	spec := proto.InstanceDesiredState{UUID: "same-instance", DesiredState: "running", Headless: true, StartTimeout: 1}

	oldCtx, cancelOld := context.WithCancel(context.Background())
	e.SetDesiredFull(oldCtx, []proto.InstanceDesiredState{spec})
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("old runtime inspect did not start")
	}
	e.SetDesiredFull(oldCtx, nil)
	cancelOld()
	close(driver.release)
	waitEngineIdle(t, e)

	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	var observed []*proto.InstanceRuntimeObservation
	deadline := time.After(2 * time.Second)
	for len(observed) == 0 {
		select {
		case st := <-reported:
			if st.RuntimeObservation != nil {
				observed = append(observed, st.RuntimeObservation)
			}
		case <-deadline:
			t.Fatal("replacement observation was not reported")
		}
	}
	time.Sleep(20 * time.Millisecond)
	for {
		select {
		case st := <-reported:
			if st.RuntimeObservation != nil {
				observed = append(observed, st.RuntimeObservation)
			}
		default:
			goto drained
		}
	}

drained:
	if len(observed) != 1 || observed[0].Container.ContainerID != newContainerID {
		t.Fatalf("published observations=%+v", observed)
	}
	if got := observed[0].Container.Args; len(got) != 3 || got[2] != "<redacted>" {
		t.Fatalf("redacted args=%q", got)
	}
	if observed[0].Container.Env["HF_TOKEN"] != "<redacted>" || observed[0].Container.Env["CONFIG"] != "<redacted>" || observed[0].Image.Env["REGISTRY_TOKEN"] != "<redacted>" {
		t.Fatalf("secrets survived observation: %+v", observed[0])
	}
	wire, err := json.Marshal(observed[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"argv-wire-secret", "env-wire-secret", "json-wire-secret"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("secret %q survived observation wire: %s", secret, wire)
		}
	}
	driver.mu.Lock()
	refs := append([]string(nil), driver.imageRefs...)
	driver.mu.Unlock()
	if len(refs) != 2 || refs[0] != oldImageID || refs[1] != newImageID {
		t.Fatalf("image inspection refs=%q", refs)
	}
}

func TestDesiredRemovalDropsPendingReconnectReconcile(t *testing.T) {
	driver := &delayedObservationDriver{
		headlessTestDriver: headlessTestDriver{name: "docker"},
		started:            make(chan struct{}), release: make(chan struct{}),
	}
	e := newHermeticEngine(driver, func(proto.InstanceStatus) {}, nil, t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.headlessStableChecks = 1
	spec := proto.InstanceDesiredState{UUID: "removed-pending", DesiredState: "running", Headless: true, StartTimeout: 1}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	<-driver.started
	// A reconnect-style full sync queues another ensure behind the blocked
	// inspect. Removing the instance must clear that request before handoff.
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	e.SetDesiredFull(context.Background(), nil)
	close(driver.release)
	waitEngineIdle(t, e)
	time.Sleep(20 * time.Millisecond)
	driver.mu.Lock()
	runs, inspections, running := driver.runs, driver.inspect, driver.running
	driver.mu.Unlock()
	if runs != 1 || inspections != 1 || running {
		t.Fatalf("removed pending reconcile resurrected workload: runs=%d inspections=%d running=%v", runs, inspections, running)
	}
}

func TestPendingReconcileHandsOffToNewestStoppedSpec(t *testing.T) {
	driver := &delayedObservationDriver{
		headlessTestDriver: headlessTestDriver{name: "docker"},
		started:            make(chan struct{}), release: make(chan struct{}),
	}
	e := newHermeticEngine(driver, func(proto.InstanceStatus) {}, nil, t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.headlessStableChecks = 1
	running := proto.InstanceDesiredState{UUID: "newest-pending", DesiredState: "running", Headless: true, StartTimeout: 1}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{running})
	<-driver.started
	// First queue a reconnect reconciliation, then replace it with a newer
	// stopped desired spec while the original inspection is still blocked.
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{running})
	stopped := running
	stopped.DesiredState = "stopped"
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{stopped})
	close(driver.release)
	waitEngineIdle(t, e)
	driver.mu.Lock()
	runs, inspections, stillRunning := driver.runs, driver.inspect, driver.running
	driver.mu.Unlock()
	if runs != 1 || inspections != 1 || stillRunning {
		t.Fatalf("pending handoff ignored newest stopped spec: runs=%d inspections=%d running=%v", runs, inspections, stillRunning)
	}
}

func waitEngineIdle(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		busy := len(e.inFlight) > 0
		e.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("engine did not leave in-flight state")
}

func TestRuntimeIdentityComparisonIsStrict(t *testing.T) {
	full := strings.Repeat("a", 64)
	if !sameContainerIdentity(full[:12], full) || !sameContainerIdentity(full, full) {
		t.Fatal("valid full/short container identity did not match")
	}
	if sameContainerIdentity("sha256:new", "sha256:new-other") || sameContainerIdentity("container", "container-other") {
		t.Fatal("non-container prefix was accepted")
	}
	if sameImageIdentity("sha256:new", "sha256:new-other") || !sameImageIdentity(oldImageID, oldImageID) {
		t.Fatal("image identity comparison was not exact")
	}
}

func TestObservationReasonMapsCancellationAndTimeoutWithoutRawError(t *testing.T) {
	if got := observationReason(context.Canceled); got != "timeout" {
		t.Fatalf("canceled reason=%q", got)
	}
	if got := observationReason(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline reason=%q", got)
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"driver permission refusal", fmt.Errorf("docker container inspect: %w: secret detail", runtime.ErrPermissionDenied), "permission_denied"},
		{"os permission error", &fs.PathError{Op: "open", Path: "/run/containerd/containerd.sock", Err: syscall.EACCES}, "permission_denied"},
		{"container answered absent", fmt.Errorf("nerdctl container inspect: %w: abc", runtime.ErrContainerNotFound), "not_running"},
		{"image answered absent", fmt.Errorf("docker image inspect: %w: org/m:v1", runtime.ErrImageNotFound), "not_running"},
		// The repository's own words are no longer read: text alone classifies as nothing.
		{"permission text only", errors.New("permission denied: secret detail"), "inspect_failed"},
		{"not found text only", errors.New("exec: \"nerdctl\": executable file not found in $PATH"), "inspect_failed"},
	}
	for _, tc := range cases {
		if got := observationReason(tc.err); got != tc.want {
			t.Errorf("%s: reason=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCanceledContextPreventsRuntimeObservationPublication(t *testing.T) {
	driver := &delayedObservationDriver{
		headlessTestDriver: headlessTestDriver{name: "docker"},
		started:            make(chan struct{}), release: make(chan struct{}),
	}
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, nil, t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "canceled-observation", DesiredState: "running"}
	e.mu.Lock()
	e.desired[spec.UUID] = spec
	e.runtimeEpoch[spec.UUID] = 1
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- e.collectAndPublishRuntimeObservation(ctx, spec, oldContainerID, oldContainerID, 1) }()
	<-driver.started
	cancel()
	close(driver.release)
	if <-done {
		t.Fatal("canceled inspection reported an observation")
	}
	select {
	case st := <-reported:
		t.Fatalf("canceled observation was published: %+v", st)
	default:
	}
}

func TestUnsupportedRuntimeReportsExplicitUnknownLayers(t *testing.T) {
	e := newHermeticEngine(&headlessTestDriver{name: "udocker"}, func(proto.InstanceStatus) {}, nil, t.TempDir())
	got := e.collectRuntimeObservation(context.Background(), e.Driver(), "name", "")
	if got.Container.Status != proto.ObservationUnknown || got.Container.Reason != "unsupported" {
		t.Fatalf("container=%+v", got.Container)
	}
	if got.Image.Status != proto.ObservationUnknown || got.Process.Status != proto.ObservationUnknown || got.Process.Reason != "not_collected" {
		t.Fatalf("observation=%+v", got)
	}
}
