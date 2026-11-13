package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestNoisyNodeCtrlLatency measures what a node's own instance log volume
// costs its control plane. Log and metric batches are envelopes on the ctrl
// stream, the same stream and agent write lock as RPC responses, so a node
// whose engine logs heavily may answer RPCs late. Two nodes sit behind
// identical private shaped links; one runs an instance that logs as fast as
// the agent ships, the other a quiet one. The case reports the RPC round-trip
// distribution of each.
//
// Measured on 2026-11-13 (40 ms RTT): at 100 Mbps the noisy node's p50 was
// 44 ms against 42 ms; at 10 Mbps 144 ms against 45 ms; at 2 Mbps 953 ms
// against 57 ms. The added delay tracks the ctrl lane's fixed stream window
// divided by link bandwidth: an RPC response queues behind at most one
// window of log envelopes on the same stream. The case fails when the noisy
// node exceeds that bound, which would mean log volume can delay RPCs without
// limit.
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 20m ./internal/integrationtest/fleet/ -run '^TestNoisyNodeCtrlLatency$'
//
// GPUWORKS_CTRL_LINK_MBPS / GPUWORKS_CTRL_LINK_RTT_MS / GPUWORKS_CTRL_SAMPLES
// shape it.
func TestNoisyNodeCtrlLatency(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("timing measurement; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	linkCfg := testenv.ShapedLinkConfig{
		RateMbps:   float64(EnvInt("GPUWORKS_CTRL_LINK_MBPS", 10)),
		RTT:        time.Duration(EnvInt("GPUWORKS_CTRL_LINK_RTT_MS", 40)) * time.Millisecond,
		QueueBytes: 4 << 20,
	}
	samples := EnvInt("GPUWORKS_CTRL_SAMPLES", 40)

	env := testenv.New(t)
	addNode := func(name string, logs func(id string, follow bool, tail int) (io.ReadCloser, error, bool)) *testenv.Node {
		link := env.NewShapedLink(t, linkCfg)
		node := env.AddNode(name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"), testenv.WithShapedLink(link))
		node.Driver.ShareEnginePorts()
		node.Driver.OnLogs(logs)
		return node
	}
	quiet := addNode("quiet", func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
		return newRateLogStream(1, id, follow, tail), nil, true
	})
	noisy := addNode("noisy", func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
		if !follow {
			return newRateLogStream(1, id, follow, tail), nil, true
		}
		return newFloodLogStream(id), nil, true
	})
	env.WaitNodeOnline(t, quiet.Name)
	env.WaitNodeOnline(t, noisy.Name)

	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatalf("log in: %v", err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*testenv.Node{quiet, noisy} {
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{
			"model_id": model.ID, "node_uuid": node.UUID, "served_name": "ctrl-" + node.Name,
			"quick_params": map[string]any{"max_model_len": 512},
		})
		if err := client.PostJSON("/api/v1/deployments", body, nil); err != nil {
			t.Fatalf("deploy on %s: %v", node.Name, err)
		}
	}
	if _, err := WaitInstancesRunning(client, 2, 3*time.Minute); err != nil {
		t.Fatalf("%v", err)
	}
	// Let the flood reach steady state: the link queue and the ctrl window fill.
	time.Sleep(5 * time.Second)

	measure := func(node *testenv.Node) []time.Duration {
		out := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			start := time.Now()
			_, err := env.Hub().CallNode(ctx, node.UUID, proto.RPCNodeConnectionDiagnostics, struct{}{})
			elapsed := time.Since(start)
			cancel()
			if err != nil {
				t.Fatalf("%s rpc %d: %v", node.Name, i, err)
			}
			out = append(out, elapsed)
			time.Sleep(200 * time.Millisecond)
		}
		return out
	}
	quietRTT, noisyRTT := measure(quiet), measure(noisy)
	t.Logf("link: %.0f Mbps, %s RTT, private per node", linkCfg.RateMbps, linkCfg.RTT)
	t.Logf("quiet node rpc: %s", summarize(quietRTT))
	t.Logf("noisy node rpc: %s", summarize(noisyRTT))

	// ctrlLaneWindow mirrors hub/conn ctrlLaneWindow (unexported there).
	const ctrlLaneWindow = 256 << 10
	windowDrain := time.Duration(float64(ctrlLaneWindow*8) / (linkCfg.RateMbps * 1e6) * float64(time.Second))
	bound := maxDuration(quietRTT) + windowDrain*3/2 + 100*time.Millisecond
	if got := maxDuration(noisyRTT); got > bound {
		t.Fatalf("noisy node rpc max %s exceeds quiet max + 1.5 ctrl windows of link time (%s): log volume delays RPCs beyond the window bound", got.Round(time.Millisecond), bound.Round(time.Millisecond))
	}
}

func maxDuration(d []time.Duration) time.Duration {
	var out time.Duration
	for _, v := range d {
		out = max(out, v)
	}
	return out
}

// floodLogStream is engine output with no pacing: the follower reads as fast
// as the agent ships, so the ctrl stream is the bottleneck.
type floodLogStream struct {
	pr *io.PipeReader
}

func newFloodLogStream(container string) *floodLogStream {
	pr, pw := io.Pipe()
	pad := strings.Repeat("x", 900)
	go func() {
		defer func() { _ = pw.Close() }()
		for i := 0; ; i++ {
			line := fmt.Sprintf("%s %s %d\n", syntheticInstanceLine(i, container), pad, i)
			if _, err := pw.Write([]byte(line)); err != nil {
				return
			}
		}
	}()
	return &floodLogStream{pr: pr}
}

func (s *floodLogStream) Read(p []byte) (int, error) { return s.pr.Read(p) }
func (s *floodLogStream) Close() error               { return s.pr.Close() }

func summarize(d []time.Duration) string {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(q float64) time.Duration { return sorted[int(q*float64(len(sorted)-1))].Round(time.Millisecond) }
	return fmt.Sprintf("n=%d p50=%s p90=%s p99=%s max=%s", len(sorted), pick(0.5), pick(0.9), pick(0.99), sorted[len(sorted)-1].Round(time.Millisecond))
}
