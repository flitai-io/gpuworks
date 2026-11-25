package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// scrapeLedger counts, per listening port, the /metrics requests it took.
type scrapeLedger struct {
	mu   sync.Mutex
	hits map[int]int
}

func (l *scrapeLedger) record(port int) {
	l.mu.Lock()
	l.hits[port]++
	l.mu.Unlock()
}

func (l *scrapeLedger) taken(port int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hits[port]
}

// The agent's real collector scrapes each instance's metrics where its desired
// state says they are (the collector ticks every ten seconds, so this runs
// for about half a minute). Two deployments of one node, one from a template that
// declares api.metrics.port: separate and one from a template that does not:
// the first serves /metrics only on its metrics port (the engine's API port
// answers 404 for it), the second only on its API port. After the collector's
// tick, the first instance's metrics port has been scraped and its API port
// never has; the second's API port has. Both instances reach running, and a
// server restart (the desired state reloaded from the database and replayed to
// the reconnecting agent) leaves the first still scraped on the metrics port.
func TestAgentScrapesTheReservedMetricsPortAndNotTheAPIPort(t *testing.T) {
	ledger := &scrapeLedger{hits: map[int]int{}}
	env := testenv.NewOnDisk(t)
	node := env.AddNode("metrics-port-scrape-node",
		testenv.WithGPUs(2, testenv.GPUSpec{Model: "NVIDIA A100", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}),
		testenv.WithDriverName("docker"),
	)
	// The API port serves the standard fake engine, except that a /metrics on it
	// is counted and, for the engine that has a port of its own for them,
	// refused. Which engine has one is read from its launch below.
	ownMetrics := map[string]bool{}
	var ownMu sync.Mutex
	node.Driver.SetEngineHandler(func(instance testenv.EngineInstance) http.Handler {
		engine := fakehandler.New(fakehandler.Options{Model: instance.Model, Dialect: "sglang"})
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				ownMu.Lock()
				refuse := ownMetrics[instance.Container]
				ownMu.Unlock()
				apiPort := r.Context().Value(http.LocalAddrContextKey).(net.Addr).(*net.TCPAddr).Port
				ledger.record(apiPort)
				if refuse {
					http.NotFound(w, r)
					return
				}
			}
			engine.ServeHTTP(w, r)
		})
	})
	var servers []*http.Server
	var serversMu sync.Mutex
	node.Driver.AfterRun(func(workload runtime.WorkloadSpec) {
		command := append(append([]string{}, workload.Entrypoint...), workload.Cmd...)
		port := flagValue(command, "--metrics-port")
		if port == "" || port == "0" {
			return
		}
		ownMu.Lock()
		ownMetrics[workload.Name] = true
		ownMu.Unlock()
		number, _ := strconv.Atoi(port)
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Errorf("metrics port %s: %v", port, err)
			return
		}
		engine := fakehandler.New(fakehandler.Options{Model: workload.Name, Dialect: "sglang"})
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			ledger.record(number)
			engine.ServeHTTP(w, r)
		})}
		serversMu.Lock()
		servers = append(servers, srv)
		serversMu.Unlock()
		go srv.Serve(listener)
	})
	t.Cleanup(func() {
		serversMu.Lock()
		defer serversMu.Unlock()
		for _, srv := range servers {
			_ = srv.Close()
		}
	})
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}

	const revision = "0123456789abcdef0123456789abcdef01234567"
	models := map[string]*store.Model{}
	var catalogModels []inferencecatalog.InferenceModel
	for _, declared := range []bool{true, false} {
		name := "gpuworks/metrics-scrape-plain"
		args := []string{"--port", "${GPUWORKS_API_PORT}"}
		metrics := &inferenceapi.Metrics{Inherit: "sglang"}
		if declared {
			name = "gpuworks/metrics-scrape-separate"
			args = append(args, "--metrics-port", "${GPUWORKS_METRICS_PORT}")
			metrics.Port = inferenceapi.MetricsPortSeparate
		}
		model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: name, Revision: revision, Name: name, Arch: "FakeForCausalLM", WeightBytes: 1 << 20, Meta: `{"model_type":"fake","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":2048}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "metrics-scrape")}); err != nil {
			t.Fatal(err)
		}
		models[name] = model
		catalogModels = append(catalogModels, inferencecatalog.InferenceModel{ModelID: name, Source: "user", Specs: []inferencecatalog.InferenceSpec{{
			Name: "scrape", Engine: "callable", Image: "example/launcher:test",
			Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: name, Revision: revision}}},
			WeightBytes: 1 << 20, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
			Entrypoint: []string{"/opt/launcher"}, Args: args,
			API: &inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}, Metrics: metrics},
		}}})
	}
	packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: catalogModels}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	request, _ := http.NewRequest(http.MethodPost, env.URL()+"/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes()))
	request.Header.Set("Content-Type", "application/gzip")
	response, err := api.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		raw := []byte{}
		if response != nil {
			raw, _ = io.ReadAll(response.Body)
			response.Body.Close()
		}
		t.Fatalf("import: err=%v body=%s", err, raw)
	}
	response.Body.Close()

	deploy := func(modelID string) store.Instance {
		t.Helper()
		template, err := env.Store().InferenceModelByModelID(modelID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"template_id": template.ID, "template_spec_name": "scrape", "node_uuid": nodes[0].UUID, "served_name": "scrape-" + strconv.Itoa(int(template.ID))})
		created, err := api.PostJSON("/api/v1/deployments", body)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(created.Body)
		created.Body.Close()
		var deployment struct {
			ID uint `json:"id"`
		}
		if created.StatusCode != http.StatusCreated || json.Unmarshal(raw, &deployment) != nil {
			t.Fatalf("create %s: %d %s", modelID, created.StatusCode, raw)
		}
		var instance store.Instance
		env.Eventually(t, 30*time.Second, func() bool {
			instances, _ := env.Store().InstancesForDeployment(deployment.ID)
			if len(instances) != 1 || instances[0].State != proto.InstRunning {
				return false
			}
			instance = instances[0]
			return true
		})
		return instance
	}
	separate := deploy("gpuworks/metrics-scrape-separate")
	plain := deploy("gpuworks/metrics-scrape-plain")
	var separateSpec, plainSpec proto.InstanceDesiredState
	if json.Unmarshal([]byte(separate.DesiredStateJSON), &separateSpec) != nil || json.Unmarshal([]byte(plain.DesiredStateJSON), &plainSpec) != nil {
		t.Fatal("desired state does not decode")
	}
	if separateSpec.MetricsPort <= 0 || plainSpec.MetricsPort != 0 {
		t.Fatalf("metrics ports: separate=%d plain=%d", separateSpec.MetricsPort, plainSpec.MetricsPort)
	}
	if slices.Contains(plainSpec.Ports, separateSpec.MetricsPort) || slices.Contains(plainSpec.Ports, separateSpec.APIPort) {
		t.Fatalf("ports overlap: separate %v plain %v", separateSpec.Ports, plainSpec.Ports)
	}

	// The collector ticks every ten seconds.
	wait := func(what string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("%s: separate api=%d metrics=%d, plain api=%d", what,
			ledger.taken(separateSpec.APIPort), ledger.taken(separateSpec.MetricsPort), ledger.taken(plainSpec.APIPort))
	}
	wait("the first scrapes", func() bool {
		return ledger.taken(separateSpec.MetricsPort) >= 1 && ledger.taken(plainSpec.APIPort) >= 1
	})
	if got := ledger.taken(separateSpec.APIPort); got != 0 {
		t.Fatalf("the API port of the instance with a metrics port was scraped %d times", got)
	}

	// The server restarts; the agent reconnects and takes the desired state the
	// restarted server reads back from the database.
	before := ledger.taken(separateSpec.MetricsPort)
	env.RestartServer(t)
	env.WaitNodeOnline(t, node.Name)
	reloaded, err := env.Store().InstanceByUUID(separate.UUID)
	if err != nil || reloaded.DesiredStateJSON != separate.DesiredStateJSON {
		t.Fatalf("the instance changed across the server restart: %+v err=%v", reloaded, err)
	}
	wait(fmt.Sprintf("scraping after the restart (was %d)", before), func() bool { return ledger.taken(separateSpec.MetricsPort) > before })
	if got := ledger.taken(separateSpec.APIPort); got != 0 {
		t.Fatalf("after the restart the API port of the instance with a metrics port was scraped %d times", got)
	}

	// The agent restarts too (a fresh agent adopting the running engines and
	// taking the desired state from the server): still the metrics port.
	before = ledger.taken(separateSpec.MetricsPort)
	env.RestartNode(t, node.Name)
	env.WaitNodeOnline(t, node.Name)
	wait(fmt.Sprintf("scraping after the agent restart (was %d)", before), func() bool { return ledger.taken(separateSpec.MetricsPort) > before })
	if got := ledger.taken(separateSpec.APIPort); got != 0 {
		t.Fatalf("after the agent restart the API port of the instance with a metrics port was scraped %d times", got)
	}
	if current, err := env.Store().InstanceByUUID(separate.UUID); err != nil || current.State != proto.InstRunning || current.DesiredStateJSON != separate.DesiredStateJSON {
		t.Fatalf("the instance after the agent restart: %+v err=%v", current, err)
	}
}
