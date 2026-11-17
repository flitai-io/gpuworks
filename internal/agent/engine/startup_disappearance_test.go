package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type startupProbeDriver struct {
	*livenessDriver
	afterStatus func(runtime.Status, error)
}

func (d *startupProbeDriver) Status(ctx context.Context, id string) (runtime.Status, error) {
	st, err := d.livenessDriver.Status(ctx, id)
	if d.afterStatus != nil {
		d.afterStatus(st, err)
	}
	return st, err
}

// go test ./internal/agent/engine -run '^TestStartup(ContainerDisappearance|InspectionErrorRecovers|CanceledInspection)$' -count=1
func TestStartupContainerDisappearance(t *testing.T) {
	for mode, headless := range map[string]bool{"http": false, "headless": true} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cleanup-fails=%t", mode, cleanupFails), func(t *testing.T) {
				driver := &startupProbeDriver{livenessDriver: newLivenessDriver()}
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer server.Close()
				e, log := newLivenessEngine(t, driver)
				e.headlessStableChecks = 3
				spec := runningSpec("startup-disappears", (&sleepHTTPServer{server: server}).port(t))
				spec.Headless = headless
				name := containerName(spec.UUID)
				if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
					t.Fatal(err)
				}
				e.desired[spec.UUID], e.runtimeEpoch[spec.UUID] = spec, 1
				preStartDir := e.preStartDir(spec.UUID)
				if err := os.MkdirAll(preStartDir, 0755); err != nil {
					t.Fatal(err)
				}
				observedRunning, observedMissing := false, false
				driver.afterStatus = func(st runtime.Status, err error) {
					if err != nil {
						return
					}
					if st.Exists && st.Running {
						observedRunning = true
						driver.deleteContainer(name)
					} else if !st.Exists {
						observedMissing = true
						driver.afterStatus = nil
						if cleanupFails {
							driver.set(func(d *livenessDriver) { d.statusErrs = 1 })
						}
					}
				}
				// This deadline bounds the test only; startup has a longer budget.
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				e.probeAndWatch(ctx, spec, name, 1)
				if !observedRunning || !observedMissing {
					t.Fatalf("did not observe running -> absent: running=%t absent=%t", observedRunning, observedMissing)
				}
				wantState, wantMessage := proto.InstFailed, "container disappeared during startup"
				if headless {
					wantMessage = "headless worker disappeared during startup"
				}
				if cleanupFails {
					wantState = proto.InstRemoving
				}
				reports := log.snapshot()
				if len(reports) != 1 || reports[0].State != wantState || !strings.HasPrefix(reports[0].Message, wantMessage) {
					t.Fatalf("reports = %+v, want one %s report for %q before startup times out", reports, wantState, wantMessage)
				}
				if f := reports[0].RuntimeFailure; f == nil || f.Code != proto.ErrorContainerDisappeared || f.Phase != proto.PhaseHealthCheck {
					t.Fatalf("runtime failure = %+v", f)
				}
				if ctx.Err() != nil {
					t.Fatal("disappearance was not reported before the test deadline")
				}
				_, err := os.Stat(preStartDir)
				if cleanupFails {
					if err != nil || !strings.Contains(reports[0].Message, "cleanup pending") {
						t.Fatalf("failed cleanup lost resources or its reason: stat=%v report=%+v", err, reports[0])
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("startup resources were not reclaimed: %v", err)
				}
				wantRequests := int32(1)
				if headless {
					wantRequests = 0
				}
				if requests.Load() != wantRequests {
					t.Fatalf("HTTP probes = %d, want %d; an absent container must not reach HTTP readiness", requests.Load(), wantRequests)
				}
				if len(e.servingEpoch) != 0 {
					t.Fatal("failed startup was handed to the liveness monitor")
				}
				e.checkLiveness(t.Context())
				if len(log.snapshot()) != 1 {
					t.Fatalf("liveness repeated the startup failure: %+v", log.snapshot())
				}
			})
		}
	}
}

func TestStartupInspectionErrorRecovers(t *testing.T) {
	for mode, headless := range map[string]bool{"http": false, "headless": true} {
		t.Run(mode, func(t *testing.T) {
			driver := newLivenessDriver()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			e, log := newLivenessEngine(t, driver)
			e.headlessStableChecks = 3
			spec := runningSpec("startup-inspect-retry", (&sleepHTTPServer{server: server}).port(t))
			spec.Headless = headless
			name := containerName(spec.UUID)
			if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
				t.Fatal(err)
			}
			driver.set(func(d *livenessDriver) { d.statusErrs = 1 })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if !e.probeAndReport(ctx, spec, name) {
				t.Fatalf("transient inspect failure prevented readiness: %+v", log.snapshot())
			}
			reports := log.snapshot()
			if len(reports) != 1 || reports[0].State != proto.InstRunning || !driver.exists(name) {
				t.Fatalf("healthy container was retired after a transient error: %+v", reports)
			}
		})
	}
}

func TestStartupCanceledInspection(t *testing.T) {
	for mode, headless := range map[string]bool{"http": false, "headless": true} {
		for _, state := range []string{"missing", "exited", "running"} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				driver := &startupProbeDriver{livenessDriver: newLivenessDriver()}
				e, log := newLivenessEngine(t, driver)
				spec := runningSpec("startup-canceled", healthyEngine(t))
				spec.Headless = headless
				name := containerName(spec.UUID)
				if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "missing":
					driver.deleteContainer(name)
				case "exited":
					driver.exit(name, 17, false)
				}
				preStartDir := e.preStartDir(spec.UUID)
				if err := os.MkdirAll(preStartDir, 0755); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				driver.afterStatus = func(runtime.Status, error) { cancel() }
				if e.probeAndReport(ctx, spec, name) {
					t.Fatal("canceled startup reported ready")
				}
				if reports := log.snapshot(); len(reports) != 0 {
					t.Fatalf("canceled probe published a startup outcome: %+v", reports)
				}
				if _, err := os.Stat(preStartDir); err != nil {
					t.Fatalf("canceled probe took over lifecycle cleanup: %v", err)
				}
			})
		}
	}
}
