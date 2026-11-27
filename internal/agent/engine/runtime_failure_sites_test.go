package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// failedReport is the one failed report among statuses.
func failedReport(t *testing.T, statuses []proto.InstanceStatus) proto.InstanceStatus {
	t.Helper()
	var failed []proto.InstanceStatus
	for _, st := range statuses {
		if st.State == proto.InstFailed {
			failed = append(failed, st)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("failed reports = %+v, want one", statuses)
	}
	if failed[0].RuntimeFailure == nil {
		t.Fatalf("failed report without a reason: %+v", failed[0])
	}
	return failed[0]
}

// Every failure before a container exists carries its code and the phase it
// failed in, with the error text as its detail; the message stays the
// English fallback.
func TestLaunchFailuresBeforeTheContainerCarryTheirCode(t *testing.T) {
	missingModel := filepath.Join(t.TempDir(), "absent")
	// The same absence beside a directory that still holds something is the
	// node's finding that the files are gone.
	storageRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(storageRoot, "another-model"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	goneModel := filepath.Join(storageRoot, "absent")
	for name, tc := range map[string]struct {
		driver  runtime.Driver
		spec    proto.InstanceDesiredState
		setup   func(*Engine)
		prep    func(*proto.InstanceDesiredState)
		code    string
		phase   string
		check   func(*testing.T, proto.RuntimeFailure)
		message string
	}{
		"model path": {
			driver: &imageToleranceDriver{exists: []bool{true}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1", ModelPath: missingModel},
			code:   proto.ErrorModelPathUnavailable, phase: proto.PhaseContainerPrepare,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if f.Resource != missingModel || !strings.Contains(f.Detail, "no such file") {
					t.Fatalf("model path reason = %+v", f)
				}
			},
			message: ModelPathUnavailable + ": model path " + missingModel,
		},
		"model path gone": {
			driver: &imageToleranceDriver{exists: []bool{true}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1", ModelPath: goneModel},
			code:   proto.ErrorModelPathMissing, phase: proto.PhaseContainerPrepare,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if f.Resource != goneModel || f.Detail != "" {
					t.Fatalf("model path reason = %+v", f)
				}
			},
			message: proto.ErrorModelPathMissing + ": model path " + goneModel,
		},
		"image pull": {
			driver: &imageToleranceDriver{present: map[string]bool{}, pullErr: errTest("registry down")},
			spec:   proto.InstanceDesiredState{Image: "a.example/img:1", ImagePullRefs: []string{"a.example/img:1"}},
			code:   proto.ErrorImagePullFailed, phase: proto.PhaseImagePull,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if !strings.Contains(f.Detail, "registry down") {
					t.Fatalf("pull reason = %+v", f)
				}
			},
			message: "registry down",
		},
		"runtime unavailable": {
			driver: &unavailableOnCheck{imageToleranceDriver{present: map[string]bool{}}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1"},
			code:   proto.ErrorContainerRuntimeUnavailable, phase: proto.PhaseImageVerify,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if f.Runtime != "headless-test" || !strings.Contains(f.Detail, "chown") {
					t.Fatalf("runtime reason = %+v", f)
				}
			},
			message: "udocker runtime unavailable: ",
		},
		"runtime asset": {
			driver: &imageToleranceDriver{exists: []bool{true}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1"},
			prep: func(spec *proto.InstanceDesiredState) {
				spec.RuntimeAssets = []proto.RuntimeAsset{{File: "x.bin", SHA256: strings.Repeat("0", 64), UnpackTo: "/opt/x"}}
			},
			code: proto.ErrorRuntimeAssetPrepareFailed, phase: proto.PhaseAssetPrepare,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if f.Detail == "" {
					t.Fatalf("asset reason = %+v", f)
				}
			},
			message: "runtime asset preparation failed: ",
		},
		"run": {
			driver: &runFailDriver{imageToleranceDriver: imageToleranceDriver{exists: []bool{true}}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1"},
			code:   proto.ErrorContainerRunFailed, phase: proto.PhaseContainerLaunch,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if !strings.Contains(f.Detail, "OCI runtime create failed") {
					t.Fatalf("run reason = %+v", f)
				}
			},
			message: "run failed: Error response from daemon: OCI runtime create failed",
		},
		"feature unsupported": {
			driver: &imageToleranceDriver{exists: []bool{true}},
			spec:   proto.InstanceDesiredState{Image: "repo:v1", GPUIndexes: []int{0}, Sleepable: true},
			prep: func(spec *proto.InstanceDesiredState) {
				spec.RequestedFeatures = &proto.RequestedFeatures{EngineSleep: true, CommandKnown: true}
			},
			setup: func(e *Engine) {
				e.prober = &gpu.KunlunProber{}
			},
			code: proto.ErrorAcceleratorFeatureUnsupported, phase: proto.PhaseContainerPrepare,
			check: func(t *testing.T, f proto.RuntimeFailure) {
				if f.Vendor != "kunlunxin" || f.Feature != "engine_sleep" {
					t.Fatalf("feature reason = %+v", f)
				}
			},
			message: "does not support requested accelerator features: engine_sleep",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSleepHTTPServer(t)
			var statuses []proto.InstanceStatus
			e := NewWithProber(tc.driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir(), injectionProber{})
			if tc.setup != nil {
				tc.setup(e)
			}
			spec := tc.spec
			if tc.prep != nil {
				tc.prep(&spec)
			}
			spec.UUID, spec.APIPort, spec.Ports, spec.StartTimeout = "launch-"+strings.ReplaceAll(name, " ", "-"), h.port(t), []int{h.port(t)}, 1
			e.startOrAdopt(t.Context(), spec)
			st := failedReport(t, statuses)
			f := *st.RuntimeFailure
			if f.Code != tc.code || f.Phase != tc.phase || f.ExitCode != nil {
				t.Fatalf("reason = %+v, want %s in %s", f, tc.code, tc.phase)
			}
			if !strings.Contains(st.Message, tc.message) {
				t.Fatalf("message = %q, want it to contain %q", st.Message, tc.message)
			}
			if st.DirectAddr != "" || st.ContainerID != "" {
				t.Fatalf("a launch failure invented runtime state: %+v", st)
			}
			tc.check(t, f)
		})
	}
}

type runFailDriver struct{ imageToleranceDriver }

func (d *runFailDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	return "", errors.New("Error response from daemon: OCI runtime create failed: unable to start container process")
}

// A card the instance is bound to that is gone fails the launch with the
// card's identity.
func TestLaunchOnAMissingCardCarriesItsIdentity(t *testing.T) {
	var statuses []proto.InstanceStatus
	prober := identityProber{gpus: []proto.GPUInfo{{Index: 0, UUID: "GPU-other"}}}
	e := NewWithProber(&imageToleranceDriver{exists: []bool{true}}, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir(), prober)
	e.startOrAdopt(t.Context(), proto.InstanceDesiredState{UUID: "card-gone", Image: "repo:v1", GPUIndexes: []int{0}, GPUIdents: []string{"GPU-gone"}})
	f := failedReport(t, statuses).RuntimeFailure
	if f.Code != proto.ErrorAcceleratorCardMissing || f.Resource != "GPU-gone" || f.Phase != proto.PhaseContainerPrepare {
		t.Fatalf("reason = %+v", f)
	}
}

type identityProber struct {
	injectionProber
	gpus []proto.GPUInfo
}

func (p identityProber) Probe(context.Context) ([]proto.GPUInfo, error) { return p.gpus, nil }

// A dGPU limit that cannot be applied fails with its reason; a partition
// cleanup that also fails reports the instance removing with the same code.
func TestPartitionFailureCarriesItsCode(t *testing.T) {
	var statuses []proto.InstanceStatus
	e := NewWithProber(&imageToleranceDriver{exists: []bool{true}}, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(t.Context(), proto.InstanceDesiredState{UUID: "partition-bad", Image: "repo:v1", GPUIndexes: []int{0}, VRAMLimitBytes: 1 << 30, PartitionBackend: "no-such-backend"})
	st := failedReport(t, statuses)
	if f := st.RuntimeFailure; f.Code != proto.ErrorDGPUPartitionFailed || !strings.Contains(f.Detail, "no-such-backend") || st.Message != f.Detail {
		t.Fatalf("partition reason = %+v, message %q", f, st.Message)
	}
}

// A headless worker that the runtime reports unhealthy, or that never stays
// up long enough, fails with its own code; what it printed goes with it.
func TestHeadlessWorkerFailuresCarryTheirCode(t *testing.T) {
	for name, tc := range map[string]struct {
		status runtime.Status
		stable int
		code   string
	}{
		"unhealthy": {status: runtime.Status{Exists: true, Running: true, Health: "unhealthy"}, stable: 3, code: proto.ErrorContainerUnhealthy},
		"timeout":   {status: runtime.Status{Exists: true, Running: true}, stable: 1 << 30, code: proto.ErrorWorkerReadinessTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: tc.status}, logs: "NCCL WARN socket timeout\n"}
			var statuses []proto.InstanceStatus
			e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
			e.headlessProbeInterval, e.headlessStableChecks = time.Millisecond, tc.stable
			e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "worker-" + name, Headless: true, NodeRank: 1, StartTimeout: 1}, "worker")
			f := failedReport(t, statuses).RuntimeFailure
			if f.Code != tc.code || f.Phase != proto.PhaseHealthCheck || f.ExitCode != nil {
				t.Fatalf("reason = %+v, want %s", f, tc.code)
			}
			if len(f.Causes) != 1 || f.Causes[0].Code != enginelog.CodeNCCLFailure {
				t.Fatalf("causes = %+v, want the output's NCCL failure", f.Causes)
			}
		})
	}
	// A worker that is still running and silent did not exit without
	// output: its timeout has no cause.
	driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}}
	var statuses []proto.InstanceStatus
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
	e.headlessProbeInterval, e.headlessStableChecks = time.Millisecond, 1<<30
	e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "worker-silent", Headless: true, NodeRank: 1, StartTimeout: 1}, "worker")
	if f := failedReport(t, statuses).RuntimeFailure; f.Code != proto.ErrorWorkerReadinessTimeout || len(f.Causes) != 0 {
		t.Fatalf("silent timeout reason = %+v", f)
	}
}

type loggingHeadlessDriver struct {
	*headlessTestDriver
	logs    string
	logsErr error
}

func (d *loggingHeadlessDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	if d.logsErr != nil {
		return nil, d.logsErr
	}
	return io.NopCloser(strings.NewReader(d.logs)), nil
}

// An exited container whose output cannot be read still fails with its exit
// code; it has no causes or evidence, and its message is the bare exit.
func TestExitWithUnreadableOutputStillCarriesTheExitCode(t *testing.T) {
	driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: runtime.Status{Exists: true, ExitCode: 3}}, logsErr: errors.New("Error response from daemon: configured logging driver does not support reading")}
	var statuses []proto.InstanceStatus
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "worker-unreadable", Headless: true, NodeRank: 1, StartTimeout: 1}, "worker")
	st := failedReport(t, statuses)
	if f := st.RuntimeFailure; f.Code != proto.ErrorContainerExited || f.ExitCode == nil || *f.ExitCode != 3 || f.Causes != nil || f.Evidence != nil {
		t.Fatalf("reason = %+v", f)
	}
	if st.Message != "headless worker exited (code 3)" {
		t.Fatalf("message = %q", st.Message)
	}
}

// A launch script that refuses its arguments prints a plain sentence and
// exits non-zero: no line names an error or exception, so the reason is what
// the container said last (GWKS-342). Both streams reach Logs interleaved,
// as docker and nerdctl deliver them.
func TestStartupExitWithoutAnErrorLineReportsItsLastOutput(t *testing.T) {
	long := strings.Repeat("x", 2*proto.MaxFailureEvidenceChars)
	cases := map[string]struct {
		logs         string
		wantMessage  string
		wantEvidence []string
	}{
		"one stderr line": {
			logs:         "This recipe requires TP16 across two nodes, with eight NPUs per node.\n",
			wantMessage:  "headless worker exited (code 1): This recipe requires TP16 across two nodes, with eight NPUs per node.",
			wantEvidence: []string{"This recipe requires TP16 across two nodes, with eight NPUs per node."},
		},
		"stdout banner then stderr reason": {
			logs:         "+ set -e\n\x1b[32mstarting worker rank 1\x1b[0m\n\nchecking topology\nloading recipe\nThis recipe requires TP16 across two nodes, with eight NPUs per node.\n\n",
			wantMessage:  "headless worker exited (code 1): This recipe requires TP16 across two nodes, with eight NPUs per node.",
			wantEvidence: []string{"checking topology", "loading recipe", "This recipe requires TP16 across two nodes, with eight NPUs per node."},
		},
		"an error line still wins over the tail": {
			logs:         "ValueError: bad tensor parallel size\ncleaning up\nbye\n",
			wantMessage:  "headless worker exited (code 1): ValueError: bad tensor parallel size",
			wantEvidence: []string{"ValueError: bad tensor parallel size"},
		},
		"a long last line is bounded": {
			logs:         long + "\n",
			wantMessage:  "headless worker exited (code 1): " + strings.Repeat("x", 300) + "…",
			wantEvidence: nil, // checked below: one line within the wire bound
		},
		"a traced launch script's credentials are not repeated": {
			logs:         "+ export HF_TOKEN=hf_abcdefSECRET\n+ curl -fsS -H 'Authorization: Bearer eyJhbGciSECRET' http://registry.local/recipe\n+ vllm serve /model --api-key sk-liveSECRET --port 8000\n",
			wantMessage:  "headless worker exited (code 1): + vllm serve /model --api-key <redacted> --port 8000",
			wantEvidence: []string{"+ export HF_TOKEN=<redacted>", "+ curl -fsS -H 'Authorization: Bearer <redacted>' http://registry.local/recipe", "+ vllm serve /model --api-key <redacted> --port 8000"},
		},
		"a progress bar redrawn in place gives its newest state": {
			logs:         "Loading weights:  10%|#\rLoading weights:  20%|##\rLoading weights:  30%|###\n",
			wantMessage:  "headless worker exited (code 1): Loading weights:  30%|###",
			wantEvidence: []string{"Loading weights:  10%|#", "Loading weights:  20%|##", "Loading weights:  30%|###"},
		},
		"output longer than what is kept is read to its end": {
			logs:         strings.Repeat(strings.Repeat("w", 4096)+"\n", 200) + "This recipe requires TP16 across two nodes, with eight NPUs per node.\n",
			wantMessage:  "headless worker exited (code 1): This recipe requires TP16 across two nodes, with eight NPUs per node.",
			wantEvidence: nil, // checked below: the last line is the last evidence
		},
		"no output": {
			logs:         "",
			wantMessage:  "headless worker exited (code 1) with no output",
			wantEvidence: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: runtime.Status{Exists: true, ExitCode: 1}}, logs: tc.logs}
			var statuses []proto.InstanceStatus
			e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
			e.headlessProbeInterval = time.Millisecond
			e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "worker-plain", Headless: true, NodeRank: 1, StartTimeout: 1}, "worker")
			st := failedReport(t, statuses)
			f := st.RuntimeFailure
			if f.Code != proto.ErrorContainerExited || f.Phase != proto.PhaseHealthCheck || f.ExitCode == nil || *f.ExitCode != 1 {
				t.Fatalf("reason = %+v", f)
			}
			if st.Message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", st.Message, tc.wantMessage)
			}
			if st.Phase != "" {
				t.Fatalf("failed report keeps progress phase %q", st.Phase)
			}
			if name == "a long last line is bounded" {
				if len(f.Evidence) != 1 || len(f.Evidence[0]) > proto.MaxFailureEvidenceChars {
					t.Fatalf("evidence = %d lines, first %d bytes", len(f.Evidence), len(f.Evidence[0]))
				}
				return
			}
			if name == "output longer than what is kept is read to its end" {
				if len(f.Evidence) != proto.MaxFailureEvidence || f.Evidence[len(f.Evidence)-1] != "This recipe requires TP16 across two nodes, with eight NPUs per node." {
					t.Fatalf("evidence = %.80q", f.Evidence)
				}
				return
			}
			if !slices.Equal(f.Evidence, tc.wantEvidence) {
				t.Fatalf("evidence = %q, want %q", f.Evidence, tc.wantEvidence)
			}
			for _, text := range append([]string{st.Message}, f.Evidence...) {
				if strings.Contains(text, "SECRET") {
					t.Fatalf("a credential is repeated in %q", text)
				}
			}
		})
	}
}

// An engine that stops while serving, a worker killed for memory and a
// container that is still running end on ordinary lines: they are not the
// reason, so none of them is reported as one.
func TestOrdinaryLastLinesAreNotAReasonOutsideAStartupExit(t *testing.T) {
	const logs = "INFO:     127.0.0.1:53210 - \"POST /v1/chat/completions HTTP/1.1\" 200 OK\n"
	e := newHermeticEngine(&loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{}, logs: logs}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())

	serving, msg := e.exitedFailure(t.Context(), "c", runtime.Status{Exists: true, ExitCode: 9}, 0, proto.PhaseServing, "oom", "container exited while running (code 9)")
	if serving.Evidence != nil || msg != "container exited while running (code 9)" {
		t.Fatalf("serving exit: evidence %q, message %q", serving.Evidence, msg)
	}
	oom, msg := e.exitedFailure(t.Context(), "c", runtime.Status{Exists: true, ExitCode: 137, OOMKilled: true}, 0, proto.PhaseHealthCheck, "headless worker OOM-killed", "headless worker exited (code 137)")
	if oom.Code != proto.ErrorContainerOOMKilled || oom.Evidence != nil || msg != "headless worker OOM-killed" {
		t.Fatalf("oom exit: %+v, message %q", oom, msg)
	}
	unhealthy := proto.RuntimeFailure{Code: proto.ErrorContainerUnhealthy, Phase: proto.PhaseHealthCheck}
	e.attachOutput(t.Context(), "c", 0, &unhealthy, false)
	if unhealthy.Evidence != nil {
		t.Fatalf("running container: evidence %q", unhealthy.Evidence)
	}
}

// A health endpoint that answers for another model is a port taken by
// another engine after the container started.
func TestForeignEngineOnThePortCarriesItsCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"someone-else"}]}`))
		}
	}))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("foreign-port", serverPort(t, server))
	spec.ServedModelName = "mine"
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	e.probeAndReport(t.Context(), spec, name)
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 {
		t.Fatalf("reports = %+v", log.snapshot())
	}
	if f := failed[0].RuntimeFailure; f == nil || f.Code != proto.ErrorEnginePortInUse || f.Port != spec.APIPort || f.Detail != "someone-else" || f.Phase != proto.PhaseHealthCheck {
		t.Fatalf("reason = %+v", f)
	}
}

// The only failed report the engine package builds is reportFailed's: no
// other file of the package spells a failed InstanceStatus.
func TestOnlyReportFailedBuildsAFailedStatus(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "runtime_failure.go" {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "proto.InstFailed") {
			t.Errorf("%s spells proto.InstFailed; report failures through reportFailed", file)
		}
	}
}
