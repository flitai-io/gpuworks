package deploy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// dockerPullTranscript reproduces what `docker pull` writes to a non-TTY
// stream: one "Pulling fs layer" per layer, then interleaved Downloading and
// Extracting lines with a byte counter, then per-layer completions and the
// final digest/status pair.
//
// The line shapes matter. The agent reads layer identity with
// `^\s*(?:sha256:)?([0-9a-f]{8,64}):` and byte counters with a
// `<value><unit>/<value><unit>` pattern, so a fixture that summarized the
// output instead of reproducing it would exercise neither.
func dockerPullTranscript(layers, samplesPerLayer int) []string {
	lines := []string{"latest: Pulling from library/fake-engine"}
	digests := make([]string, layers)
	sizes := make([]float64, layers)
	for i := range digests {
		digests[i] = fmt.Sprintf("%012x", 0xa2abf6c4d29d+int64(i))
		sizes[i] = 27.09 + float64(i)
		lines = append(lines, digests[i]+": Pulling fs layer")
	}
	for i, digest := range digests {
		total := sizes[i]
		for sample := 1; sample <= samplesPerLayer; sample++ {
			done := total * float64(sample) / float64(samplesPerLayer)
			filled := strings.Repeat("=", sample*40/samplesPerLayer)
			lines = append(lines, fmt.Sprintf("%s: Downloading [%-50s]  %.2fMB/%.2fMB", digest, filled+">", done, total))
		}
		lines = append(lines, digest+": Verifying Checksum", digest+": Download complete")
	}
	for i, digest := range digests {
		total := sizes[i]
		for sample := 1; sample <= samplesPerLayer/4; sample++ {
			done := total * float64(sample) / float64(samplesPerLayer/4)
			lines = append(lines, fmt.Sprintf("%s: Extracting [%-50s]  %.2fMB/%.2fMB", digest, strings.Repeat("=", sample)+">", done, total))
		}
		lines = append(lines, digest+": Pull complete")
	}
	return append(lines,
		"Digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"Status: Downloaded newer image for docker.io/library/fake-engine:latest",
	)
}

// udockerPullTranscript reproduces the other dialect the agent has to read: the
// udocker prefetcher's per-blob byte counter and the storage-growth heartbeat.
func udockerPullTranscript(layers, samplesPerLayer int) []string {
	var lines []string
	for i := 0; i < layers; i++ {
		digest := fmt.Sprintf("%012x", 0xbe57f00d0000+int64(i))
		total := int64(33554432 + i*1024)
		for sample := 1; sample <= samplesPerLayer; sample++ {
			done := total * int64(sample) / int64(samplesPerLayer)
			lines = append(lines, fmt.Sprintf("sha256:%s: Downloading %d B/%d B", digest, done, total))
			if sample%8 == 0 {
				lines = append(lines, fmt.Sprintf("udocker storage: %d B / %d B", done, total))
			}
		}
	}
	return lines
}

// TestImagePullProgressDoesNotAmplifyIntoEventsOrStatusReports is the whole
// change stated as one number: a pull that prints thousands of lines must reach
// the control plane as a handful of milestones, not as one database row per
// line. The figures asserted are ratios against the replayed transcript and the
// observed duration, never wall-clock thresholds.
func TestImagePullProgressDoesNotAmplifyIntoEventsOrStatusReports(t *testing.T) {
	transcript := append(dockerPullTranscript(40, 100), udockerPullTranscript(8, 60)...)
	if len(transcript) < 5000 {
		t.Fatalf("fixture replays %d lines, want a realistic pull of at least 5000", len(transcript))
	}

	env := testenv.New(t)
	node := env.AddNode("amplify-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	node.Driver.OnPullImage(func(_ string, onLine func(string)) (error, bool) {
		for _, line := range transcript {
			onLine(line)
		}
		return nil, true
	})
	env.WaitNodeOnline(t, "amplify-node")

	// Count what actually reached the store. Every applied status report
	// publishes exactly one instance notification, so this is the report count
	// the server had to write, which is the quantity the limiter bounds.
	var reports atomic.Int64
	sub := env.Store().Bus.Subscribe(1<<16, "instance")
	defer sub.Close()
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-sub.C():
				reports.Add(1)
			case <-stop:
				// Drain whatever is still buffered so the count is complete.
				for {
					select {
					case <-sub.C():
						reports.Add(1)
					default:
						return
					}
				}
			}
		}
	}()

	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v err = %v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}

	client := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "fake", "quick_params": map[string]any{"max_model_len": 512}})
	started := time.Now()
	resp, err := client.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	var instance store.Instance
	env.Eventually(t, 60*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		if len(instances) != 1 {
			return false
		}
		instance = instances[0]
		return instance.State == proto.InstRunning
	})
	elapsed := time.Since(started)
	// Let any trailing flush land before counting.
	time.Sleep(2 * statusProgressWindow)
	close(stop)
	<-drained
	if coalesced := sub.CoalescedCount(); coalesced != 0 {
		t.Fatalf("%d notifications were coalesced, so the count is not exact", coalesced)
	}

	events, err := env.Store().InstanceEvents(instance.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	const eventBudget = 15
	if len(events) > eventBudget {
		var codes []string
		for _, e := range events {
			codes = append(codes, e.Code+"/"+e.Data)
		}
		t.Fatalf("a %d-line pull wrote %d event rows (budget %d):\n%s", len(transcript), len(events), eventBudget, strings.Join(codes, "\n"))
	}

	// One report per second per instance, plus one per milestone, plus the
	// final frame each milestone flushes ahead of itself.
	statusReports := int(reports.Load())
	budget := int(elapsed/statusProgressWindow) + 1 + 3*eventBudget
	if statusReports > budget {
		t.Fatalf("%d status reports reached the store in %s (budget %d for %d replayed lines)",
			statusReports, elapsed.Round(time.Millisecond), budget, len(transcript))
	}
	if statusReports == 0 {
		t.Fatal("no status report reached the store; the limiter is suppressing everything")
	}
	t.Logf("%d transcript lines -> %d status reports -> %d event rows in %s",
		len(transcript), statusReports, len(events), elapsed.Round(time.Millisecond))

	// The milestone that ends the pull carries the phase's final counters, so
	// the transfer reads as complete even though the columns have moved on.
	summary := findPhaseSummary(t, events, "image_pull")
	pulled, expected := summary["prev_bytes_done"], summary["prev_bytes_total"]
	if pulled == nil || expected == nil || pulled != expected {
		t.Fatalf("the milestone ending image_pull reports %v/%v bytes, want a completed transfer", pulled, expected)
	}

	assertTimelineIsFoldedAndNewestFirst(t, client, instance.UUID)
}

const statusProgressWindow = time.Second

func findPhaseSummary(t *testing.T, events []store.InstanceEvent, phase string) map[string]any {
	t.Helper()
	for _, e := range events {
		var data map[string]any
		if json.Unmarshal([]byte(e.Data), &data) != nil {
			continue
		}
		if data["prev_phase"] == phase {
			return data
		}
	}
	t.Fatalf("no milestone ended phase %q; the timeline never summarized it", phase)
	return nil
}

func assertTimelineIsFoldedAndNewestFirst(t *testing.T, client *testenv.APIClient, uuid string) {
	t.Helper()
	resp, err := client.Get("/api/v1/instances/" + uuid + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	var envelope apiwire.CursorPage[struct {
		ID        uint           `json:"id"`
		Code      string         `json:"code"`
		Params    map[string]any `json:"params"`
		CreatedAt time.Time      `json:"created_at"`
	}]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	page := envelope.Items
	if len(page) == 0 {
		t.Fatal("the events endpoint returned nothing for a running instance")
	}
	if page[0].Params["state"] != proto.InstRunning {
		t.Fatalf("the newest entry is %+v, want the running milestone first", page[0])
	}
	for i := 1; i < len(page); i++ {
		if page[i].ID >= page[i-1].ID {
			t.Fatalf("entries are not newest-first: %d then %d", page[i-1].ID, page[i].ID)
		}
		previous := fmt.Sprint(page[i].Code, page[i].Params["state"], page[i].Params["phase"])
		current := fmt.Sprint(page[i-1].Code, page[i-1].Params["state"], page[i-1].Params["phase"])
		if previous == current {
			t.Fatalf("entries %d and %d are the same milestone; the fold did not run", i-1, i)
		}
	}
}
