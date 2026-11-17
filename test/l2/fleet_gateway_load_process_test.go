//go:build multiproc

package l2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/fleet"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// TestFleetGatewayLoadAcrossProcesses measures the /v1 data plane at fleet
// scale: the shipped server binary in one process, a fleet of real agents in a
// second one, and an OpenAI client population in this one. Every request walks
// the product's whole path - API-key check, model extraction, route table,
// least-request pick, yamux tunnel to the agent, the agent's local dial, and a
// streamed response relayed delta by delta - against engines that answer with
// a fixed first-token delay and one delta per token. What the gateway adds on
// top of those delays, and what the server process spends to add it, is the
// measurement.
//
// Default shape: 50 nodes x 8 GPUs, one deployment of 8 replicas per node, so
// 400 serving instances, 2000 concurrent streaming clients for 60s. Run it on
// an idle host:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -tags=multiproc -timeout 60m ./test/l2/ -run '^TestFleetGatewayLoadAcrossProcesses$'
//
// Every knob is an environment variable: GPUWORKS_GATEWAY_NODES (50) x
// GPUWORKS_GATEWAY_GPUS_PER_NODE (8) instances, GPUWORKS_GATEWAY_CONCURRENCY
// (2000) clients for GPUWORKS_GATEWAY_SECONDS (60); engine shape
// GPUWORKS_GATEWAY_TTFT_MS (200), _TPOT_MS (20), _DELTAS (64);
// GPUWORKS_GATEWAY_PROMPT_BYTES pads each prompt (long-context requests);
// GPUWORKS_GATEWAY_TRACE_EXPORT=1 runs with the request trace export on;
// GPUWORKS_GATEWAY_MAX_SERVER_CPU_US_PER_REQUEST budgets the server's CPU per
// request (the nightly job sets it for its runner: the figure depends on the
// host's CPU); GPUWORKS_GATEWAY_PRIORITY_CLASS=high binds the clients' key to that request
// priority class, so every request has its engine priority written into the
// body (the deployments run priority scheduling by default).
func TestFleetGatewayLoadAcrossProcesses(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("gateway load case; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	nodeCount := fleet.EnvInt("GPUWORKS_GATEWAY_NODES", 50)
	gpus := fleet.EnvInt("GPUWORKS_GATEWAY_GPUS_PER_NODE", 8)
	concurrency := fleet.EnvInt("GPUWORKS_GATEWAY_CONCURRENCY", 2000)
	duration := time.Duration(fleet.EnvInt("GPUWORKS_GATEWAY_SECONDS", 60)) * time.Second
	rampUp := time.Duration(fleet.EnvInt("GPUWORKS_GATEWAY_RAMP_SECONDS", 5)) * time.Second
	promptBytes := fleet.EnvInt("GPUWORKS_GATEWAY_PROMPT_BYTES", 0)
	priorityClass := os.Getenv("GPUWORKS_GATEWAY_PRIORITY_CLASS")
	ttft := time.Duration(fleet.EnvInt("GPUWORKS_GATEWAY_TTFT_MS", 200)) * time.Millisecond
	tpot := time.Duration(fleet.EnvInt("GPUWORKS_GATEWAY_TPOT_MS", 20)) * time.Millisecond
	deltas := fleet.EnvInt("GPUWORKS_GATEWAY_DELTAS", 64)
	maxErrorPermille := fleet.EnvInt("GPUWORKS_GATEWAY_MAX_ERROR_PERMILLE", 1)
	ttftOverheadBudget := time.Duration(fleet.EnvInt("GPUWORKS_GATEWAY_TTFT_OVERHEAD_P99_MS", 1000)) * time.Millisecond
	maxImbalancePercent := fleet.EnvInt("GPUWORKS_GATEWAY_MAX_IMBALANCE_PERCENT", 25)
	// The server's CPU per request, in microseconds; 0 checks nothing. A
	// figure for one host only: set it where the case runs on fixed hardware.
	maxServerCPUPerRequest := fleet.EnvInt("GPUWORKS_GATEWAY_MAX_SERVER_CPU_US_PER_REQUEST", 0)
	if gpus > 32 {
		t.Fatalf("GPUWORKS_GATEWAY_GPUS_PER_NODE=%d: one deployment per node cannot exceed 32 replicas", gpus)
	}
	instances := nodeCount * gpus
	// Per instance: the agent's tunnel stream and local engine socket, plus
	// the fleet's sessions; per client: its connection on both ends.
	requireFleetFileDescriptors(t, 4*instances+6*concurrency+6*nodeCount+2048)

	serverBin := fleetBinary(t, "GPUWORKS_FLEET_SERVER_BIN", "./cmd/gpuworks")
	hostBin := fleetBinary(t, "GPUWORKS_FLEET_HOST_BIN", "./test/l2/cmd/fleet-host")
	workRoot := fleetWorkRoot(t)
	dataDir := workRoot + "/server-data"
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverArgs := []string{"serve", "--addr", "127.0.0.1:" + port, "--data", dataDir}
	if os.Getenv("GPUWORKS_GATEWAY_TRACE_EXPORT") == "1" {
		serverArgs = append(serverArgs, "--config", loopbackOutboundConfig(t))
	}
	server := serverCommand(t, fleetAdminPassword, serverBin, serverArgs...)
	server.Stdout, server.Stderr = fleetProcessLog(t, workRoot, "server.log")
	if err := server.Start(); err != nil {
		t.Fatalf("start the control plane: %v", err)
	}
	t.Cleanup(func() { stopProcess(server) })
	client := fleetLogin(t, base)

	start := time.Now()
	roster := make([]fleetRosterEntry, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("gw-node-%03d", i)
		identity, err := client.CreateNode(name)
		if err != nil {
			t.Fatalf("enrol %s: %v", name, err)
		}
		roster = append(roster, fleetRosterEntry{Name: name, UUID: identity.UUID, Token: identity.Token})
	}
	rosterPath := workRoot + "/roster.json"
	encoded, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rosterPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	modelDir := workRoot + "/preloaded-model"
	readyPath := workRoot + "/fleet.ready"
	host := exec.Command(hostBin,
		"--roster", rosterPath, "--server", base,
		"--state-root", workRoot+"/agents",
		"--model-dir", modelDir, "--ready", readyPath,
		"--gpus", strconv.Itoa(gpus),
		"--engine-ttft", ttft.String(), "--engine-tpot", tpot.String(),
		"--engine-chunks", strconv.Itoa(deltas),
		"--log", workRoot+"/fleet-agent.log")
	host.Env = os.Environ()
	host.Stdout, host.Stderr = fleetProcessLog(t, workRoot, "fleet-host.log")
	if err := host.Start(); err != nil {
		t.Fatalf("start the fleet: %v", err)
	}
	t.Cleanup(func() { stopProcess(host) })
	waitForFile(t, readyPath, time.Minute)
	if _, err := fleet.WaitFleetOnline(client, nodeCount, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d nodes x %d GPUs online in %s", nodeCount, gpus, time.Since(start).Round(time.Millisecond))

	modelID, err := client.RegisterLocalModel(roster[0].UUID, modelDir, "qwen3-gateway-fake")
	if err != nil {
		t.Fatalf("register the preloaded model: %v", err)
	}
	models := make([]string, 0, nodeCount)
	start = time.Now()
	for i, node := range roster {
		if i > 0 {
			if err := client.RegisterModelPath(modelID, node.UUID, modelDir); err != nil {
				t.Fatalf("register the model on %s: %v", node.Name, err)
			}
		}
		served := fmt.Sprintf("gw-model-%03d", i)
		body, _ := json.Marshal(map[string]any{
			"model_id": modelID, "node_uuid": node.UUID, "served_name": served,
			"replicas": gpus, "quick_params": map[string]any{"max_model_len": 512},
		})
		if err := client.PostJSON("/api/v1/deployments", body, nil); err != nil {
			t.Fatalf("deploy %s on %s: %v", served, node.Name, err)
		}
		models = append(models, served)
	}
	waitGatewayInstancesRunning(t, client, instances, 15*time.Minute)
	t.Logf("%d deployments, %d instances running in %s", nodeCount, instances, time.Since(start).Round(time.Millisecond))

	var key struct {
		Key string `json:"key"`
	}
	keyRequest := []byte(`{"name":"gateway-load"}`)
	if priorityClass != "" {
		keyRequest = fmt.Appendf(nil, `{"name":"gateway-load","priority":{"class":%q,"policy":"fixed"}}`, priorityClass)
	}
	if err := client.PostJSON("/api/v1/apikeys", keyRequest, &key); err != nil || key.Key == "" {
		t.Fatalf("create an API key: %v", err)
	}
	if priorityClass != "" {
		requireGatewayPriority(t, client, base, key.Key, models[0], priorityClass, len(models))
	}
	// GPUWORKS_GATEWAY_TRACE_EXPORT=1 turns the request trace export on
	// (ADR 0046), every trace sampled, to a collector in this process that
	// counts the spans: the run then measures the gateway with export on.
	var traces *otlpCollector
	if os.Getenv("GPUWORKS_GATEWAY_TRACE_EXPORT") == "1" {
		traces = newOTLPCollector(t)
		token := loginWithPassword(t, base, fleetAdminPassword)
		resp, data := authedDo(t, http.MethodPut, base, token, "/api/v1/telemetry/export",
			[]byte(fmt.Sprintf(`{"enabled":true,"endpoint":%q,"sample_ratio":1}`, traces.server.URL)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn the trace export on: %d %s", resp.StatusCode, data)
		}
	}
	// One request per model first: every deployment is proven routable before
	// the clock starts, so a slow route-table warm-up is not read as load.
	warm := fleet.GatewayLoad{Base: base, APIKey: key.Key, Models: models}.Warm(context.Background())
	if warm.Errors > 0 || warm.E2E.Count() != len(models) {
		t.Fatalf("warm-up: %d of %d models answered: %v", warm.E2E.Count(), len(models), warm.Failures())
	}

	serverBefore, hostBefore := readProcessCPU(server.Process.Pid), readProcessCPU(host.Process.Pid)
	overflowsBefore := readListenOverflows()
	peak := &peakRSS{}
	stopSampling := peak.sample(server.Process.Pid, time.Second)
	t.Logf("driving %d streaming clients (ramped over %s, %d-byte prompts) for %s across %d deployments", concurrency, rampUp, promptBytes, duration, len(models))
	report := fleet.GatewayLoad{Base: base, APIKey: key.Key, Models: models, Concurrency: concurrency, Duration: duration, RampUp: rampUp, PromptBytes: promptBytes}.Run(context.Background())
	stopSampling()
	serverCPU := readProcessCPU(server.Process.Pid) - serverBefore
	hostCPU := readProcessCPU(host.Process.Pid) - hostBefore
	if overflowsBefore >= 0 {
		t.Logf("host TCP listen overflows during the run: %d", readListenOverflows()-overflowsBefore)
	}

	for _, line := range report.Lines(ttft, tpot, deltas) {
		t.Log(line)
	}
	seconds := report.Elapsed.Seconds()
	t.Logf("server process: %.2f cores busy (%.1f CPU-s), RSS peak %.0f MiB, now %.0f MiB",
		serverCPU/seconds, serverCPU, peak.max(), readProcessRSSMiB(server.Process.Pid))
	perRequest := serverCPU * 1e6 / float64(max(report.Requests, 1))
	t.Logf("server CPU per request: %.0f µs", perRequest)
	t.Logf("fleet process (agents + engines): %.2f cores busy (%.1f CPU-s)", hostCPU/seconds, hostCPU)
	t.Logf("request bodies spooled to disk at once (peak): %d", peak.spooled())

	// The proxy flushes its routing counters every 10s.
	time.Sleep(12 * time.Second)
	if traces != nil {
		var export struct {
			Status struct {
				Exported, Dropped, Failed uint64
			} `json:"status"`
		}
		if err := client.GetJSON("/api/v1/telemetry/export", &export); err != nil {
			t.Errorf("read the trace export status: %v", err)
		}
		t.Logf("trace export: %d spans at the collector; exporter exported %d, dropped %d, failed %d (requests %d)",
			traces.count(), export.Status.Exported, export.Status.Dropped, export.Status.Failed, report.Requests)
		if export.Status.Dropped+export.Status.Failed > uint64(report.Requests)/1000 {
			t.Errorf("the trace export lost %d of %d spans", export.Status.Dropped+export.Status.Failed, report.Requests)
		}
	}
	if retries, err := fleet.SumMetric(client, "gpuworks_proxy_retries_total", 30*time.Minute); err == nil {
		t.Logf("gateway retries (connection-level, before any response): %.0f", retries)
	}
	spreads, err := fleet.RoutedSpread(client, 30*time.Minute)
	if err != nil {
		t.Errorf("read routing counters: %v", err)
	} else {
		if len(spreads) != len(models) {
			t.Errorf("routing counters cover %d deployments, want %d", len(spreads), len(models))
		}
		for i, spread := range spreads {
			if i < 3 {
				t.Logf("replica spread, deployment %s: %d replicas, min=%.0f mean=%.0f max=%.0f (%.1f%% off the mean)",
					spread.Deployment, spread.Replicas, spread.Min, spread.Mean, spread.Max, 100*spread.Imbalance())
			}
			if spread.Replicas != gpus {
				t.Errorf("deployment %s routed to %d replicas, want all %d", spread.Deployment, spread.Replicas, gpus)
			}
			if 100*spread.Imbalance() > float64(maxImbalancePercent) {
				t.Errorf("deployment %s replica spread is %.1f%% off the mean (budget %d%%)",
					spread.Deployment, 100*spread.Imbalance(), maxImbalancePercent)
			}
		}
	}

	if report.E2E.Count() == 0 {
		t.Fatal("no request completed")
	}
	if permille := report.Errors * 1000 / max(report.Requests, 1); permille > int64(maxErrorPermille) {
		t.Errorf("%d of %d requests failed (%d‰, budget %d‰): %v", report.Errors, report.Requests, permille, maxErrorPermille, report.Failures())
	}
	if maxServerCPUPerRequest > 0 && perRequest > float64(maxServerCPUPerRequest) {
		t.Errorf("server CPU per request is %.0f µs (budget %d µs)", perRequest, maxServerCPUPerRequest)
	}
	if overhead := report.TTFT.Percentile(0.99) - ttft; overhead > ttftOverheadBudget {
		t.Errorf("gateway adds %s to TTFT at p99 (budget %s)", overhead.Round(time.Millisecond), ttftOverheadBudget)
	}
}

// requireGatewayPriority fails the run unless it measures what it says: every
// deployment runs priority scheduling, and the gateway answers a request on
// the key at the class it is bound to.
func requireGatewayPriority(t *testing.T, client *fleet.Client, base, key, model, class string, deployments int) {
	t.Helper()
	var rows apiwire.List[struct {
		PriorityScheduling bool `json:"priority_scheduling"`
	}]
	if err := client.GetJSON("/api/v1/deployments", &rows); err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	on := 0
	for _, row := range rows.Items {
		if row.PriorityScheduling {
			on++
		}
	}
	if on != deployments {
		t.Fatalf("%d of %d deployments run priority scheduling", on, deployments)
	}
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, model)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe the class: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get("X-GPUWorks-Priority-Class"); resp.StatusCode != http.StatusOK || got != class {
		t.Fatalf("probe answered %d at class %q, want %q", resp.StatusCode, got, class)
	}
	t.Logf("all %d deployments run priority scheduling; the key's requests run at class %q", deployments, class)
}

// waitGatewayInstancesRunning polls the instance list until want instances
// report running.
func waitGatewayInstancesRunning(t *testing.T, client *fleet.Client, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	running := 0
	for time.Now().Before(deadline) {
		var rows apiwire.List[struct {
			State string `json:"state"`
		}]
		if err := client.GetJSON("/api/v1/instances", &rows); err == nil {
			running = 0
			for _, row := range rows.Items {
				if row.State == "running" {
					running++
				}
			}
			if running >= want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("only %d of %d instances reached running within %s", running, want, timeout)
}

// peakRSS samples a process's resident set while the load runs; the end
// value alone hides a transient spike.
type peakRSS struct {
	mu    sync.Mutex
	peak  float64
	spool int
}

func (p *peakRSS) sample(pid int, every time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			rss := readProcessRSSMiB(pid)
			// Request bodies past the proxy's in-memory replay budget are
			// spooled to temp files; how many exist at once shows whether
			// the budget holds the offered load.
			spooled, _ := filepath.Glob(filepath.Join(os.TempDir(), "gpuworks-proxy-body-*"))
			p.mu.Lock()
			p.peak = max(p.peak, rss)
			p.spool = max(p.spool, len(spooled))
			p.mu.Unlock()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { close(stop); <-done }
}

func (p *peakRSS) spooled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spool
}

func (p *peakRSS) max() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}
