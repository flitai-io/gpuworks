//go:build multiproc

package l2

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// metricsPortInstance is what an instance's row says about its ports: the
// deployment it serves, the ports the node holds for it, and the desired state
// the server pushes to the agent, as the keys the agent receives.
type metricsPortInstance struct {
	UUID         string
	DeploymentID uint
	Ports        []int
	Desired      map[string]json.RawMessage
	APIPort      int
	MetricsPort  int
}

func readMetricsPortInstances(t *testing.T, dataDir string) map[uint]metricsPortInstance {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "gpuworks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT uuid, deployment_id, ports, spec FROM instances WHERE spec != ''`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uint]metricsPortInstance{}
	for rows.Next() {
		var in metricsPortInstance
		var ports, spec string
		if err := rows.Scan(&in.UUID, &in.DeploymentID, &ports, &spec); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(ports), &in.Ports); err != nil {
			t.Fatalf("ports %q: %v", ports, err)
		}
		if err := json.Unmarshal([]byte(spec), &in.Desired); err != nil {
			t.Fatalf("spec %q: %v", spec, err)
		}
		_ = json.Unmarshal(in.Desired["api_port"], &in.APIPort)
		_ = json.Unmarshal(in.Desired["metrics_port"], &in.MetricsPort)
		out[in.DeploymentID] = in
	}
	return out
}

// engineScrapes counts, per fake engine log, the /metrics requests by the port
// they arrived on ("metrics scraped on port N") and the ones an API port
// refused ("metrics requested on API port N").
func engineScrapes(t *testing.T, dir string) (scraped map[int]int, refused map[int]int) {
	t.Helper()
	scraped, refused = map[int]int{}, map[int]int{}
	logs, _ := filepath.Glob(filepath.Join(dir, "engine-*.log"))
	for _, path := range logs {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if _, rest, ok := strings.Cut(line, "metrics scraped on port "); ok {
				if port, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					scraped[port]++
				}
			}
			if _, rest, ok := strings.Cut(line, "metrics requested on API port "); ok {
				if port, err := strconv.Atoi(strings.Fields(rest)[0][:len(strings.Fields(rest)[0])-1]); err == nil {
					refused[port]++
				}
			}
		}
	}
	return scraped, refused
}

// killEnginesOf stops the fake engines a stopped agent left behind: the ones
// whose command line names this test's model directory.
func killEnginesOf(modelDir string) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte(modelDir)) || !bytes.Contains(cmdline, []byte("fake-engine")) {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}
}

func metricsPortTemplate(modelID, revision string) inferencecatalog.InferenceModel {
	spec := func(name string, args []string, metrics *inferenceapi.Metrics) inferencecatalog.InferenceSpec {
		return inferencecatalog.InferenceSpec{
			Name: name, Engine: "callable", Image: "example/launcher:test",
			Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID, Revision: revision}}},
			WeightBytes: 7, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia"},
			Entrypoint: []string{"/opt/launcher"}, Args: args,
			API: &inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}, Metrics: metrics},
		}
	}
	return inferencecatalog.InferenceModel{ModelID: modelID, Source: "user", Architecture: "FakeForCausalLM", InputModalities: []string{"text"}, Specs: []inferencecatalog.InferenceSpec{
		spec("separate", []string{"--port", "${GPUWORKS_API_PORT}", "--metrics-port", "${GPUWORKS_METRICS_PORT}"}, &inferenceapi.Metrics{Port: inferenceapi.MetricsPortSeparate, Inherit: "sglang"}),
		spec("plain", []string{"--port", "${GPUWORKS_API_PORT}"}, &inferenceapi.Metrics{Inherit: "sglang"}),
	}}
}

// TestMetricsPortServedOnItsOwnPortIsScrapedByTheRealAgent is
// l2-metrics-port-separate-scrape: real server and agent processes, the
// recording runtime starting a fake engine for every serving container. Three
// callable deployments share one node: two of a template spec that declares
// api.metrics.port: separate and one of a spec that does not. The fake engine
// of the first kind serves /metrics only on the port the platform reserved
// for it and refuses it on its API port; the other serves it on its API port.
//
//   - the platform reserves a second port per declaring instance, distinct
//     from every port of every instance on the node, and writes it into the
//     desired state the agent receives (the undeclared one has no such key);
//   - the agent scrapes the metrics port of the first kind and never asks its
//     API port for /metrics, the API port of the second;
//   - this holds across a server restart (the desired state read back from the
//     database and replayed to the reconnected agent);
//   - re-importing the template without the declaration leaves the running
//     deployments as they are and gives the next deployment no second port.
//
// GPUWORKS_L2_LEGACY_AGENT_BIN names an agent binary built before the field
// existed; the case then runs only as far as the instances launching and their
// API ports being scraped.
//
// Run: go test -tags=multiproc ./test/l2/ -run TestMetricsPortServedOnItsOwnPortIsScrapedByTheRealAgent -count=1 -v
func TestMetricsPortServedOnItsOwnPortIsScrapedByTheRealAgent(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	legacyAgent := os.Getenv("GPUWORKS_L2_LEGACY_AGENT_BIN")
	if legacyAgent != "" {
		// An agent built from a revision that predates the field, to run the
		// server side of the case against it: it must launch the instances and
		// scrape their API ports, as before. Its fake runtime does not hand an
		// engine a metrics port, so the engines serve /metrics on the API port.
		agentBin = legacyAgent
	}
	engineBin := buildBinary(t, "./test/fake-engine")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	startServer := func() *exec.Cmd {
		command := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
		command.Stdout, command.Stderr = serverLog, serverLog
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	server := startServer()
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "metrics-port-process")

	recordDir := t.TempDir()
	recordPath := filepath.Join(recordDir, "workload.json")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x4,81920",
		"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
		"GPUWORKS_FAKE_RUNTIME_ENGINE="+engineBin,
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 4
	})

	const modelID = "gpuworks/l2-metrics-port"
	const revision = "0123456789abcdef0123456789abcdef01234567"
	modelBody, _ := json.Marshal(map[string]any{
		"source": "huggingface", "source_id": modelID, "revision": revision, "name": modelID,
		"browser_metadata": true, "weight_bytes": 7,
		"config_raw": `{"model_type":"fake","architectures":["FakeForCausalLM"],"max_position_embeddings":2048}`,
	})
	modelResponse, modelData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models", modelBody)
	var model struct {
		ID uint `json:"id"`
	}
	if modelResponse.StatusCode != http.StatusCreated || json.Unmarshal(modelData, &model) != nil || model.ID == 0 {
		t.Fatalf("create model status=%d body=%s", modelResponse.StatusCode, modelData)
	}
	modelDir := filepath.Join(t.TempDir(), "metrics-port-model")
	t.Cleanup(func() { killEnginesOf(modelDir) })
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"fake","architectures":["FakeForCausalLM"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	registerBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir})
	if resp, data := authedDo(t, http.MethodPost, base, adminToken, fmt.Sprintf("/api/v1/models/%d/register-path", model.ID), registerBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("register path status=%d body=%s", resp.StatusCode, data)
	}

	importTemplate := func(catalog inferencecatalog.InferenceModel) {
		t.Helper()
		packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{catalog}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/inference-templates/import?overwrite=true", bytes.NewReader(archive.Bytes()))
		request.Header.Set("Authorization", "Bearer "+adminToken)
		request.Header.Set("Content-Type", "application/gzip")
		response, err := processHTTPClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("import status=%d body=%s", response.StatusCode, raw)
		}
	}
	importTemplate(metricsPortTemplate(modelID, revision))
	templateID := findTemplateID(t, base, adminToken, modelID)
	deploy := func(spec, name string) uint {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"template_id": templateID, "template_spec_name": spec, "model_id": model.ID, "node_uuid": node.UUID, "served_name": name})
		resp, data := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", body)
		var created struct {
			ID uint `json:"id"`
		}
		if resp.StatusCode != http.StatusCreated || json.Unmarshal(data, &created) != nil {
			t.Fatalf("deploy %s status=%d body=%s", name, resp.StatusCode, data)
		}
		waitInstance(t, base, adminToken, func(in processInstance) bool { return in.DeploymentID == created.ID && in.State == "running" })
		return created.ID
	}
	sepA := deploy("separate", "own-metrics-a")
	sepB := deploy("separate", "own-metrics-b")
	plain := deploy("plain", "api-metrics")

	// The ports the node holds: two for each declaring instance, one for the
	// other, no port twice, and the key present in the desired state of the
	// first kind only.
	instances := readMetricsPortInstances(t, dataDir)
	if len(instances) != 3 {
		t.Fatalf("instances=%+v", instances)
	}
	held := map[int]uint{}
	for deployment, in := range instances {
		want := 2
		if deployment == plain {
			want = 1
		}
		if len(in.Ports) != want || in.Ports[0] != in.APIPort {
			t.Fatalf("deployment %d ports %v api %d", deployment, in.Ports, in.APIPort)
		}
		_, hasKey := in.Desired["metrics_port"]
		if (deployment == plain) == hasKey || (want == 2 && (in.MetricsPort != in.Ports[1] || in.MetricsPort == in.APIPort)) {
			t.Fatalf("deployment %d desired state %v", deployment, in.Desired)
		}
		for _, p := range in.Ports {
			if owner, taken := held[p]; taken {
				t.Fatalf("port %d is held by deployments %d and %d", p, owner, deployment)
			}
			held[p] = deployment
		}
	}

	// The collector ticks every ten seconds.
	waitScrapes := func(what string, done func(scraped, refused map[int]int) bool) (map[int]int, map[int]int) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			scraped, refused := engineScrapes(t, recordDir)
			if done(scraped, refused) {
				return scraped, refused
			}
			time.Sleep(500 * time.Millisecond)
		}
		scraped, refused := engineScrapes(t, recordDir)
		t.Fatalf("%s: scraped=%v refused=%v instances=%+v", what, scraped, refused, instances)
		return nil, nil
	}
	if legacyAgent != "" {
		waitScrapes("an agent that predates the field scrapes the API ports", func(scraped, _ map[int]int) bool {
			for _, in := range instances {
				if scraped[in.APIPort] < 2 {
					return false
				}
			}
			return true
		})
		return
	}
	allScraped := func(scraped, _ map[int]int) bool {
		return scraped[instances[sepA].MetricsPort] >= 2 && scraped[instances[sepB].MetricsPort] >= 2 && scraped[instances[plain].APIPort] >= 2
	}
	scraped, refused := waitScrapes("first scrapes", allScraped)
	for _, deployment := range []uint{sepA, sepB} {
		in := instances[deployment]
		if scraped[in.APIPort] != 0 || refused[in.APIPort] != 0 {
			t.Fatalf("deployment %d: its API port %d was asked for /metrics (%d served, %d refused)", deployment, in.APIPort, scraped[in.APIPort], refused[in.APIPort])
		}
	}

	// What the scrape produced reaches the monitor under the instance's own
	// label: the engine's running-request gauge, which only the scrape of the
	// right port can have carried (the API port of a declaring instance
	// answers 404).
	for _, deployment := range []uint{sepA, sepB, plain} {
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp, data := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/monitor/deployments/"+itoa(deployment)+"?range=15m", nil)
			var view struct {
				Charts struct {
					Concurrency []struct {
						Labels map[string]string `json:"labels"`
						Points [][2]float64      `json:"points"`
					} `json:"concurrency"`
				} `json:"charts"`
			}
			found := false
			if resp.StatusCode == http.StatusOK && json.Unmarshal(data, &view) == nil {
				for _, series := range view.Charts.Concurrency {
					found = found || (series.Labels["instance"] == instances[deployment].UUID && series.Labels["metric"] == "running" && len(series.Points) > 0)
				}
			}
			if found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the monitor of deployment %d never carried the scraped running-request series of instance %s: %s", deployment, instances[deployment].UUID, data)
			}
			time.Sleep(2 * time.Second)
		}
	}

	// Server restart: the instances keep their rows, ports and desired state,
	// and the reconnected agent still scrapes the metrics ports.
	before := instances
	baseline, _ := engineScrapes(t, recordDir)
	stopProcess(server)
	server = startServer()
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	// A deployment of the edited template follows, so wait for the cards too:
	// online is set at the handshake, before the status report that carries them.
	waitNodeWithCards(t, base, adminToken, node.UUID)
	after := readMetricsPortInstances(t, dataDir)
	for deployment, in := range before {
		got := after[deployment]
		if got.UUID != in.UUID || !slices.Equal(got.Ports, in.Ports) || got.APIPort != in.APIPort || got.MetricsPort != in.MetricsPort {
			t.Fatalf("deployment %d changed across the server restart: %+v -> %+v", deployment, in, got)
		}
	}
	scraped, refused = waitScrapes("scrapes after the server restart", func(scraped, _ map[int]int) bool {
		return scraped[instances[sepA].MetricsPort] > baseline[instances[sepA].MetricsPort] &&
			scraped[instances[sepB].MetricsPort] > baseline[instances[sepB].MetricsPort] &&
			scraped[instances[plain].APIPort] > baseline[instances[plain].APIPort]
	})
	for _, deployment := range []uint{sepA, sepB} {
		if in := instances[deployment]; scraped[in.APIPort] != 0 || refused[in.APIPort] != 0 {
			t.Fatalf("after the restart deployment %d's API port %d was asked for /metrics", deployment, in.APIPort)
		}
	}

	// The template is re-imported without the declaration: the running
	// deployments keep their ports, the next deployment of the spec gets one.
	edited := metricsPortTemplate(modelID, revision)
	edited.Specs[0].Args = []string{"--port", "${GPUWORKS_API_PORT}"}
	edited.Specs[0].API.Metrics = &inferenceapi.Metrics{Inherit: "sglang"}
	importTemplate(edited)
	next := deploy("separate", "after-the-edit")
	final := readMetricsPortInstances(t, dataDir)
	if _, present := final[next].Desired["metrics_port"]; present || len(final[next].Ports) != 1 {
		t.Fatalf("a deployment of the edited template reserved a metrics port: %+v", final[next])
	}
	for _, deployment := range []uint{sepA, sepB, plain} {
		if got := final[deployment]; got.MetricsPort != before[deployment].MetricsPort || !slices.Equal(got.Ports, before[deployment].Ports) {
			t.Fatalf("the template edit moved deployment %d's ports: %+v -> %+v", deployment, before[deployment], got)
		}
	}
}
