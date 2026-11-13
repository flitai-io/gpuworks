package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestFleetLogCenterScale is the log-center's load verdict: while a
// fleet-scale set of agents streams instance logs through the control plane
// and half the nodes redeploy in a loop, an operator opens the log center and
// searches. The case holds the read targets the product promises — first
// screen, keyword search, histogram, and ingest freshness — measured against
// the real HTTP endpoints.
//
// Gated and sized like the churn case, on an idle machine:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleetLogCenterScale$'
//
// with GPUWORKS_FLEET_NODES / GPUWORKS_FLEET_CHURN_NODES / GPUWORKS_FLEET_ROUNDS /
// GPUWORKS_LOG_LINES_PER_INSTANCE_PER_SEC / GPUWORKS_LOG_SAMPLE_SECONDS to
// scale it.
func TestFleetLogCenterScale(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 1000)
	churnCount := EnvInt("GPUWORKS_FLEET_CHURN_NODES", nodeCount/2)
	rounds := EnvInt("GPUWORKS_FLEET_ROUNDS", 2)
	sampleWindow := time.Duration(EnvInt("GPUWORKS_LOG_SAMPLE_SECONDS", 90)) * time.Second
	// Lines per running instance per second. At fleet size the default puts
	// the aggregate near the product's target peak of ~2000 lines/s.
	ratePerInstance := float64(EnvInt("GPUWORKS_LOG_LINES_PER_INSTANCE_PER_SEC", 2))
	if churnCount > nodeCount {
		churnCount = nodeCount
	}

	env := testenv.NewOnDisk(t)
	names := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("fleet-%04d", i)
		node := env.AddNode(name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
		node.Driver.ShareEnginePorts()
		// Every running instance streams synthetic engine output at the
		// configured rate: mostly INFO request chatter on application
		// endpoints, a WARN band, and a CUDA OOM error tail.
		node.Driver.OnLogs(func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
			return newRateLogStream(ratePerInstance, id, follow, tail), nil, true
		})
		names = append(names, name)
	}

	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatalf("log in: %v", err)
	}
	start := time.Now()
	if online, err := WaitFleetOnline(client, nodeCount, 10*time.Minute); err != nil {
		t.Fatalf("%v", err)
	} else {
		t.Logf("%d nodes online in %s", online, time.Since(start).Round(time.Millisecond))
	}

	rows, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]store.Node{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every node can run the model without a download; the churn nodes are the
	// ones that redeploy in a loop (the deploy storm).
	nodeUUIDs := make([]string, 0, nodeCount)
	churnUUIDs := make([]string, 0, churnCount)
	for i, name := range names {
		node := byName[name]
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
			t.Fatal(err)
		}
		nodeUUIDs = append(nodeUUIDs, node.UUID)
		if i < churnCount {
			churnUUIDs = append(churnUUIDs, node.UUID)
		}
	}

	// The steady load: long-running deployments on every non-churn node. The
	// churn nodes add the storm — their deployments are redeployed in rounds
	// and deleted when each round ends, which is exactly the burst the log
	// center has to ride through.
	longRunning := nodeUUIDs[churnCount:]
	deployGroup := &sync.WaitGroup{}
	for _, nodeUUID := range longRunning {
		deployGroup.Add(1)
		go func(nodeUUID string) {
			defer deployGroup.Done()
			body, _ := json.Marshal(map[string]any{
				"model_id": model.ID, "node_uuid": nodeUUID, "served_name": "fleet-" + nodeUUID,
				"quick_params": map[string]any{"max_model_len": 512},
			})
			var created struct {
				ID uint `json:"id"`
			}
			if err := client.PostJSON("/api/v1/deployments", body, &created); err != nil {
				t.Errorf("deploy %s: %v", nodeUUID, err)
			}
		}(nodeUUID)
	}
	deployGroup.Wait()
	if running, err := WaitInstancesRunning(client, len(longRunning), 5*time.Minute); err != nil {
		t.Fatalf("%v", err)
	} else {
		t.Logf("steady state: %d long-running instances before the storm", running)
	}

	// Run the deploy storm in the background: rounds of redeployments across
	// the churn nodes while the read window samples the log center.
	watch := &NodeWatch{Snapshot: func() ([]NodeState, error) { return ListNodes(client) }}
	storm := &Churn{
		Client: client, ModelID: model.ID, ChurnNodes: churnUUIDs,
		Rounds: rounds, Reads: false, Watch: watch,
		Log: func(format string, args ...any) { t.Logf(format, args...) },
	}
	stormDone := make(chan struct{})
	var report Report
	var stormErr error
	go func() {
		defer close(stormDone)
		report, stormErr = storm.Run()
	}()

	// Sample the log-center reads while the storm runs. This window IS the
	// measurement: an operator searching while the fleet churns.
	t.Logf("sampling log-center reads for %s while %d nodes churn", sampleWindow, churnCount)
	logReport := SampleLogReads(context.Background(), client, sampleWindow)
	// The storm finishes all its rounds even if the sampling window closes
	// first; the fleet verdict still needs its report.
	<-stormDone
	if stormErr != nil {
		t.Fatalf("churn: %v", stormErr)
	}

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("fleet=%d churn=%d rounds=%d rate/instance=%.2f/s window=%s goroutines=%d heap=%.1f MiB",
		nodeCount, churnCount, rounds, ratePerInstance, sampleWindow, runtime.NumGoroutine(), float64(memory.HeapInuse)/(1<<20))
	for _, line := range report.Lines() {
		t.Log(line)
	}
	for _, line := range logReport.Lines() {
		t.Log(line)
	}

	// Verdicts: the read targets, in the same terms the plan states them.
	assertLatency(t, "first screen", logReport.First, 500*time.Millisecond)
	assertLatency(t, "keyword search", logReport.Keyword, 6*time.Second)
	assertLatency(t, "histogram", logReport.Histogram, time.Second)
	if logReport.Freshness.Count() > 0 {
		if worst := logReport.Freshness.Percentile(0.95); worst > 60*time.Second {
			t.Errorf("ingest freshness p95 = %s, want <= 60s: the pipeline fell behind the writers", worst)
		}
	} else {
		t.Error("no freshness samples: the first screen returned no rows at all")
	}
	if logReport.WireError != "" {
		t.Errorf("log-center response off the documented wire shape: %s", logReport.WireError)
	}
	if logReport.Samples < 5 {
		t.Errorf("only %d samples in the window", logReport.Samples)
	}
	lost, err := OfflineNodes(client)
	if err != nil {
		t.Fatalf("read the fleet after the storm: %v", err)
	}
	for _, problem := range report.Problems(lost) {
		t.Error(problem)
	}
	client.CloseIdleConnections()
	env.StopNodes(t)
}

func assertLatency(t *testing.T, name string, l *Latencies, budget time.Duration) {
	t.Helper()
	if l.Count() == 0 {
		t.Errorf("%s: not sampled", name)
		return
	}
	if p95 := l.Percentile(0.95); p95 > budget {
		t.Errorf("%s p95 = %s, want <= %s (n=%d)", name, p95.Round(time.Millisecond), budget, l.Count())
	}
}
