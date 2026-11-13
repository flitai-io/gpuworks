//go:build multiproc

package l2

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// monitorPoint mirrors dto.Point ([t,v] pairs) without importing
// internal/server/api/dto, matching this file's neighbors (processInstance,
// processNode) which all define their own minimal wire-shape mirrors rather
// than reaching into the server's internal packages from a real-process
// test.
type monitorPoint [2]float64

type monitorSeriesDTO struct {
	Labels map[string]string `json:"labels"`
	Points []monitorPoint    `json:"points"`
}

type monitorChartsDTO struct {
	Latency []monitorSeriesDTO `json:"latency"`
	TPOT    []monitorSeriesDTO `json:"tpot"`
}

type monitorDeploymentDTO struct {
	Diagnoses []struct {
		Code string `json:"code"`
	} `json:"diagnoses"`
	Charts monitorChartsDTO `json:"charts"`
}

type monitorSignalDTO struct {
	Key       string `json:"key"`
	Collected bool   `json:"collected"`
}

type monitorOverviewDTO struct {
	Signals []monitorSignalDTO `json:"signals"`
}

// TestMonitorOverviewReportsQuantilesFromFakeEngineHistograms proves the
// monitor center's second-phase pipeline end to end across real process
// boundaries (plan artifacts/archive/2026-09-19-monitor-center/plan.md §8: "L2：
// fake-engine 暴露直方图与抢占计数，验证端到端分位数与 rate"): a real
// gpuworks-agent scrapes a real fake-engine container's Prometheus
// /metrics endpoint over several real 10s collector ticks, the real server
// ingests those histogram buckets into quantile-at-ingest gauges
// (internal/server/app/observability_quantile.go) and the real preemptions
// counter, and GET /api/v1/monitor/deployments/{id} and
// /api/v1/monitor/series surface both back out over HTTP.
//
// Like TestProcessSleepWakeReconcilesTheRealEngine, this needs a live engine
// process actually answering /metrics, so it runs the fake engine in a real
// Docker container (GPUWORKS_RUNTIME_DRIVER=docker) rather than the
// recording-runtime path most other L2 cases use; it is skipped when Docker
// is unavailable, and fails outright under GPUWORKS_TEST_STRICT=1.
func TestMonitorOverviewReportsQuantilesFromFakeEngineHistograms(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker required in strict mode: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker daemon unavailable in strict mode: %v", err)
		}
		t.Skipf("docker daemon unavailable: %v", err)
	}
	root := repoRoot()
	if err := ensureFakeEngineImage(t, root); err != nil {
		t.Fatal(err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "monitor-quantiles-node")

	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,24576",
		"GPUWORKS_FAKE_GPU_NO_INJECT=1",
		"GPUWORKS_RUNTIME_DRIVER=docker",
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	waitNodeWithCards(t, base, adminToken, seed.UUID)

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "monitor-quantiles-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(localData, &model); err != nil {
		t.Fatal(err)
	}

	// Fixed, deterministic histogram/counter values (test/fake-engine/handler
	// /metrics.go): every scrape adds MetricsObservationsPerScrape
	// observations at exactly these seconds values and
	// MetricsPreemptionsPerScrape more to the preemptions counter, so the
	// quantile-at-ingest gauges and the preemptions rate are both exact and
	// stable across collector ticks, not just "eventually nonzero".
	//
	// The expected p95 is NOT the raw configured value: every observation
	// this fixture makes lands at exactly the same value, so the family's
	// bucket vector is a step function with all mass in one bucket, and
	// Prometheus' histogram_quantile linearly interpolates the p95 rank
	// across that whole bucket's width rather than reporting the exact
	// value - see wantTTFTMs/wantTPOTMs below, computed by the same
	// interpolation over vLLM's real bucket boundaries
	// (test/fake-engine/handler/metrics.go's ttftBucketsSeconds/
	// tpotBucketsSeconds). Both values are chosen to land in a narrow early
	// bucket so the interpolation error stays small.
	const ttftSeconds = "0.085" // bucket (0.08, 0.1]
	const tpotSeconds = "0.012" // bucket (0.01, 0.025]
	deployBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": seed.UUID, "served_name": "monitor-quantiles",
		"engine": "vllm", "image": "gpuworks/fake-engine:test",
		"container_overrides": map[string]any{"env": map[string]string{
			"FAKE_ENGINE_METRICS_TTFT_SECONDS":            ttftSeconds,
			"FAKE_ENGINE_METRICS_TPOT_SECONDS":            tpotSeconds,
			"FAKE_ENGINE_METRICS_OBSERVATIONS_PER_SCRAPE": "5",
			"FAKE_ENGINE_METRICS_PREEMPTIONS_PER_SCRAPE":  "2",
		}},
	})
	deployResp, deployData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", deployBody)
	if deployResp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", deployResp.StatusCode, deployData)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(deployData, &created); err != nil {
		t.Fatal(err)
	}

	instance := waitInstance(t, base, adminToken, func(inst processInstance) bool {
		return inst.DeploymentID == created.ID && inst.State == "running" && inst.ContainerID != ""
	})
	t.Cleanup(func() {
		deleteDeployment(t, base, adminToken, created.ID)
		if instance.ContainerID != "" {
			_ = exec.Command("docker", "rm", "-f", instance.ContainerID).Run()
		}
	})

	// The collector ticks every 10s (internal/agent/collector/collector.go)
	// and the quantile-at-ingest bucket-delta needs at least two ticks before
	// it emits its first gauge (the very first scrape only establishes the
	// baseline vector - see observability_quantile.go's doc comment), so wait
	// for at least 3 ticks worth of real wall-clock time. The deadline below
	// gives that generous headroom for a slow CI runner without letting a
	// genuine regression hang the suite.
	const deadline = 90 * time.Second
	deployPath := "/api/v1/monitor/deployments/" + itoa(created.ID) + "?range=15m"
	overviewPath := "/api/v1/monitor/overview"
	seriesPath := "/api/v1/monitor/series?metric=gpuworks_infer_preemptions_total&fn=rate"

	var latestTTFT, latestTPOT monitorSeriesDTO
	var haveTTFT, haveTPOT bool
	deadlineAt := time.Now().Add(deadline)
	for time.Now().Before(deadlineAt) {
		resp, data := authedDo(t, http.MethodGet, base, adminToken, deployPath, nil)
		if resp.StatusCode == http.StatusOK {
			var dep monitorDeploymentDTO
			if err := json.Unmarshal(data, &dep); err == nil {
				// TTFT is on the latency chart, TPOT on its own.
				for _, s := range append(dep.Charts.Latency, dep.Charts.TPOT...) {
					switch s.Labels["metric"] {
					case "ttft_p95":
						if len(s.Points) > 0 {
							latestTTFT, haveTTFT = s, true
						}
					case "tpot_p95":
						if len(s.Points) > 0 {
							latestTPOT, haveTPOT = s, true
						}
					}
				}
			}
		} else {
			resp.Body.Close()
		}
		if haveTTFT && haveTPOT {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !haveTTFT || !haveTPOT {
		resp, data := authedDo(t, http.MethodGet, base, adminToken, deployPath, nil)
		resp.Body.Close()
		t.Fatalf("charts.latency/charts.tpot never carried a non-empty ttft_p95/tpot_p95 series within %s: last response=%s", deadline, data)
	}
	// chartSeriesDTO (monitorapi/deployment.go) reports latency in
	// milliseconds. Every observation lands at exactly ttftSeconds/
	// tpotSeconds, so the bucket vector is a step function with 100% of the
	// mass in the bucket (lower, upper] straddling that value; classic-
	// histogram histogram_quantile interpolates the p95 rank linearly across
	// that bucket's whole width: lower + 0.95*(upper-lower). Both values were
	// chosen to land in a narrow bucket (width 0.02s / 0.015s) so this is a
	// tight, still-deterministic target rather than the raw observation
	// value itself.
	wantTTFTMs := (0.08 + 0.95*(0.1-0.08)) * 1000   // = 99
	wantTPOTMs := (0.01 + 0.95*(0.025-0.01)) * 1000 // = 24.25
	gotTTFTMs := latestTTFT.Points[len(latestTTFT.Points)-1][1]
	gotTPOTMs := latestTPOT.Points[len(latestTPOT.Points)-1][1]
	if diff := gotTTFTMs - wantTTFTMs; diff < -5 || diff > 5 {
		t.Fatalf("ttft_p95 latest point = %v ms, want ~%v ms (histogram_quantile interpolation over the fixed observation's bucket)", gotTTFTMs, wantTTFTMs)
	}
	if diff := gotTPOTMs - wantTPOTMs; diff < -5 || diff > 5 {
		t.Fatalf("tpot_p95 latest point = %v ms, want ~%v ms (histogram_quantile interpolation over the fixed observation's bucket)", gotTPOTMs, wantTPOTMs)
	}

	overviewResp, overviewData := authedDo(t, http.MethodGet, base, adminToken, overviewPath, nil)
	if overviewResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", overviewPath, overviewResp.StatusCode, overviewData)
	}
	var overview monitorOverviewDTO
	if err := json.Unmarshal(overviewData, &overview); err != nil {
		t.Fatalf("decode overview: %v (%s)", err, overviewData)
	}
	ttftCollected := false
	for _, sig := range overview.Signals {
		if sig.Key == "ttft" {
			ttftCollected = sig.Collected
		}
	}
	if !ttftCollected {
		t.Fatalf("overview signals never reported ttft.collected=true: %s", overviewData)
	}

	var seriesResult []monitorSeriesDTO
	seriesDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(seriesDeadline) {
		resp, data := authedDo(t, http.MethodGet, base, adminToken, seriesPath, nil)
		if resp.StatusCode == http.StatusOK {
			if err := apiwire.UnmarshalList(data, &seriesResult); err != nil {
				t.Fatalf("decode %s: %v (%s)", seriesPath, err, data)
			}
			if len(seriesResult) > 0 && len(seriesResult[0].Points) > 0 {
				break
			}
		} else {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	if len(seriesResult) == 0 || len(seriesResult[0].Points) == 0 {
		t.Fatalf("/monitor/series?metric=gpuworks_infer_preemptions_total&fn=rate never returned a non-empty series")
	}
}
