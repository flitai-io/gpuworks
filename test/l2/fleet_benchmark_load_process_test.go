//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/fleet"
)

// TestFleetServerOriginBenchmarkAcrossProcesses guards the control plane while
// it drives an evaluation itself: the shipped server binary in one process, a
// fleet of real agents with streaming fake engines in a second one, and a
// server-origin saturation run through the gateway at the concurrency and
// output length of a real capacity probe. The load generator, its progress
// samples and the stored curve share the server process with the UI and the
// monitor, so the run must finish within a memory budget while the operator
// lists stay readable. A per-completion summary over every request so far
// once saturated the server's cores and grew it to an OOM kill at a few
// thousand requests; this case reproduces that request volume.
//
// Default shape: one 32-GPU node serving a 32-replica deployment, 300 sessions
// in flight, multi-turn conversations of 1024 streamed tokens each (one delta
// per token, 3ms apart), 10 requests per unit of concurrency:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -tags=multiproc -timeout 60m ./test/l2/ -run '^TestFleetServerOriginBenchmarkAcrossProcesses$'
//
// Knobs: GPUWORKS_BENCH_REPLICAS (32), GPUWORKS_BENCH_NODES (1; replicas are
// spread over the nodes by automatic placement), GPUWORKS_BENCH_CONCURRENCY (300), GPUWORKS_BENCH_REQUESTS_PER_CONCURRENCY
// (10); engine shape GPUWORKS_BENCH_TTFT_MS (200), _TPOT_MS (3), _DELTAS
// (1024); budgets GPUWORKS_BENCH_MAX_RSS_MIB (2048) and
// GPUWORKS_BENCH_MAX_ERROR_PERMILLE (1).
func TestFleetServerOriginBenchmarkAcrossProcesses(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("server-origin benchmark load case; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	replicas := fleet.EnvInt("GPUWORKS_BENCH_REPLICAS", 32)
	nodeCount := max(fleet.EnvInt("GPUWORKS_BENCH_NODES", 1), 1)
	gpusPerNode := (replicas + nodeCount - 1) / nodeCount
	concurrency := fleet.EnvInt("GPUWORKS_BENCH_CONCURRENCY", 300)
	multiplier := fleet.EnvInt("GPUWORKS_BENCH_REQUESTS_PER_CONCURRENCY", 10)
	ttft := time.Duration(fleet.EnvInt("GPUWORKS_BENCH_TTFT_MS", 200)) * time.Millisecond
	tpot := time.Duration(fleet.EnvInt("GPUWORKS_BENCH_TPOT_MS", 3)) * time.Millisecond
	deltas := fleet.EnvInt("GPUWORKS_BENCH_DELTAS", 1024)
	maxRSS := float64(fleet.EnvInt("GPUWORKS_BENCH_MAX_RSS_MIB", 2048))
	maxErrorPermille := fleet.EnvInt("GPUWORKS_BENCH_MAX_ERROR_PERMILLE", 1)
	if replicas > planning.MaxReplicas {
		t.Fatalf("GPUWORKS_BENCH_REPLICAS=%d: one deployment cannot exceed %d replicas", replicas, planning.MaxReplicas)
	}
	requireFleetFileDescriptors(t, 4*replicas+6*concurrency+6*nodeCount+2048)

	serverBin := fleetBinary(t, "GPUWORKS_FLEET_SERVER_BIN", "./cmd/gpuworks")
	hostBin := fleetBinary(t, "GPUWORKS_FLEET_HOST_BIN", "./test/l2/cmd/fleet-host")
	workRoot := fleetWorkRoot(t)
	dataDir := workRoot + "/server-data"
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, fleetAdminPassword, serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = fleetProcessLog(t, workRoot, "server.log")
	if err := server.Start(); err != nil {
		t.Fatalf("start the control plane: %v", err)
	}
	t.Cleanup(func() { stopProcess(server) })
	client := fleetLogin(t, base)

	roster := make([]fleetRosterEntry, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("bench-node-%03d", i)
		identity, err := client.CreateNode(name)
		if err != nil {
			t.Fatalf("enrol %s: %v", name, err)
		}
		roster = append(roster, fleetRosterEntry{Name: name, UUID: identity.UUID, Token: identity.Token})
	}
	rosterPath := workRoot + "/roster.json"
	writeFleetRoster(t, rosterPath, roster)
	modelDir := workRoot + "/preloaded-model"
	readyPath := workRoot + "/fleet.ready"
	host := exec.Command(hostBin,
		"--roster", rosterPath, "--server", base,
		"--state-root", workRoot+"/agents",
		"--model-dir", modelDir, "--ready", readyPath,
		"--gpus", strconv.Itoa(gpusPerNode),
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
	if _, err := fleet.WaitFleetCards(client, nodeCount, max(gpusPerNode, 1), 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	modelID, err := client.RegisterLocalModel(roster[0].UUID, modelDir, "qwen3-bench-fake")
	if err != nil {
		t.Fatalf("register the preloaded model: %v", err)
	}
	// One node pins the deployment there; several spread it by automatic
	// placement, every node holding the preloaded model at the same path.
	request := map[string]any{
		"model_id": modelID, "node_uuid": roster[0].UUID, "served_name": "bench-model", "replicas": replicas,
		"quick_params": map[string]any{"max_model_len": 8192},
	}
	if nodeCount > 1 {
		for _, node := range roster[1:] {
			path, _ := json.Marshal(map[string]string{"node_uuid": node.UUID, "path": modelDir})
			if err := client.PostJSON(fmt.Sprintf("/api/v1/models/%d/register-path", modelID), path, nil); err != nil {
				t.Fatalf("register the preloaded model on %s: %v", node.Name, err)
			}
		}
		request["placement_mode"] = "auto"
	}
	body, _ := json.Marshal(request)
	var deployment struct {
		ID uint `json:"id"`
	}
	if err := client.PostJSON("/api/v1/deployments", body, &deployment); err != nil || deployment.ID == 0 {
		t.Fatalf("deploy %d replicas: %v (id %d)", replicas, err, deployment.ID)
	}
	waitGatewayInstancesRunning(t, client, replicas, 15*time.Minute)

	// The evaluation as the wizard submits a capacity probe of a managed
	// deployment: the server sends the load, the tokenizer comes back from the
	// node over the data lane, and natural multi-turn text is measured.
	body, _ = json.Marshal(map[string]any{
		"name": "server-origin-load", "target_kind": "instance", "deployment_id": deployment.ID,
		"load_origin": "server", "num_gpus": replicas, "mode": "saturation", "allow_conflict": true,
		"dataset": map[string]any{"kind": "multiturn", "output_tokens": deltas},
		"search": map[string]any{
			"concurrency": concurrency, "max_concurrency": concurrency, "osl": []int{deltas},
			"warmup_requests": 1, "requests_per_point_multiplier": multiplier,
		},
	})
	var run struct {
		ID uint `json:"id"`
	}
	if err := client.PostJSON("/api/v1/benchmarks", body, &run); err != nil || run.ID == 0 {
		t.Fatalf("create the evaluation: %v (id %d)", err, run.ID)
	}

	serverBefore := readProcessCPU(server.Process.Pid)
	peak := &peakRSS{}
	stopSampling := peak.sample(server.Process.Pid, time.Second)
	probe := fleet.StartListReadProbe(client, "server-origin benchmark", 3)
	t.Cleanup(func() { probe.Stop() })
	started := time.Now()
	type benchmarkRun struct {
		State    string `json:"state"`
		Error    string `json:"error"`
		Progress struct {
			Completed    int `json:"completed"`
			CurrentPoint *struct {
				RequestTotal int `json:"request_total"`
			} `json:"current_point"`
		} `json:"progress"`
		Points []struct {
			State   string  `json:"state"`
			Success int     `json:"success"`
			Errors  int     `json:"errors"`
			Output  float64 `json:"output_tput"`
		} `json:"points"`
	}
	var final benchmarkRun
	deadline := started.Add(30 * time.Minute)
	lastLog := started
	for {
		var current benchmarkRun
		if err := client.GetJSON(fmt.Sprintf("/api/v1/benchmarks/%d", run.ID), &current); err != nil {
			t.Fatalf("read the evaluation after %s: %v (server RSS peak %.0f MiB)", time.Since(started).Round(time.Second), err, peak.max())
		}
		if current.State != "running" && current.State != "queued" && current.State != "pending" {
			final = current
			break
		}
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			t.Logf("%s: %d requests completed, server RSS peak %.0f MiB, now %.0f MiB",
				time.Since(started).Round(time.Second), current.Progress.Completed, peak.max(), readProcessRSSMiB(server.Process.Pid))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the evaluation is still %s after %s", current.State, time.Since(started).Round(time.Second))
		}
		time.Sleep(2 * time.Second)
	}
	elapsed := time.Since(started)
	stopSampling()
	serverCPU := readProcessCPU(server.Process.Pid) - serverBefore
	t.Logf("evaluation %s in %s: server process %.2f cores busy (%.1f CPU-s), RSS peak %.0f MiB, now %.0f MiB",
		final.State, elapsed.Round(time.Second), serverCPU/elapsed.Seconds(), serverCPU, peak.max(), readProcessRSSMiB(server.Process.Pid))
	checkFleetReadProbe(t, probe)

	if final.State != "done" {
		t.Fatalf("evaluation ended %s (%s), want done", final.State, final.Error)
	}
	if len(final.Points) != 1 || final.Points[0].State != "done" {
		t.Fatalf("points = %+v, want one finished point", final.Points)
	}
	point := final.Points[0]
	t.Logf("point: %d succeeded, %d failed, %.0f output tokens/s", point.Success, point.Errors, point.Output)
	if point.Success < concurrency*multiplier {
		t.Errorf("%d requests succeeded, want at least the %d planned sessions", point.Success, concurrency*multiplier)
	}
	if permille := point.Errors * 1000 / max(point.Success+point.Errors, 1); permille > maxErrorPermille {
		t.Errorf("%d of %d requests failed (%d‰, budget %d‰)", point.Errors, point.Success+point.Errors, permille, maxErrorPermille)
	}
	if peak.max() > maxRSS {
		t.Errorf("server RSS peaked at %.0f MiB while it drove the evaluation (budget %.0f MiB)", peak.max(), maxRSS)
	}
}
