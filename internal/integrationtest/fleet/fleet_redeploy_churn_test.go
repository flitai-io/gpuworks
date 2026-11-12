package fleet

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The fleet case is a load measurement, so it is gated the same way the tunnel
// throughput assertions are: it runs on an idle machine on request, not in the
// default suite.
func skipUnlessPerf(t *testing.T) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
}

// TestFleet1000NodesRedeployChurn holds a fleet of nodes connected while half
// of them redeploy in a loop, and measures what that costs the control plane:
// event rows per deployment, whether heartbeats still land (a node wrongly
// marked offline is the failure this whole change exists to prevent), and the
// read latency of the two endpoints an operator refreshes.
//
// Server and fleet share one process here. That is the cheapest shape to run
// and the one that cannot tell the two apart: its RSS, its CPU and its
// scheduler are the sum of both. The process-separated shape in test/l2 runs
// the same workload with the server as its own process, and is the one to read
// when the question is what the server alone costs.
//
// Node and round counts come from the environment so the case can be run at a
// tractable size locally and at fleet size on a machine that has the memory:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleet1000NodesRedeployChurn$'
//
// GPUWORKS_FLEET_NODES / GPUWORKS_FLEET_CHURN_NODES / GPUWORKS_FLEET_ROUNDS
// scale it.
func TestFleet1000NodesRedeployChurn(t *testing.T) {
	skipUnlessPerf(t)
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 1000)
	churnCount := EnvInt("GPUWORKS_FLEET_CHURN_NODES", nodeCount/2)
	if churnCount > nodeCount {
		churnCount = nodeCount
	}
	requireFileDescriptors(t, 6*nodeCount+2048)

	// A durable database is the point: an in-memory one never fsyncs, so it
	// cannot show whether the write volume still leaves room for heartbeats.
	env := testenv.NewOnDisk(t)
	if env.DBPath() == "" {
		t.Fatal("the fleet case needs a durable database")
	}
	transcript := PullTranscript(2000)

	names := make([]string, nodeCount)
	start := time.Now()
	for i := range names {
		names[i] = fmt.Sprintf("fleet-%04d", i)
		node := env.AddNode(names[i], testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
		// Every node is a separate machine as far as the scheduler is
		// concerned, so they all get the same engine port. On one host that is
		// one bind; see FakeDriver.ShareEnginePorts.
		node.Driver.ShareEnginePorts()
		if i < churnCount {
			node.Driver.OnPullImage(func(_ string, onLine func(string)) (error, bool) {
				for _, line := range transcript {
					onLine(line)
				}
				return nil, true
			})
		}
	}

	client, err := Login(env.URL(), "admin", "password")
	if err != nil {
		t.Fatalf("log in to the control plane: %v", err)
	}
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
	// Seeding the model and marking it present is setup, not measurement: it
	// happens before the window and the in-process shape can do it directly.
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	churnUUIDs := make([]string, 0, churnCount)
	for i := 0; i < churnCount; i++ {
		node := byName[names[i]]
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
			t.Fatal(err)
		}
		churnUUIDs = append(churnUUIDs, node.UUID)
	}

	baselineDB := fileSize(t, env.DBPath())
	dump := newStallDumper()
	enableContentionProfiling(dump)
	watch := &NodeWatch{
		Snapshot:     func() ([]NodeState, error) { return ListNodes(client) },
		OnStall:      dump.onStall(),
		StallTrigger: dump.trigger(),
	}
	rounds := EnvInt("GPUWORKS_FLEET_ROUNDS", 2)
	report, err := Churn{
		Client: client, ModelID: model.ID, ChurnNodes: churnUUIDs,
		Rounds: rounds, Reads: ReadsEnabled(), Watch: watch,
		Log: func(format string, args ...any) { t.Logf(format, args...) },
	}.Run()
	if err != nil {
		t.Fatalf("churn: %v", err)
	}

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	grown := fileSize(t, env.DBPath()) - baselineDB
	t.Logf("fleet=%d churn=%d rounds=%d", nodeCount, churnCount, rounds)
	for _, line := range report.Lines() {
		t.Log(line)
	}
	t.Logf("database grew %.1f MiB; goroutines=%d; heap in use=%.1f MiB; one-process RSS=%.1f MiB",
		float64(grown)/(1<<20), runtime.NumGoroutine(), float64(memory.HeapInuse)/(1<<20), processRSSMiB())

	lost, err := OfflineNodes(client)
	if err != nil {
		t.Fatalf("read the fleet after the churn: %v", err)
	}
	for _, problem := range report.Problems(lost) {
		t.Error(problem)
	}
	// Hand the fleet back before the harness tears the server down. Cancelling
	// a thousand agents and shutting the server down at the same time leaves it
	// draining sessions past its shutdown budget, which fails the run for a
	// reason that has nothing to do with what was measured.
	client.CloseIdleConnections()
	env.StopNodes(t)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	// WAL and shared-memory files are part of what the database costs on disk.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}
