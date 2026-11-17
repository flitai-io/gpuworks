package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

func causeCodes(f *proto.RuntimeFailure) []string {
	var out []string
	for _, c := range f.Causes {
		out = append(out, c.Code)
	}
	return out
}

func TestLivenessReportsAnExitedContainerOnceWithItsCode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		oom        bool
		logs       string
		want       string
		wantCode   string
		wantCauses []string
	}{
		{name: "exit zero", code: 0, want: "container exited while running (code 0) with no output", wantCode: proto.ErrorContainerExited, wantCauses: []string{enginelog.CodeNoOutput}},
		{name: "error", code: 3, logs: "INFO serving\nRuntimeError: CUDA error: an illegal memory access was encountered\n", want: "container exited while running (code 3): RuntimeError: CUDA error: an illegal memory access was encountered", wantCode: proto.ErrorContainerExited},
		{name: "oom flag", code: 137, oom: true, want: "container OOM-killed while running (code 137)", wantCode: proto.ErrorContainerOOMKilled, wantCauses: []string{enginelog.CodeNoOutput}},
		{name: "oom from logs", code: 137, logs: "torch.OutOfMemoryError: CUDA out of memory", want: "container OOM-killed while running (code 137)", wantCode: proto.ErrorContainerOOMKilled, wantCauses: []string{enginelog.CodeCUDAOOM}},
		{name: "sigkill without oom", code: 137, logs: "INFO serving", want: "container exited while running (code 137)", wantCode: proto.ErrorContainerExited},
		// What gpu3 printed when a running slice's limit was cut below its
		// holding: the engine's own last word is the 500 it served.
		{name: "dgpu limit", code: 0, logs: "[HAMI-core ERROR (pid:192 thread=140279431087936 allocator.c:52)]: Device 0 OOM 5429658624 / 4294967296\n(EngineCore pid=192) ERROR 10-18 14:42:47 [dump_input.py:72] Dumping input data\n(APIServer pid=1) INFO:     127.0.0.1:39734 - \"POST /v1/chat/completions HTTP/1.1\" 500 Internal Server Error\n", want: "container exited while running (code 0): dGPU memory limit reached on GPU 0: the engine needed 5.1 GiB, its limit is 4.0 GiB", wantCode: proto.ErrorContainerExited, wantCauses: []string{enginelog.CodeDGPUMemoryLimit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := newLivenessDriver()
			driver.logs = tc.logs
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("exit-"+strings.ReplaceAll(tc.name, " ", "-"), healthyEngine(t))
			serve(t, e, log, spec)
			name := containerName(spec.UUID)

			e.checkLiveness(context.Background())
			if failed := log.states(proto.InstFailed); len(failed) != 0 {
				t.Fatalf("a running container was reported failed: %+v", failed)
			}
			driver.exit(name, tc.code, tc.oom)
			e.checkLiveness(context.Background())
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].Message != tc.want || failed[0].UUID != spec.UUID {
				t.Fatalf("failed reports = %+v, want one %q", failed, tc.want)
			}
			f := failed[0].RuntimeFailure
			if f == nil || f.Code != tc.wantCode || f.Phase != proto.PhaseServing || f.ExitCode == nil || *f.ExitCode != tc.code || f.OOMKilled != (tc.wantCode == proto.ErrorContainerOOMKilled) {
				t.Fatalf("runtime failure = %+v, want %s exit %d", f, tc.wantCode, tc.code)
			}
			if got := causeCodes(f); !slices.Equal(got, tc.wantCauses) {
				t.Fatalf("causes = %v, want %v", got, tc.wantCauses)
			}
			if tc.wantCode == proto.ErrorContainerExited && strings.Contains(tc.want, "): ") && !strings.Contains(tc.want, "dGPU") && (len(f.Evidence) != 1 || !strings.HasSuffix(tc.want, f.Evidence[0])) {
				t.Fatalf("evidence = %q, want the root-cause line", f.Evidence)
			}
			if driver.exists(name) {
				t.Fatal("the dead container was not reclaimed before the failure was reported")
			}
			for range 3 {
				e.checkLiveness(context.Background())
			}
			if got := log.states(proto.InstFailed); len(got) != 1 {
				t.Fatalf("the failure was reported again: %+v", got)
			}
			if got := log.states(proto.InstRemoving); len(got) != 0 {
				t.Fatalf("unexpected removing reports: %+v", got)
			}
			waitIdle(t, e, spec.UUID)
		})
	}
}

func TestLivenessReportsAContainerRemovedOutsideTheAgentOnce(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("removed-by-hand", healthyEngine(t))
	serve(t, e, log, spec)

	driver.deleteContainer(containerName(spec.UUID))
	e.checkLiveness(context.Background())
	e.checkLiveness(context.Background())
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "disappeared while running") {
		t.Fatalf("failed reports = %+v", failed)
	}
	if f := failed[0].RuntimeFailure; f == nil || f.Code != proto.ErrorContainerDisappeared || f.Phase != proto.PhaseServing || f.ExitCode != nil {
		t.Fatalf("runtime failure = %+v", f)
	}
}

func TestLivenessSkipsARoundWhoseInspectFails(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("inspect-fails", healthyEngine(t))
	serve(t, e, log, spec)

	driver.exit(containerName(spec.UUID), 2, false)
	driver.set(func(d *livenessDriver) { d.statusErrs = 1 })
	e.checkLiveness(context.Background())
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("an inspect error was taken for an exit: %+v", failed)
	}
	if !driver.exists(containerName(spec.UUID)) {
		t.Fatal("an inspect error reclaimed the container")
	}
	e.checkLiveness(context.Background())
	if failed := log.states(proto.InstFailed); len(failed) != 1 || !strings.Contains(failed[0].Message, "code 2") {
		t.Fatalf("failed reports after a clean inspect = %+v", failed)
	}
}

// A round's runtime calls must not grow with the number of instances: one
// listing covers every running container, and only a container the listing
// does not show as running is inspected.
func TestLivenessRoundListsOnceHoweverManyInstancesRun(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	const instances = 6
	specs := make([]proto.InstanceDesiredState, instances)
	for i := range specs {
		specs[i] = runningSpec(fmt.Sprintf("packed-%d", i), healthyEngine(t))
	}
	e.SetDesiredFull(context.Background(), specs)
	log.waitFor(t, proto.InstRunning, instances)
	for _, spec := range specs {
		waitWatched(t, e, spec.UUID)
	}

	driver.resetCalls()
	e.checkLiveness(context.Background())
	if lists, inspects := driver.calls(); lists != 1 || len(inspects) != 0 {
		t.Fatalf("a round over %d running instances made %d listing(s) and inspects %v, want one listing and no inspect", instances, lists, inspects)
	}

	exited := containerName(specs[2].UUID)
	driver.exit(exited, 9, false)
	driver.resetCalls()
	e.checkLiveness(context.Background())
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].UUID != specs[2].UUID || !strings.Contains(failed[0].Message, "code 9") {
		t.Fatalf("failed reports = %+v, want only %s with its exit code", failed, specs[2].UUID)
	}
	lists, inspects := driver.calls()
	if lists != 1 {
		t.Fatalf("the round made %d listings, want 1", lists)
	}
	for name := range inspects {
		if name != exited {
			t.Fatalf("running container %s was inspected although the listing showed it: %v", name, inspects)
		}
	}
	if inspects[exited] == 0 {
		t.Fatalf("the exited container was retired without an inspect: %v", inspects)
	}
}

func TestLivenessWithNothingWatchedDoesNotCallTheRuntime(t *testing.T) {
	driver := newLivenessDriver()
	e, _ := newLivenessEngine(t, driver)
	e.checkLiveness(context.Background())
	if lists, inspects := driver.calls(); lists != 0 || len(inspects) != 0 {
		t.Fatalf("an idle round made %d listing(s) and inspects %v", lists, inspects)
	}
}

// The listing never decides: when it fails, comes back empty or leaves a
// running container out, the round falls back to that container's inspect,
// which keeps a healthy instance and still catches a dead one.
func TestLivenessFallsBackToInspectWhenTheListingDoesNotShowAContainer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(d *livenessDriver, container string)
	}{
		{name: "listing fails", break_: func(d *livenessDriver, _ string) { d.listErrs = 2 }},
		{name: "listing leaves the container out", break_: func(d *livenessDriver, container string) { d.unlisted[container] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("unlisted-"+strings.ReplaceAll(tc.name, " ", "-"), healthyEngine(t))
			other := runningSpec("listed-"+strings.ReplaceAll(tc.name, " ", "-"), healthyEngine(t))
			e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec, other})
			log.waitFor(t, proto.InstRunning, 2)
			waitWatched(t, e, spec.UUID)
			waitWatched(t, e, other.UUID)
			name := containerName(spec.UUID)

			driver.set(func(d *livenessDriver) { tc.break_(d, name) })
			driver.resetCalls()
			e.checkLiveness(context.Background())
			if failed := log.states(proto.InstFailed); len(failed) != 0 {
				t.Fatalf("a running container missing from the listing was reported failed: %+v", failed)
			}
			if !driver.exists(name) {
				t.Fatal("a running container missing from the listing was reclaimed")
			}
			if _, inspects := driver.calls(); inspects[name] != 1 {
				t.Fatalf("inspects = %v, want the unlisted container inspected once", inspects)
			}

			driver.exit(name, 4, false)
			e.checkLiveness(context.Background())
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].UUID != spec.UUID || !strings.Contains(failed[0].Message, "code 4") {
				t.Fatalf("failed reports = %+v, want only %s with its exit code", failed, spec.UUID)
			}
		})
	}
}

// Every sleep level keeps the engine process, and so its container, alive:
// a sleeping instance is never reported. One whose container really exits is
// reported like an awake one, and its sleep level is forgotten with it.
func TestLivenessSleepingInstance(t *testing.T) {
	for _, level := range []int{1, 2} {
		t.Run(fmt.Sprintf("L%d", level), func(t *testing.T) {
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec(fmt.Sprintf("sleeper-%d", level), healthyEngine(t))
			spec.Sleepable, spec.DesiredSleep = true, level
			serve(t, e, log, spec)
			waitIdle(t, e, spec.UUID)
			e.mu.Lock()
			asleep := e.processOfLocked(spec.UUID).sleepLevel
			e.mu.Unlock()
			if asleep != level {
				t.Fatalf("sleep level = %d, want %d", asleep, level)
			}

			for range 3 {
				e.checkLiveness(context.Background())
			}
			if failed := log.states(proto.InstFailed); len(failed) != 0 {
				t.Fatalf("a sleeping instance was reported failed: %+v", failed)
			}

			driver.exit(containerName(spec.UUID), 1, false)
			e.checkLiveness(context.Background())
			if failed := log.states(proto.InstFailed); len(failed) != 1 || failed[0].Phase != "" {
				t.Fatalf("failed reports = %+v, want one without the sleep phase", failed)
			}
			e.mu.Lock()
			remembered := e.processes[spec.UUID]
			e.mu.Unlock()
			if remembered != nil {
				t.Fatalf("the dead sleeper's process facts were kept for a later container: %+v", *remembered)
			}
		})
	}
}

func TestLivenessHeadlessWorker(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := proto.InstanceDesiredState{UUID: "rank-1", DesiredState: "running", Headless: true, NodeRank: 1, StartTimeout: 30}
	serve(t, e, log, spec)

	driver.exit(containerName(spec.UUID), 9, false)
	e.checkLiveness(context.Background())
	e.checkLiveness(context.Background())
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].Message != "headless worker exited while running (code 9) with no output" {
		t.Fatalf("failed reports = %+v", failed)
	}
}

// A startup failure is the startup probe's to report; the monitor never
// repeats it, even though the instance stays desired with no container.
func TestLivenessDoesNotRepeatAStartupFailure(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	// Nothing listens on the health port, so the probe keeps inspecting.
	spec := runningSpec("crashes-at-startup", 1)
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	deadline := time.Now().Add(5 * time.Second)
	for !driver.exists(containerName(spec.UUID)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	driver.exit(containerName(spec.UUID), 1, false)
	log.waitFor(t, proto.InstFailed, 1)
	waitIdle(t, e, spec.UUID)
	for range 3 {
		e.checkLiveness(context.Background())
	}
	if failed := log.states(proto.InstFailed); len(failed) != 1 || !strings.Contains(failed[0].Message, "container exited (code 1)") {
		t.Fatalf("failed reports = %+v, want only the startup probe's", failed)
	}
}

// An engine that dies at startup leaves the failed instance its last error
// line: removal deletes the output, and this is what remains of it. With no
// output at all (a full disk cannot write it) the message says so.
func TestStartupFailureCarriesTheEnginesLastErrorLine(t *testing.T) {
	// vLLM's last line only points up ("See root cause above"); the failed
	// instance carries the worker's exception instead.
	vllmFailure, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "enginelog", "testdata", "vllm-max-num-seqs-failure.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ logs, want, cause string }{
		"vllm root cause above": {logs: string(vllmFailure), want: "container exited (code 2): ValueError: max_num_seqs (1024) exceeds available Mamba cache blocks (839). Each decode sequence requires one Mamba cache block, so CUDA graph capture cannot proceed. Please lower max_num_seqs to at most 839 or increase gpu_memory_utilization."},
		"argument error": {logs: "usage: vllm [-h] [-v]\nvllm: error: unrecognized arguments: --definitely-not-a-flag\n",
			want: "container exited (code 2): vllm: error: unrecognized arguments: --definitely-not-a-flag"},
		"no device": {logs: "Traceback (most recent call last):\n  File \"/usr/local/bin/vllm\", line 10\n\x1b[31mRuntimeError: Failed to infer device type\x1b[0m\n",
			want: "container exited (code 2): RuntimeError: Failed to infer device type"},
		"no output": {logs: "", want: "container exited (code 2) with no output", cause: enginelog.CodeNoOutput},
		"kv cache": {logs: "ValueError: The model's max seq len (32768) is larger than the maximum number of tokens that can be stored in KV cache (12000).\n",
			want: "container exited (code 2): ValueError: The model's max seq len (32768) is larger than the maximum number of tokens that can be stored in KV cache (12000).", cause: enginelog.CodeKVCacheInsufficient},
		"glibc": {logs: "/usr/bin/python3: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.33' not found (required by /opt/gpuworks/libvruntime.so)\n",
			want: "container glibc too old: missing GLIBC_2.33; Ascend dGPU images require glibc 2.33 or newer", cause: enginelog.CodeGLIBCTooOld},
	} {
		t.Run(name, func(t *testing.T) {
			driver := newLivenessDriver()
			driver.logs = tc.logs
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("startup-"+strings.ReplaceAll(name, " ", "-"), 1)
			e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
			deadline := time.Now().Add(5 * time.Second)
			for !driver.exists(containerName(spec.UUID)) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			driver.exit(containerName(spec.UUID), 2, false)
			log.waitFor(t, proto.InstFailed, 1)
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].Message != tc.want {
				t.Fatalf("failed reports = %+v, want %q", failed, tc.want)
			}
			f := failed[0].RuntimeFailure
			if f == nil || f.Code != proto.ErrorContainerExited || f.Phase != proto.PhaseHealthCheck || f.ExitCode == nil || *f.ExitCode != 2 || f.OOMKilled {
				t.Fatalf("runtime failure = %+v", f)
			}
			if tc.cause != "" && (len(f.Causes) == 0 || f.Causes[0].Code != tc.cause) {
				t.Fatalf("causes = %+v, want %s first", f.Causes, tc.cause)
			}
			if tc.cause == enginelog.CodeGLIBCTooOld && f.Causes[0].GLIBC != "GLIBC_2.33" {
				t.Fatalf("glibc cause = %+v", f.Causes[0])
			}
			if tc.cause == "" && len(f.Causes) != 0 {
				t.Fatalf("unknown failure got causes %+v", f.Causes)
			}
			// A loader refusal names no error or exception: its line is the
			// cause's evidence, not a root-cause line.
			if tc.cause == enginelog.CodeGLIBCTooOld {
				if len(f.Evidence) != 0 || len(f.Causes[0].Evidence) != 1 {
					t.Fatalf("glibc evidence = %q / %q", f.Evidence, f.Causes[0].Evidence)
				}
			} else if tc.logs != "" && (len(f.Evidence) != 1 || len(f.Evidence[0]) > proto.MaxFailureEvidenceChars) {
				t.Fatalf("evidence = %q, want the bounded root-cause line", f.Evidence)
			}
		})
	}
}

// An ensure in flight owns the instance: its exit is that attempt's to see.
func TestLivenessLeavesAnInstanceInFlightToItsEnsure(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("re-ensured", healthyEngine(t))
	serve(t, e, log, spec)
	runs := 0
	driver.set(func(d *livenessDriver) { runs = d.runs })

	// A reconnect's state sync re-ensures the instance; hold its inspect.
	started, release := driver.holdNextStatus()
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	<-started
	driver.exit(containerName(spec.UUID), 1, false)
	e.checkLiveness(context.Background())
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("an instance in flight was reported failed: %+v", failed)
	}
	release()
	// The ensure finds the dead container itself, replaces it and serves again.
	waitWatched(t, e, spec.UUID)
	driver.set(func(d *livenessDriver) {
		if d.runs != runs+1 {
			t.Errorf("runs = %d, want the ensure to start one replacement after %d", d.runs, runs)
		}
	})
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("failed reports = %+v", failed)
	}
}

// A stop or removal owns the instance as soon as it arrives; the monitor never
// reports a container it tears down, even when that teardown fails.
func TestLivenessIgnoresInstancesBeingStoppedOrRemoved(t *testing.T) {
	t.Run("stopped with cleanup failing", func(t *testing.T) {
		driver := newLivenessDriver()
		e, log := newLivenessEngine(t, driver)
		spec := runningSpec("stopping", healthyEngine(t))
		serve(t, e, log, spec)
		driver.exit(containerName(spec.UUID), 0, false)
		driver.set(func(d *livenessDriver) { d.removeErrs = 100 })
		stopped := spec
		stopped.DesiredState = "stopped"
		e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{stopped}, nil)
		log.waitFor(t, proto.InstRemoving, 1)
		waitIdle(t, e, spec.UUID)
		for range 3 {
			e.checkLiveness(context.Background())
		}
		if failed := log.states(proto.InstFailed); len(failed) != 0 {
			t.Fatalf("an instance being stopped was reported failed: %+v", failed)
		}
	})
	t.Run("removed", func(t *testing.T) {
		driver := newLivenessDriver()
		e, log := newLivenessEngine(t, driver)
		spec := runningSpec("removed", healthyEngine(t))
		serve(t, e, log, spec)
		e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
		log.waitFor(t, proto.InstStopped, 1)
		e.checkLiveness(context.Background())
		if failed := log.states(proto.InstFailed); len(failed) != 0 {
			t.Fatalf("a removed instance was reported failed: %+v", failed)
		}
		e.mu.Lock()
		markers := len(e.servingEpoch)
		e.mu.Unlock()
		if markers != 0 {
			t.Fatalf("a removed instance is still watched: %d marker(s)", markers)
		}
	})
}

// A removal that lands between the inspect and the monitor taking the
// instance wins: the monitor rechecks under the lock and backs off.
func TestLivenessYieldsToARemovalAfterItsInspect(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("removed-mid-inspect", healthyEngine(t))
	serve(t, e, log, spec)
	driver.exit(containerName(spec.UUID), 1, false)

	started, release := driver.holdNextStatus()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.checkLiveness(context.Background())
	}()
	<-started
	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	log.waitFor(t, proto.InstStopped, 1)
	release()
	<-done
	waitIdle(t, e, spec.UUID)
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("a removed instance was reported failed: %+v", failed)
	}
	// Only the removal acknowledged the teardown; the monitor did not take
	// the instance over and run a second one.
	if stopped := log.states(proto.InstStopped); len(stopped) != 1 {
		t.Fatalf("stopped reports = %+v, want the removal's one", stopped)
	}
}

// A removal that arrives while the monitor reclaims the dead container is
// queued behind it like behind any ensure: the teardown is acknowledged as
// stopped and no failure is reported for an instance the server removed.
func TestLivenessHandsAConcurrentRemovalItsTeardown(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("removed-mid-reclaim", healthyEngine(t))
	serve(t, e, log, spec)
	driver.exit(containerName(spec.UUID), 1, false)

	started, release := driver.holdNextRemove()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.checkLiveness(context.Background())
	}()
	<-started
	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	release()
	<-done
	log.waitFor(t, proto.InstStopped, 1)
	waitIdle(t, e, spec.UUID)
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("a removed instance was reported failed: %+v", failed)
	}
	if driver.exists(containerName(spec.UUID)) {
		t.Fatal("the container survived the removal")
	}
}

func TestLivenessReportsAFailedCleanupOnce(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("cleanup-fails", healthyEngine(t))
	serve(t, e, log, spec)
	driver.exit(containerName(spec.UUID), 1, false)
	driver.set(func(d *livenessDriver) { d.removeErrs = 100 })

	for range 3 {
		e.checkLiveness(context.Background())
	}
	removing := log.states(proto.InstRemoving)
	if len(removing) != 1 || !strings.Contains(removing[0].Message, "exited while running (code 1) with no output; cleanup pending") {
		t.Fatalf("removing reports = %+v", removing)
	}
	// The pending cleanup keeps the failure's own reason.
	if f := removing[0].RuntimeFailure; f == nil || f.Code != proto.ErrorContainerExited || f.ExitCode == nil || *f.ExitCode != 1 {
		t.Fatalf("removing reason = %+v", f)
	}
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("failed reports = %+v", failed)
	}
	// The server answers with a stop; the teardown is retried and succeeds.
	driver.set(func(d *livenessDriver) { d.removeErrs = 0 })
	stopped := spec
	stopped.DesiredState = "stopped"
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{stopped})
	log.waitFor(t, proto.InstStopped, 1)
	if driver.exists(containerName(spec.UUID)) {
		t.Fatal("the retried teardown left the container")
	}
}

func TestRunLivenessMonitorReportsWithinOneInterval(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	e.livenessInterval = 20 * time.Millisecond
	spec := runningSpec("monitored", healthyEngine(t))
	serve(t, e, log, spec)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.RunLivenessMonitor(ctx)
	}()
	driver.exit(containerName(spec.UUID), 4, false)
	failed := log.waitFor(t, proto.InstFailed, 1)
	if !strings.Contains(failed[0].Message, "code 4") {
		t.Fatalf("failed report = %+v", failed[0])
	}
	time.Sleep(100 * time.Millisecond)
	if got := log.states(proto.InstFailed); len(got) != 1 {
		t.Fatalf("the monitor reported the exit %d times", len(got))
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the monitor did not stop with its context")
	}
}

// A runtime switch removes every container from the old runtime and starts the
// instance again on the new one; none of that is a failure, and the instance
// is watched again once the new runtime serves it.
func TestSwitchDriverStopsWatchingTheOldRuntimesContainers(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("switched", healthyEngine(t))
	serve(t, e, log, spec)
	next := newLivenessDriver()
	next.name = "udocker"
	started, release := next.holdNextStatus()
	switched := make(chan error, 1)
	go func() { switched <- e.SwitchDriver(context.Background(), next) }()
	// The new runtime has no container yet; until the instance is adopted or
	// started there, the monitor must not read that as a removal.
	<-started
	e.checkLiveness(context.Background())
	release()
	if err := <-switched; err != nil {
		t.Fatal(err)
	}
	waitWatched(t, e, spec.UUID)
	if driver.exists(containerName(spec.UUID)) || !next.exists(containerName(spec.UUID)) {
		t.Fatal("the instance did not move to the new runtime")
	}
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("a runtime switch was reported as a failure: %+v", failed)
	}
}
