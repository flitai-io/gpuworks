package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

func TestApplyPreStartArgvWrapsEngineCommand(t *testing.T) {
	stateDir := t.TempDir()
	e := &Engine{stateDir: stateDir, log: slog.New(slog.DiscardHandler)}
	ws := runtime.WorkloadSpec{
		Entrypoint: []string{"vllm", "serve"},
		Cmd:        []string{"/model", "--port", "30000"},
	}
	spec := proto.InstanceDesiredState{UUID: "instance/pre-start", ResolvedLaunch: proto.ResolvedLaunch{PreStartArgv: []string{"prepare-tool", "value with spaces", "it's-safe"}}}
	if err := e.applyPreStart(&ws, spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ws.Entrypoint, []string{"/bin/sh", preStartMountPath}) {
		t.Fatalf("entrypoint = %#v", ws.Entrypoint)
	}
	wantCommand := []string{"vllm", "serve", "/model", "--port", "30000"}
	if !slices.Equal(ws.Cmd, wantCommand) {
		t.Fatalf("command = %#v, want %#v", ws.Cmd, wantCommand)
	}
	if len(ws.Mounts) != 1 || ws.Mounts[0].Target != preStartMountPath || !ws.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %#v", ws.Mounts)
	}
	content, err := os.ReadFile(ws.Mounts[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	want := "'prepare-tool' 'value with spaces' 'it'\"'\"'s-safe'\nexec \"$@\""
	if !strings.Contains(string(content), want) {
		t.Fatalf("wrapper = %q, want fragment %q", content, want)
	}
}

func TestApplyPreStartValidatesContract(t *testing.T) {
	tests := []struct {
		name string
		spec proto.InstanceDesiredState
		ws   runtime.WorkloadSpec
		want string
	}{
		{name: "mutually-exclusive", spec: desiredStateWithPreStart([]string{"a"}, "b"), ws: runtime.WorkloadSpec{Entrypoint: []string{"engine"}}, want: "mutually exclusive"},
		{name: "script-limit", spec: desiredStateWithPreStart(nil, strings.Repeat("x", maxPreStartScriptBytes+1)), ws: runtime.WorkloadSpec{Entrypoint: []string{"engine"}}, want: "64 KiB"},
		{name: "explicit-entrypoint", spec: desiredStateWithPreStart([]string{"a"}, ""), want: "explicit engine entrypoint"},
		{name: "empty-argv", spec: desiredStateWithPreStart([]string{"a", " "}, ""), ws: runtime.WorkloadSpec{Entrypoint: []string{"engine"}}, want: "pre_start_argv[1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := &Engine{stateDir: t.TempDir()}
			err := e.applyPreStart(&test.ws, test.spec)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func desiredStateWithPreStart(argv []string, script string) proto.InstanceDesiredState {
	return proto.InstanceDesiredState{UUID: "x", ResolvedLaunch: proto.ResolvedLaunch{PreStartArgv: argv, PreStartScript: script}}
}

type headlessTestDriver struct {
	status runtime.Status
	calls  int
	name   string
}

func TestSwitchDriverReplacesRuntimeAndKeepsDesiredState(t *testing.T) {
	old := &headlessTestDriver{name: "docker"}
	newDriver := &headlessTestDriver{name: "udocker"}
	e := newHermeticEngine(old, nil, slog.Default(), t.TempDir())
	e.SetDesiredFull(context.Background(), nil)
	if err := e.SwitchDriver(context.Background(), newDriver); err != nil {
		t.Fatalf("switch driver: %v", err)
	}
	e.mu.Lock()
	got := e.driver
	e.mu.Unlock()
	if got != newDriver {
		t.Fatalf("active driver = %v, want new driver", got.Name())
	}
	if err := e.SwitchDriver(context.Background(), newDriver); err != nil {
		t.Fatalf("idempotent switch: %v", err)
	}
}

type switchFailureDriver struct {
	*headlessTestDriver
	statusCalls int
	inspectErr  bool
	stopErr     bool
	removeErr   bool
	verifyErr   bool
	stillExists bool
}

func (d *switchFailureDriver) Status(context.Context, string) (runtime.Status, error) {
	d.statusCalls++
	if (d.statusCalls == 1 && d.inspectErr) || (d.statusCalls > 1 && d.verifyErr) {
		return runtime.Status{}, errors.New("inspection failed")
	}
	if d.statusCalls > 1 && !d.stillExists {
		return runtime.Status{}, nil
	}
	return runtime.Status{Exists: true, Running: true}, nil
}

func (d *switchFailureDriver) Stop(context.Context, string) error {
	if d.stopErr {
		return errors.New("stop failed")
	}
	return nil
}

func (d *switchFailureDriver) Remove(context.Context, string) error {
	if d.removeErr {
		return errors.New("remove failed")
	}
	return nil
}

func TestSwitchDriverKeepsOldRuntimeUntilContainersAreConfirmedAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*switchFailureDriver)
		marker bool
	}{
		{name: "inspect failure", setup: func(d *switchFailureDriver) { d.inspectErr = true }},
		{name: "stop failure", setup: func(d *switchFailureDriver) { d.stopErr = true }},
		{name: "remove failure", setup: func(d *switchFailureDriver) { d.removeErr = true }},
		{name: "verification failure", setup: func(d *switchFailureDriver) { d.verifyErr = true }},
		{name: "container still exists", setup: func(d *switchFailureDriver) { d.stillExists = true }},
		{name: "cleanup pending", marker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &switchFailureDriver{headlessTestDriver: &headlessTestDriver{name: "old"}}
			if tc.setup != nil {
				tc.setup(old)
			}
			next := &headlessTestDriver{name: "new"}
			e := newHermeticEngine(old, nil, slog.Default(), t.TempDir())
			spec := proto.InstanceDesiredState{UUID: "switch-owned", DesiredState: "running"}
			e.mu.Lock()
			e.desired[spec.UUID] = spec
			if tc.marker {
				e.cleanupRequired[spec.UUID] = spec
			}
			e.mu.Unlock()
			if err := e.SwitchDriver(context.Background(), next); err == nil {
				t.Fatal("runtime switched despite unconfirmed old container cleanup")
			}
			if got := e.Driver(); got != old {
				t.Fatalf("runtime changed after failed cleanup: %s", got.Name())
			}
		})
	}
}

type blockedPullDriver struct {
	*headlessTestDriver
	pulling chan struct{}
	proceed chan struct{}
	mu      sync.Mutex
	runs    int
}

func (d *blockedPullDriver) ImageExists(context.Context, string) (bool, error) { return false, nil }
func (d *blockedPullDriver) PullImage(context.Context, string, func(string)) error {
	close(d.pulling)
	<-d.proceed
	return nil
}
func (d *blockedPullDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	d.runs++
	d.mu.Unlock()
	return "", errors.New("start was not canceled")
}

func TestRemovalDuringPullWaitsForStartupBeforeAcknowledgingTeardown(t *testing.T) {
	driver := &blockedPullDriver{
		headlessTestDriver: &headlessTestDriver{},
		pulling:            make(chan struct{}),
		proceed:            make(chan struct{}),
	}
	reported := make(chan proto.InstanceStatus, 16)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "removal-during-pull", Image: "example:latest", DesiredState: "running"}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	select {
	case <-driver.pulling:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not reach image pull")
	}
	removeDone := make(chan struct{})
	go func() {
		e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
		close(removeDone)
	}()
	select {
	case <-removeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("remove update did not finish while pull was blocked")
	}
	select {
	case st := <-reported:
		if st.State == proto.InstStopped {
			t.Fatal("teardown acknowledged before the in-flight startup finished")
		}
	case <-time.After(50 * time.Millisecond):
	}
	close(driver.proceed)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstStopped {
				driver.mu.Lock()
				runs := driver.runs
				driver.mu.Unlock()
				if runs != 0 {
					t.Fatalf("removed instance was started after the pull: %d runs", runs)
				}
				return
			}
		case <-deadline:
			t.Fatal("startup completion was never followed by teardown acknowledgement")
		}
	}
}

type healthWaitDriver struct {
	*headlessTestDriver
	checked chan struct{}
	once    sync.Once
}

func (d *healthWaitDriver) Status(context.Context, string) (runtime.Status, error) {
	d.once.Do(func() { close(d.checked) })
	return runtime.Status{Exists: true, Running: true}, nil
}

func TestRemovalCancelsHealthWaitBeforeTeardownAck(t *testing.T) {
	driver := &healthWaitDriver{headlessTestDriver: &headlessTestDriver{}, checked: make(chan struct{})}
	reported := make(chan proto.InstanceStatus, 32)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "removal-during-health", DesiredState: "running", APIPort: 1, StartTimeout: 600}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	select {
	case <-driver.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not reach health inspection")
	}
	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	deadline := time.After(time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstStopped {
				return
			}
		case <-deadline:
			t.Fatal("removal waited for the health retry instead of canceling startup")
		}
	}
}

type blockedGenerationDriver struct {
	*headlessTestDriver
	firstRunEntered chan struct{}
	releaseFirstRun chan struct{}
	secondRun       chan struct{}
	mu              sync.Mutex
	runs            int
	removeAttempts  int
	exists          bool
	startedEarly    bool
}

func (d *blockedGenerationDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	d.runs++
	run := d.runs
	if run > 1 && d.removeAttempts < 2 {
		d.startedEarly = true
	}
	d.exists = true
	d.mu.Unlock()
	if run == 1 {
		close(d.firstRunEntered)
		<-d.releaseFirstRun // Mimic a runtime that cannot abort Run on cancellation.
	} else if run == 2 {
		close(d.secondRun)
	}
	return "old-or-new-container", nil
}

func (d *blockedGenerationDriver) Status(context.Context, string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return runtime.Status{Exists: d.exists, Running: d.exists}, nil
}

func (d *blockedGenerationDriver) Remove(context.Context, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeAttempts++
	if d.removeAttempts == 1 {
		return errors.New("temporary removal failure")
	}
	d.exists = false
	return nil
}

func TestSameUUIDReaddWaitsForSuccessfulOldGenerationCleanup(t *testing.T) {
	driver := &blockedGenerationDriver{
		headlessTestDriver: &headlessTestDriver{},
		firstRunEntered:    make(chan struct{}),
		releaseFirstRun:    make(chan struct{}),
		secondRun:          make(chan struct{}),
	}
	reported := make(chan proto.InstanceStatus, 32)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "same-uuid-generation", Image: "example:latest", DesiredState: "running", Headless: true}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	select {
	case <-driver.firstRunEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old generation did not enter Run")
	}
	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{spec}, nil)
	close(driver.releaseFirstRun)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstRemoving && strings.Contains(st.Message, "temporary removal failure") {
				goto cleanupFailed
			}
		case <-deadline:
			t.Fatal("failed old-generation cleanup was not reported")
		}
	}
cleanupFailed:
	driver.mu.Lock()
	runs, early := driver.runs, driver.startedEarly
	driver.mu.Unlock()
	if runs != 1 || early {
		t.Fatalf("replacement started before cleanup retry: runs=%d early=%v", runs, early)
	}
	// A later full sync retries the failed cleanup and then starts the newest
	// desired generation. The prior failure must not permanently wedge the UUID.
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	select {
	case <-driver.secondRun:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not start after successful cleanup retry")
	}
	driver.mu.Lock()
	runs, early = driver.runs, driver.startedEarly
	driver.mu.Unlock()
	if runs != 2 || early {
		t.Fatalf("replacement ordering: runs=%d early=%v", runs, early)
	}
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstStopped {
				t.Fatal("same-UUID replacement received a stale stopped acknowledgement")
			}
		default:
			return
		}
	}
}

func TestFullSyncRetriesFailedRemovalWithoutDesiredEntry(t *testing.T) {
	driver := &blockedGenerationDriver{
		headlessTestDriver: &headlessTestDriver{},
		firstRunEntered:    make(chan struct{}),
		releaseFirstRun:    make(chan struct{}),
		secondRun:          make(chan struct{}),
	}
	reported := make(chan proto.InstanceStatus, 32)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "retry-removed-generation", Image: "example:latest", DesiredState: "running", Headless: true}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	select {
	case <-driver.firstRunEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old generation did not enter Run")
	}
	e.SetDesiredFull(context.Background(), nil)
	close(driver.releaseFirstRun)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstRemoving && strings.Contains(st.Message, "temporary removal failure") {
				goto retry
			}
		case <-deadline:
			t.Fatal("first cleanup failure was not reported")
		}
	}
retry:
	e.SetDesiredFull(context.Background(), nil)
	deadline = time.After(2 * time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstStopped {
				driver.mu.Lock()
				attempts, runs := driver.removeAttempts, driver.runs
				driver.mu.Unlock()
				if attempts != 2 || runs != 1 {
					t.Fatalf("retry ordering: remove attempts=%d runs=%d", attempts, runs)
				}
				return
			}
		case <-deadline:
			t.Fatal("full sync did not retry failed removal")
		}
	}
}

type transientInspectDriver struct {
	*headlessTestDriver
	mu      sync.Mutex
	checks  int
	removes int
	runs    int
}

func (d *transientInspectDriver) Status(context.Context, string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checks++
	if d.checks == 1 {
		return runtime.Status{}, errors.New("temporary inspect error")
	}
	return runtime.Status{Exists: true, Running: true}, nil
}

func (d *transientInspectDriver) Remove(context.Context, string) error {
	d.mu.Lock()
	d.removes++
	d.mu.Unlock()
	return nil
}

func (d *transientInspectDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	d.runs++
	d.mu.Unlock()
	return "unexpected-run", nil
}

func TestTransientInspectionErrorRetriesWithoutRetiringRunningInstance(t *testing.T) {
	driver := &transientInspectDriver{headlessTestDriver: &headlessTestDriver{}}
	reported := make(chan proto.InstanceStatus, 16)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.headlessStableChecks = 1
	spec := proto.InstanceDesiredState{UUID: "transient-inspect", DesiredState: "running", Headless: true}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstRemoving || st.State == proto.InstStopped {
				t.Fatalf("transient inspection retired the desired instance: %+v", st)
			}
			if st.State == proto.InstRunning {
				driver.mu.Lock()
				checks, removes, runs := driver.checks, driver.removes, driver.runs
				driver.mu.Unlock()
				if checks < 2 || removes != 0 || runs != 0 {
					t.Fatalf("existing container was not adopted safely: checks=%d removes=%d runs=%d", checks, removes, runs)
				}
				return
			}
		case <-deadline:
			t.Fatal("instance was not adopted after inspection recovered")
		}
	}
}

func (d *headlessTestDriver) Name() string {
	if d.name != "" {
		return d.name
	}
	return "headless-test"
}
func (d *headlessTestDriver) Detect(context.Context) (bool, error)                  { return true, nil }
func (d *headlessTestDriver) ImageExists(context.Context, string) (bool, error)     { return true, nil }
func (d *headlessTestDriver) PullImage(context.Context, string, func(string)) error { return nil }
func (d *headlessTestDriver) Prepare(context.Context, runtime.WorkloadSpec) error   { return nil }
func (d *headlessTestDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	return "worker", nil
}
func (d *headlessTestDriver) Stop(context.Context, string) error   { return nil }
func (d *headlessTestDriver) Remove(context.Context, string) error { return nil }
func (d *headlessTestDriver) ListContainers(context.Context) ([]string, error) {
	return nil, nil
}

// RunningContainers names nothing: this driver answers every Status with one
// fixed value and tracks no container names.
func (d *headlessTestDriver) RunningContainers(context.Context) ([]string, error) {
	return nil, nil
}
func (d *headlessTestDriver) Status(context.Context, string) (runtime.Status, error) {
	d.calls++
	return d.status, nil
}
func (d *headlessTestDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (d *headlessTestDriver) Exec(context.Context, string, []string) ([]byte, error) { return nil, nil }

type injectionProber struct {
	inj       gpu.RuntimeInjection
	driverInj map[string]gpu.RuntimeInjection
}

func (p injectionProber) Vendor() string  { return "test" }
func (p injectionProber) Available() bool { return true }
func (p injectionProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	return nil, nil
}
func (p injectionProber) DriverVersion(ctx context.Context) string { return "" }
func (p injectionProber) Topology(ctx context.Context) string      { return "" }
func (p injectionProber) Metrics(ctx context.Context) ([]gpu.Metric, error) {
	return nil, nil
}
func (p injectionProber) RuntimeInjection(indexes []int) gpu.RuntimeInjection {
	return p.inj
}

// driverAwareInjectionProber additionally implements gpu.DriverAwareInjector,
// exercising the optional-interface dispatch in applyDeviceInjection the same
// way AscendProber does for nerdctl.
type driverAwareInjectionProber struct {
	injectionProber
}

func (p driverAwareInjectionProber) RuntimeInjectionForDriver(indexes []int, driverName string) gpu.RuntimeInjection {
	if inj, ok := p.driverInj[driverName]; ok {
		return inj
	}
	return p.inj
}

func TestApplyDeviceInjection(t *testing.T) {
	e := NewWithProber(nil, nil, slog.Default(), t.TempDir(), injectionProber{
		inj: gpu.RuntimeInjection{
			Env:        map[string]string{"ASCEND_RT_VISIBLE_DEVICES": "2,4"},
			DockerArgs: []string{"--runtime", "ascend"},
		},
	})
	ws := runtime.WorkloadSpec{Env: map[string]string{"HF_HUB_OFFLINE": "1"}}

	if err := e.applyDeviceInjection(&ws, []int{2, 4}); err != nil {
		t.Fatal(err)
	}

	if got := ws.Env["ASCEND_RT_VISIBLE_DEVICES"]; got != "2,4" {
		t.Fatalf("ASCEND_RT_VISIBLE_DEVICES = %q", got)
	}
	if got := ws.Env["HF_HUB_OFFLINE"]; got != "1" {
		t.Fatalf("existing env was not preserved: %q", got)
	}
	if len(ws.DockerArgs) != 2 || ws.DockerArgs[0] != "--runtime" || ws.DockerArgs[1] != "ascend" {
		t.Fatalf("DockerArgs = %+v", ws.DockerArgs)
	}
}

func TestDetectOOMUsesLogMarkerWhenRuntimeOnlyReportsExit137(t *testing.T) {
	d := &headlessTestDriver{status: runtime.Status{Exists: true, ExitCode: 137}}
	e := newHermeticEngine(d, nil, slog.Default(), t.TempDir())
	if e.detectOOM(context.Background(), "worker", d.status) {
		t.Fatal("empty logs incorrectly classified exit 137 as OOM")
	}
	// The fake driver contract includes the canonical marker in Logs; use a
	// narrow wrapper so this test exercises the same fallback as L3 containers.
	marker := &oomMarkerDriver{headlessTestDriver: d}
	e.driver = marker
	if !e.detectOOM(context.Background(), "worker", d.status) {
		t.Fatal("OOM log marker was not recognized")
	}
}

type oomMarkerDriver struct{ *headlessTestDriver }

func (d *oomMarkerDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("torch.cuda.OutOfMemoryError: CUDA out of memory\n")), nil
}

func TestApplyDeviceInjectionRejectsUnsupportedRuntime(t *testing.T) {
	e := NewWithProber(nil, nil, slog.Default(), t.TempDir(), injectionProber{
		inj: gpu.RuntimeInjection{UnsupportedReason: "vendor runtime is not configured"},
	})
	ws := runtime.WorkloadSpec{Env: map[string]string{}}
	if err := e.applyDeviceInjection(&ws, []int{0}); err == nil || !strings.Contains(err.Error(), "vendor runtime") {
		t.Fatalf("applyDeviceInjection error = %v", err)
	}
}

func TestApplyDeviceInjectionUsesDriverAwareInjectorAndPropagatesMounts(t *testing.T) {
	prober := driverAwareInjectionProber{injectionProber{
		// The plain (non-driver-aware) fallback would return this if the
		// optional-interface dispatch were broken; assert against it never
		// being used for the nerdctl case below.
		inj: gpu.RuntimeInjection{
			Env:        map[string]string{"ASCEND_VISIBLE_DEVICES": "2,4"},
			DockerArgs: []string{"--runtime", "ascend"},
		},
		driverInj: map[string]gpu.RuntimeInjection{
			"nerdctl": {
				Env:        map[string]string{"ASCEND_RT_VISIBLE_DEVICES": "0,1"},
				DockerArgs: []string{"--device", "/dev/davinci2", "--device", "/dev/davinci4"},
				Mounts: []gpu.RuntimeMount{
					{Source: "/usr/local/dcmi", Target: "/usr/local/dcmi", ReadOnly: true},
				},
			},
		},
	}}
	e := NewWithProber(&headlessTestDriver{name: "nerdctl"}, nil, slog.Default(), t.TempDir(), prober)
	ws := runtime.WorkloadSpec{Env: map[string]string{}}

	if err := e.applyDeviceInjection(&ws, []int{2, 4}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ws.Env["ASCEND_VISIBLE_DEVICES"]; ok {
		t.Fatalf("used the driver-unaware fallback instead of RuntimeInjectionForDriver: %+v", ws.Env)
	}
	if got := ws.Env["ASCEND_RT_VISIBLE_DEVICES"]; got != "0,1" {
		t.Fatalf("ASCEND_RT_VISIBLE_DEVICES = %q", got)
	}
	if len(ws.DockerArgs) != 4 {
		t.Fatalf("DockerArgs = %+v", ws.DockerArgs)
	}
	if len(ws.Mounts) != 1 || ws.Mounts[0].Source != "/usr/local/dcmi" || !ws.Mounts[0].ReadOnly {
		t.Fatalf("driver-injected mounts were not propagated to WorkloadSpec: %+v", ws.Mounts)
	}
}

func TestHeadlessWorkerUsesContainerReadinessWithoutHTTP(t *testing.T) {
	driver := &headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.headlessStableChecks = 2
	spec := proto.InstanceDesiredState{
		UUID: "worker-1", Headless: true, NodeRank: 1, APIPort: 1,
		StartTimeout: 1, DesiredState: "running",
	}

	e.probeAndReport(context.Background(), spec, "worker")

	st := <-reported
	if st.State != proto.InstRunning || st.DirectAddr != "" || len(st.Ports) != 0 || len(st.Measured) != 0 {
		t.Fatalf("headless status = %+v", st)
	}
	if driver.calls < 2 {
		t.Fatalf("status calls = %d, want sustained readiness checks", driver.calls)
	}
}

func TestHeadlessWorkerReportsContainerExit(t *testing.T) {
	driver := &headlessTestDriver{status: runtime.Status{Exists: true, Running: false, ExitCode: 17}}
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.probeAndReport(context.Background(), proto.InstanceDesiredState{
		UUID: "worker-exited", Headless: true, NodeRank: 1, StartTimeout: 1,
	}, "worker")

	st := <-reported
	if st.State != proto.InstFailed || !strings.Contains(st.Message, "17") {
		t.Fatalf("headless exit status = %+v", st)
	}
}

func TestMetricSpecsExcludeHeadlessWorkers(t *testing.T) {
	e := newHermeticEngine(&headlessTestDriver{}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	e.desired["leader"] = proto.InstanceDesiredState{UUID: "leader", DesiredState: "running", APIPort: 30000}
	e.desired["worker"] = proto.InstanceDesiredState{UUID: "worker", DesiredState: "running", Headless: true}

	logs := e.RunningSpecs()
	metrics := e.MetricSpecs()
	if len(logs) != 2 {
		t.Fatalf("running specs = %d, want leader and worker logs", len(logs))
	}
	if len(metrics) != 1 || metrics[0].UUID != "leader" {
		t.Fatalf("metric specs = %+v, want leader only", metrics)
	}
}

type sleepHTTPServer struct {
	t           *testing.T
	server      *httptest.Server
	calls       []string
	bodies      map[string]string // path → last request body
	status      map[string]int
	body        map[string]string
	probeBodies []string
	// drop closes the connection of a request to the path without an answer:
	// a transport failure, after which the order may still have run.
	drop map[string]bool
}

func newSleepHTTPServer(t *testing.T) *sleepHTTPServer {
	t.Helper()
	h := &sleepHTTPServer{t: t, status: map[string]int{}, body: map[string]string{}, bodies: map[string]string{}, drop: map[string]bool{}}
	h.body["/is_sleeping"] = `{"is_sleeping":false}`
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls = append(h.calls, r.Method+" "+r.URL.RequestURI())
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			h.bodies[r.URL.Path] = string(raw)
		}
		if h.drop[r.URL.Path] {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack %s: %v", r.URL.Path, err)
				return
			}
			_ = conn.Close()
			return
		}
		code := h.status[r.URL.Path]
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		body := h.body[r.URL.Path]
		if r.URL.Path == "/is_sleeping" && len(h.probeBodies) > 0 {
			body = h.probeBodies[0]
			h.probeBodies = h.probeBodies[1:]
		}
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *sleepHTTPServer) port(t *testing.T) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(h.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcileSleepTransitionsAndRetainsLevelOnFailure(t *testing.T) {
	h := newSleepHTTPServer(t)
	reported := make(chan proto.InstanceStatus, 16)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "sleep-1", Sleepable: true, APIPort: h.port(t), Ports: []int{h.port(t)}}

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if got := e.sleepLevel[spec.UUID]; got != 1 {
		t.Fatalf("level after L1 = %d, want 1", got)
	}
	if st := <-reported; st.SleepLevel != 1 || st.State != proto.InstRunning || st.Phase != "" {
		t.Fatalf("L1 report = %+v", st)
	}

	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	if got := e.sleepLevel[spec.UUID]; got != 0 {
		t.Fatalf("level after wake = %d, want 0", got)
	}
	if st := <-reported; st.SleepLevel != 0 {
		t.Fatalf("wake report = %+v", st)
	}

	e.mu.Lock()
	e.sleepLevel[spec.UUID] = 1
	e.mu.Unlock()
	spec.DesiredSleep = 2
	spec.SleepMode = "abort"
	e.reconcileSleep(context.Background(), spec)
	if got := e.sleepLevel[spec.UUID]; got != 2 {
		t.Fatalf("level after L1→L2 = %d, want 2", got)
	}
	if st := <-reported; st.SleepLevel != 2 {
		t.Fatalf("L2 report = %+v", st)
	}
	want := []string{"POST /sleep?level=1&mode=wait", "POST /wake_up", "POST /wake_up", "POST /sleep?level=2&mode=abort"}
	if strings.Join(h.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("sleep calls = %+v, want %+v", h.calls, want)
	}

	h.calls = nil
	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 0 {
		t.Fatalf("L2 wake report = %+v", st)
	}
	want = []string{"POST /wake_up?tags=weights", "POST /collective_rpc", "POST /wake_up?tags=kv_cache", "POST /reset_prefix_cache"}
	if strings.Join(h.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("wake calls = %+v, want %+v", h.calls, want)
	}
	if h.bodies["/collective_rpc"] != `{"method":"reload_weights"}` {
		t.Fatalf("collective_rpc body = %q", h.bodies["/collective_rpc"])
	}

	// A failed order leaves the actual level unchanged and reports its target.
	// It is not retried until the server sends another desired-state push.
	h.calls = nil
	h.status["/sleep"] = http.StatusInternalServerError
	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if got := e.sleepLevel[spec.UUID]; got != 0 {
		t.Fatalf("failed reconcile changed level to %d, want 0", got)
	}
	st := <-reported
	if st.State != proto.InstRunning || st.SleepLevel != 0 || st.SleepTarget == nil || *st.SleepTarget != 1 || st.Phase != proto.PhaseSleep || !strings.Contains(st.Message, "http 500") {
		t.Fatalf("failure report = %+v", st)
	}
	if len(h.calls) != 2 || h.calls[1] != "GET /is_sleeping" {
		t.Fatalf("failed order was retried without a new push: calls=%+v", h.calls)
	}

	h.status["/sleep"] = 0
	e.reconcileSleep(context.Background(), spec)
	if got := e.sleepLevel[spec.UUID]; got != 1 || len(h.calls) != 3 {
		t.Fatalf("explicit retry: level=%d calls=%+v", got, h.calls)
	}
	if st := <-reported; st.SleepLevel != 1 || st.Phase != "" {
		t.Fatalf("retry report = %+v", st)
	}
}

func TestReconcileSleepTreatsPartialL2WakeAsUncertain(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/wake_up"] = http.StatusInternalServerError
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.sleepLevel["partial-l2"] = 2
	e.reconcileSleep(context.Background(), proto.InstanceDesiredState{UUID: "partial-l2", Sleepable: true, DesiredSleep: 0, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstFailed || st.Phase != proto.PhaseSleep || !strings.Contains(st.Message, "uncertain") {
		t.Fatalf("partial L2 report = %+v", st)
	}
	if e.sleepLevel["partial-l2"] != 2 {
		t.Fatalf("partial L2 level changed to %d", e.sleepLevel["partial-l2"])
	}
}

func TestReconcileSleepRestoresConfirmedL1AfterFailedWake(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/wake_up"] = http.StatusInternalServerError
	h.probeBodies = []string{`{"is_sleeping":false}`, `{"is_sleeping":true}`}
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.sleepLevel["restore-l1"] = 1
	e.reconcileSleep(context.Background(), proto.InstanceDesiredState{UUID: "restore-l1", Sleepable: true, DesiredSleep: 0, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstRunning || st.SleepLevel != 1 || st.Phase != proto.PhaseSleep {
		t.Fatalf("restored L1 report = %+v", st)
	}
	if !strings.Contains(st.Message, "failed") {
		t.Fatalf("restored L1 report lost failure detail: %+v", st)
	}
}

func TestReconcileSleepCanceledOrderReportsUncertain(t *testing.T) {
	h := newSleepHTTPServer(t)
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.reconcileSleep(ctx, proto.InstanceDesiredState{UUID: "canceled-sleep", Sleepable: true, DesiredSleep: 1, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstFailed || !strings.Contains(st.Message, "uncertain") {
		t.Fatalf("canceled order report = %+v", st)
	}
}

func TestReconcileSleepProbeErrorReportsUncertain(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/wake_up"] = http.StatusInternalServerError
	h.body["/is_sleeping"] = `not-json`
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.sleepLevel["probe-error"] = 1
	e.reconcileSleep(context.Background(), proto.InstanceDesiredState{UUID: "probe-error", Sleepable: true, DesiredSleep: 0, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstFailed || !strings.Contains(st.Message, "uncertain") {
		t.Fatalf("probe error report = %+v", st)
	}
}

// The connection of the sleep order broke without an answer, and the engine
// says it sleeps: the order ran to the end, so it is reported applied.
func TestReconcileSleepReportsAppliedWhenSleepCompletedDespiteATransportError(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.drop["/sleep"] = true
	h.body["/is_sleeping"] = `{"is_sleeping":true}`
	reported := make(chan proto.InstanceStatus, 1)
	e := newHermeticEngine(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.reconcileSleep(context.Background(), proto.InstanceDesiredState{UUID: "late-sleep", Sleepable: true, DesiredSleep: 1, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstRunning || st.SleepLevel != 1 || st.Phase != "" || st.Message != "" {
		t.Fatalf("late sleep report = %+v", st)
	}
}

// vLLM answers a sleep that failed part way with 500 while /is_sleeping reads
// true (its scheduler paused before the offload). Measured on vLLM 0.30: the
// weights were partly moved out, the wake that followed was a no-op and the
// next request crashed the engine with an illegal memory access. The agent
// must not report that engine asleep: it fails the generation, closed to the
// gateway, for the controller to rebuild.
func TestReconcileSleepRefusedByTheEngineYetSleepingIsUncertain(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/sleep"] = http.StatusInternalServerError
	h.body["/is_sleeping"] = `{"is_sleeping":true}`
	reported := make(chan proto.InstanceStatus, 1)
	e := New(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.servingEpoch["half-offloaded"] = 7
	e.reconcileSleep(context.Background(), proto.InstanceDesiredState{UUID: "half-offloaded", Sleepable: true, DesiredSleep: 1, APIPort: h.port(t)})
	st := <-reported
	if st.State != proto.InstFailed || st.Phase != proto.PhaseSleep || !strings.Contains(st.Message, "uncertain") || !strings.Contains(st.Message, "http 500") {
		t.Fatalf("refused sleep report = %+v, want a failed generation in the sleep phase", st)
	}
	if got := e.sleepLevel["half-offloaded"]; got != 0 {
		t.Fatalf("level = %d, want the level the agent last applied (0)", got)
	}
	if _, watched := e.servingEpoch["half-offloaded"]; watched {
		t.Fatal("the failed generation is still watched for its container's exit")
	}
	if st.L1HostBufferHeld {
		t.Fatalf("a refused sleep claimed the host buffer: %+v", st)
	}
	want := []string{"POST /sleep?level=1&mode=wait", "GET /is_sleeping"}
	if strings.Join(h.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %+v, want %+v (no retry of the ambiguous order)", h.calls, want)
	}
}

// The agent reports that an engine holds the pinned host buffer of an L1
// sleep once one went through, and keeps saying so awake; an L2 sleep pins
// none, and forgetting the instance forgets it.
func TestReconcileSleepReportsTheL1HostBuffer(t *testing.T) {
	h := newSleepHTTPServer(t)
	reported := make(chan proto.InstanceStatus, 8)
	e := New(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "buffer", Sleepable: true, APIPort: h.port(t), Ports: []int{h.port(t)}}

	spec.DesiredSleep = 2
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 2 || st.L1HostBufferHeld {
		t.Fatalf("L2 report = %+v, want no host buffer", st)
	}
	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 0 || st.L1HostBufferHeld {
		t.Fatalf("wake from L2 report = %+v, want no host buffer", st)
	}
	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 1 || !st.L1HostBufferHeld {
		t.Fatalf("L1 report = %+v, want the host buffer held", st)
	}
	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 0 || !st.L1HostBufferHeld {
		t.Fatalf("wake from L1 report = %+v, want the host buffer still held", st)
	}
	e.forgetSleepState(spec.UUID)
	if e.holdsL1HostBuffer(spec.UUID) {
		t.Fatal("a forgotten instance still holds a host buffer")
	}
}

// After an agent restart the engine only says whether it is asleep. The level
// comes from the order the server last pushed; an engine asleep against an
// awake order is read as L2, whose wake sequence is right for either depth.
// A level this agent applied itself is never overridden by a re-adoption.
func TestAdoptSleepLevelTakesTheLevelFromTheDesiredState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		desired int
		want    int
	}{
		{name: "awake", body: `{"is_sleeping":false}`, desired: 2, want: 0},
		{name: "asleep at pushed L2", body: `{"is_sleeping":true}`, desired: 2, want: 2},
		{name: "asleep at pushed L1", body: `{"is_sleeping":true}`, desired: 1, want: 1},
		{name: "asleep against awake order", body: `{"is_sleeping":true}`, desired: 0, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSleepHTTPServer(t)
			h.body["/is_sleeping"] = tc.body
			e := newHermeticEngine(&headlessTestDriver{}, nil, slog.Default(), t.TempDir())
			spec := proto.InstanceDesiredState{UUID: "adopt", Sleepable: true, DesiredSleep: tc.desired, APIPort: h.port(t)}
			if err := e.adoptSleepLevel(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			if got := e.sleepLevel[spec.UUID]; got != tc.want {
				t.Fatalf("adopted level = %d, want %d", got, tc.want)
			}
			// The next reconcile adopts the same container again with a
			// changed order; what this agent already knows wins.
			spec.DesiredSleep = 0
			calls := len(h.calls)
			if err := e.adoptSleepLevel(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			if got := e.sleepLevel[spec.UUID]; got != tc.want || len(h.calls) != calls {
				t.Fatalf("re-adoption changed level to %d (probe calls %d→%d)", got, calls, len(h.calls))
			}
		})
	}
}

func TestProbeSleepStateSupportsBooleanAndObjectResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		err  bool
	}{
		{name: "true", body: "true", want: 1},
		{name: "false", body: "false", want: 0},
		{name: "is_sleeping", body: `{"is_sleeping":true}`, want: 1},
		{name: "sleeping", body: `{"sleeping":true}`, want: 1},
		{name: "awake-object", body: `{"is_sleeping":false,"sleeping":false}`, want: 0},
		{name: "invalid", body: `{"unexpected":}`, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSleepHTTPServer(t)
			h.body["/is_sleeping"] = tc.body
			e := newHermeticEngine(&headlessTestDriver{}, nil, slog.Default(), t.TempDir())
			got, err := e.probeSleepState(context.Background(), proto.InstanceDesiredState{APIPort: h.port(t)})
			if tc.err {
				if err == nil {
					t.Fatalf("probe error = nil, want invalid response error")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("probe = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestProbeAndReportPrewarmsOnceAndSkipsWhenDesiredSleepNonzero(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}
	reported := make(chan proto.InstanceStatus, 16)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "prewarm-1", Sleepable: true, DesiredSleep: 0, APIPort: h.port(t), StartTimeout: 1}
	e.probeAndReport(context.Background(), spec, "container")
	first := <-reported
	second := <-reported
	if first.State != proto.InstOptimizing || second.State != proto.InstRunning {
		t.Fatalf("prewarm reports = %+v, %+v", first, second)
	}
	if !second.L1HostBufferHeld {
		t.Fatalf("running report after the prewarm = %+v, want the host buffer held", second)
	}
	if len(h.calls) != 4 || h.calls[0] != "GET /health" || h.calls[1] != "POST /sleep?level=1" || h.calls[2] != "POST /wake_up" || h.calls[3] != "GET /openapi.json" {
		t.Fatalf("first prewarm calls = %+v", h.calls)
	}

	e.probeAndReport(context.Background(), spec, "container")
	if st := <-reported; st.State != proto.InstRunning {
		t.Fatalf("second probe report = %+v", st)
	}
	if len(h.calls) != 6 || h.calls[4] != "GET /health" || h.calls[5] != "GET /openapi.json" {
		t.Fatalf("prewarm repeated unexpectedly: calls=%+v", h.calls)
	}

	h2 := newSleepHTTPServer(t)
	reported2 := make(chan proto.InstanceStatus, 4)
	e2 := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported2 <- st }, slog.Default(), t.TempDir())
	spec2 := proto.InstanceDesiredState{UUID: "prewarm-sleeping", Sleepable: true, DesiredSleep: 1, APIPort: h2.port(t), StartTimeout: 1}
	e2.probeAndReport(context.Background(), spec2, "container")
	if st := <-reported2; st.State != proto.InstRunning || st.L1HostBufferHeld {
		t.Fatalf("desired sleeping report = %+v, want running without a host buffer (no prewarm)", st)
	}
	for _, call := range h2.calls {
		if strings.HasPrefix(call, "POST /sleep") || call == "POST /wake_up" {
			t.Fatalf("desired sleep unexpectedly prewarmed: calls=%+v", h2.calls)
		}
	}
}

func TestAdoptRunningContainerSkipsPrewarm(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.body["/is_sleeping"] = "false"
	driver := &headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}
	reported := make(chan proto.InstanceStatus, 4)
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	spec := proto.InstanceDesiredState{UUID: "adopt-prewarmed", Sleepable: true, DesiredSleep: 0, APIPort: h.port(t), StartTimeout: 1}
	e.startOrAdopt(context.Background(), spec)
	// An adopting agent does not know whether the engine holds a host buffer
	// and says none; the server keeps what the earlier agent reported.
	if st := <-reported; st.State != proto.InstRunning || st.L1HostBufferHeld {
		t.Fatalf("adopt report=%+v", st)
	}
	for _, call := range h.calls {
		if strings.HasPrefix(call, "POST /sleep") || call == "POST /wake_up" {
			t.Fatalf("adopted container repeated prewarm: calls=%+v", h.calls)
		}
	}
	if !e.prewarmed[spec.UUID] {
		t.Fatal("adopted container was not marked prewarmed")
	}
}

func TestProbeAndReportPrewarmFailureReportsFailed(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/sleep"] = http.StatusInternalServerError
	reported := make(chan proto.InstanceStatus, 16)
	e := newHermeticEngine(&headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir())
	e.probeAndReport(context.Background(), proto.InstanceDesiredState{UUID: "prewarm-fail", Sleepable: true, APIPort: h.port(t), StartTimeout: 1}, "container")
	got := []proto.InstanceStatus{<-reported, <-reported}
	if got[0].State != proto.InstOptimizing || got[1].State != proto.InstFailed || !strings.Contains(got[1].Message, "sleep prewarm failed") {
		t.Fatalf("prewarm failure reports = %+v", got)
	}
}

type imageToleranceDriver struct {
	headlessTestDriver
	exists    []bool
	pullErr   error
	pullErrs  map[string]error
	pullRefs  []string
	pullProxy []string
	pullCalls int
	runCalls  int
	// present, when non-nil, makes the fake answer per reference like a real
	// runtime: only a reference that was pulled or tagged exists, and a digest
	// pull is recorded under the name it came from. exists is then ignored.
	present   map[string]bool
	tags      [][2]string
	runImages []string
	// hook runs at the start of every pull, to model an image that appears
	// while the pull is failing.
	hook func(ref string)
}

func (d *imageToleranceDriver) ImageExists(_ context.Context, ref string) (bool, error) {
	if d.present != nil {
		return d.present[ref], nil
	}
	if len(d.exists) == 0 {
		return false, nil
	}
	got := d.exists[0]
	d.exists = d.exists[1:]
	return got, nil
}

func (d *imageToleranceDriver) PullImage(ctx context.Context, image string, _ func(string)) error {
	d.pullCalls++
	d.pullRefs = append(d.pullRefs, image)
	d.pullProxy = append(d.pullProxy, runtime.PullProxy(ctx))
	if d.hook != nil {
		d.hook(image)
	}
	err := d.pullErr
	if d.pullErrs != nil {
		err = d.pullErrs[image]
	}
	if err == nil && d.present != nil {
		d.present[image] = true
	}
	return err
}

func TestStartOrAdoptCarriesTheSelectedImageProxyOnlyToThePull(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{exists: []bool{false, true}}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{
		UUID: "image-proxy", Image: "repo:v1", ImagePullRefs: []string{"repo:v1"},
		ImagePullExplicit: true, ImagePullProxy: "http://proxy.example:3128",
		APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1,
	})
	if len(driver.pullProxy) != 1 || driver.pullProxy[0] != "http://proxy.example:3128" {
		t.Fatalf("pull proxy = %v", driver.pullProxy)
	}
}

// TagImage refuses a digest target the way docker does.
func (d *imageToleranceDriver) TagImage(_ context.Context, source, target string) error {
	if strings.Contains(target, "@") {
		return errors.New("refusing to create a tag with a digest reference")
	}
	if d.present != nil && !d.present[source] {
		return errors.New("No such image: " + source)
	}
	d.tags = append(d.tags, [2]string{source, target})
	if d.present != nil {
		d.present[target] = true
	}
	return nil
}

func (d *imageToleranceDriver) Run(_ context.Context, ws runtime.WorkloadSpec) (string, error) {
	d.runCalls++
	d.runImages = append(d.runImages, ws.Image)
	d.status = runtime.Status{Exists: true, Running: true}
	return "container", nil
}

func TestStartOrAdoptToleratesPullFailureWhenImageAppearsLocally(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{exists: []bool{false, true}, pullErr: errors.New("registry unavailable")}
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{UUID: "image-race", Image: "local/build:latest", APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1})
	if driver.pullCalls != 1 || driver.runCalls != 1 {
		t.Fatalf("pull/run calls = %d/%d, want 1/1", driver.pullCalls, driver.runCalls)
	}
	var last proto.InstanceStatus
	for {
		select {
		case last = <-reported:
		default:
			if last.State != proto.InstRunning {
				t.Fatalf("final status = %+v, want running", last)
			}
			return
		}
	}
}

func TestStartOrAdoptFailsWhenPullAndImageRecheckBothFail(t *testing.T) {
	driver := &imageToleranceDriver{exists: []bool{false, false}, pullErr: errors.New("registry unavailable")}
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{UUID: "image-failed", Image: "missing:latest", APIPort: 1, StartTimeout: 1})
	if driver.pullCalls != 1 || driver.runCalls != 0 {
		t.Fatalf("pull/run calls = %d/%d, want 1/0", driver.pullCalls, driver.runCalls)
	}
	var got proto.InstanceStatus
	for {
		select {
		case got = <-reported:
			if got.State == proto.InstFailed {
				if !strings.Contains(got.Message, "image sources failed") {
					t.Fatalf("failed status message = %q", got.Message)
				}
				return
			}
		default:
			t.Fatalf("no failed status; last=%+v", got)
		}
	}
}

func TestStartOrAdoptFallsBackAcrossImagePullRefs(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{
		exists:   []bool{false, false},
		pullErrs: map[string]error{"mirror-1/repo:v1": errors.New("mirror unavailable"), "mirror-2/repo:v1": nil},
	}
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{UUID: "image-fallback", Image: "repo:v1", ImagePullRefs: []string{"mirror-1/repo:v1", "mirror-2/repo:v1"}, APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1})
	if driver.pullCalls != 2 || driver.runCalls != 1 {
		t.Fatalf("pull/run calls = %d/%d, want 2/1", driver.pullCalls, driver.runCalls)
	}
	if len(driver.pullRefs) != 2 || driver.pullRefs[0] != "mirror-1/repo:v1" || driver.pullRefs[1] != "mirror-2/repo:v1" {
		t.Fatalf("pull refs = %v", driver.pullRefs)
	}
}

func TestStartOrAdoptReportsImageVerificationStages(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{exists: []bool{false, true}}
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{UUID: "image-stages", Image: "repo:v1", APIPort: h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1})

	var phases []string
	verified := false
	for {
		select {
		case st := <-reported:
			phases = append(phases, st.Phase)
			if st.Phase == "image_verify" && st.Progress == 1 {
				verified = true
			}
		default:
			if !verified {
				t.Fatalf("image verification completion was not reported: phases=%v", phases)
			}
			if !containsString(phases, "image_pull") || !containsString(phases, "container_prepare") {
				t.Fatalf("startup phases = %v", phases)
			}
			return
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// orphanDriver records the lifecycle calls an orphan sweep makes and lets a
// test inject listing and removal failures.
type orphanDriver struct {
	headlessTestDriver
	listed    []string
	listErr   error
	stopErr   map[string]error
	removeErr map[string]error
	stopped   []string
	removed   []string
}

func (d *orphanDriver) ListContainers(context.Context) ([]string, error) { return d.listed, d.listErr }

func (d *orphanDriver) Stop(_ context.Context, id string) error {
	d.stopped = append(d.stopped, id)
	return d.stopErr[id]
}

func (d *orphanDriver) Remove(_ context.Context, id string) error {
	d.removed = append(d.removed, id)
	return d.removeErr[id]
}

// orphanTestEngine builds an engine whose desired set holds exactly the given
// instance uuids.
func orphanTestEngine(t *testing.T, driver runtime.Driver, uuids ...string) *Engine {
	t.Helper()
	e := newHermeticEngine(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	for _, uuid := range uuids {
		e.desired[uuid] = proto.InstanceDesiredState{UUID: uuid, DesiredState: "running"}
	}
	return e
}

func TestCleanupOrphansRemovesOnlyUnmanagedContainersOutsideTheDesiredSet(t *testing.T) {
	active := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	orphan := "11111111-2222-3333-4444-555555555555"
	driver := &orphanDriver{listed: []string{
		containerName(active),
		containerName(orphan),
		"unmanaged-container", // not created by gpuworks
		"gpuworks-probe-42",   // gpuworks-named, but not an instance container
	}}
	e := orphanTestEngine(t, driver, active)

	if err := e.CleanupOrphans(context.Background(), e.driver.ListContainers); err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	// The desired container and the unmanaged one must survive untouched: the
	// first is a live workload, the second does not belong to gpuworks.
	want := []string{containerName(orphan), "gpuworks-probe-42"}
	if !slices.Equal(driver.stopped, want) {
		t.Fatalf("stopped containers = %v, want %v", driver.stopped, want)
	}
	if !slices.Equal(driver.removed, want) {
		t.Fatalf("removed containers = %v, want %v", driver.removed, want)
	}
	if slices.Contains(driver.removed, containerName(active)) {
		t.Fatalf("the desired container was swept: %v", driver.removed)
	}
	if slices.Contains(driver.removed, "unmanaged-container") {
		t.Fatalf("a container gpuworks does not manage was swept: %v", driver.removed)
	}
}

func TestCleanupOrphansReportsListingFailureInsteadOfReclaimingNothing(t *testing.T) {
	driver := &orphanDriver{listErr: errors.New("udocker runtime is unavailable")}
	e := orphanTestEngine(t, driver)
	failure := func(context.Context) ([]string, error) { return nil, driver.listErr }

	err := e.CleanupOrphans(context.Background(), failure)
	if err == nil {
		t.Fatal("a failed container listing was silently treated as an empty node")
	}
	if !errors.Is(err, driver.listErr) {
		t.Fatalf("error does not carry the listing failure: %v", err)
	}
	if len(driver.stopped) != 0 || len(driver.removed) != 0 {
		t.Fatalf("sweep acted on a failed listing: stopped=%v removed=%v", driver.stopped, driver.removed)
	}

	// A missing lister is a wiring mistake, not a node with nothing to clean.
	if err := e.CleanupOrphans(context.Background(), nil); err == nil {
		t.Fatal("a nil container lister was accepted")
	}
}

func TestCleanupOrphansReportsContainersThatSurvivedTheSweep(t *testing.T) {
	stoppedOrphan := "11111111-2222-3333-4444-555555555555"
	stubbornOrphan := "66666666-7777-8888-9999-000000000000"
	driver := &orphanDriver{
		listed:    []string{containerName(stoppedOrphan), containerName(stubbornOrphan)},
		stopErr:   map[string]error{containerName(stubbornOrphan): errors.New("stop timed out")},
		removeErr: map[string]error{containerName(stoppedOrphan): errors.New("rootfs is still mounted")},
	}
	e := orphanTestEngine(t, driver)

	err := e.CleanupOrphans(context.Background(), e.driver.ListContainers)
	if err == nil {
		t.Fatal("orphan containers that survived the sweep were reported as reclaimed")
	}
	for _, want := range []string{containerName(stoppedOrphan), containerName(stubbornOrphan)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %s: %v", want, err)
		}
	}
	// A container whose stop failed is never removed, so a live workload cannot
	// be deleted underneath its still-running process.
	if slices.Contains(driver.removed, containerName(stubbornOrphan)) {
		t.Fatalf("container was removed after Stop failed: %v", driver.removed)
	}
}

func TestTeardownReportsRemovalFailureToTheControlPlane(t *testing.T) {
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	name := containerName(uuid)
	var reported []proto.InstanceStatus
	driver := &orphanDriver{
		headlessTestDriver: headlessTestDriver{status: runtime.Status{Exists: true, Running: true}},
		removeErr:          map[string]error{name: errors.New("rootfs cleanup failed")},
	}
	stateDir := t.TempDir()
	configDir := filepath.Join(stateDir, partitionStateRoot, "instances", sanitizeID(uuid))
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported = append(reported, st) }, slog.Default(), stateDir)

	e.teardown(context.Background(), uuid)

	if len(reported) != 1 {
		t.Fatalf("reported statuses = %+v, want exactly one", reported)
	}
	st := reported[0]
	if st.State != proto.InstRemoving {
		t.Fatalf("teardown failure reported as %q, want %q", st.State, proto.InstRemoving)
	}
	if !strings.Contains(st.Message, "rootfs cleanup failed") {
		t.Fatalf("teardown message does not carry the failure: %q", st.Message)
	}
	if _, err := os.Stat(configDir); err != nil {
		t.Fatalf("partition state was removed before the container: %v", err)
	}
}

type statusErrorDriver struct {
	orphanDriver
}

func (d *statusErrorDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{}, errors.New("inspect failed")
}

func TestTeardownRetainsStateWhenContainerInspectionFails(t *testing.T) {
	stateDir, uuid := t.TempDir(), "status-error-instance"
	configDir := filepath.Join(stateDir, partitionStateRoot, "instances", sanitizeID(uuid))
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driver := &statusErrorDriver{}
	var reported proto.InstanceStatus
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported = st }, slog.Default(), stateDir)
	if e.teardown(context.Background(), uuid) || reported.State != proto.InstRemoving || len(driver.removed) != 0 {
		t.Fatalf("inspection failure was treated as removal: %+v, removed=%v", reported, driver.removed)
	}
	if _, err := os.Stat(configDir); err != nil {
		t.Fatalf("partition state was removed after failed inspection: %v", err)
	}
}

func TestTeardownReportsStoppedWhenTheContainerIsGone(t *testing.T) {
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var reported []proto.InstanceStatus
	driver := &orphanDriver{headlessTestDriver: headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}}
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported = append(reported, st) }, slog.Default(), t.TempDir())

	e.teardown(context.Background(), uuid)

	if len(reported) != 1 || reported[0].State != proto.InstStopped || reported[0].Message != "" {
		t.Fatalf("reported statuses = %+v, want a single clean stopped status", reported)
	}
	if !slices.Equal(driver.removed, []string{containerName(uuid)}) {
		t.Fatalf("removed containers = %v", driver.removed)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
