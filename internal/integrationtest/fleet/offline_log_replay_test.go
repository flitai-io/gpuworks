package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestInstanceLogsWrittenWhileTheServerIsDownReachTheLogCenter restarts the
// server under an instance that keeps logging. Every line it wrote, including
// the ones written while the server was down and the node offline, must be
// searchable in the log center afterwards, with no gap (GWKS-258). Before
// the agent buffered unsent log batches, the same run lost about 120 lines:
// the 5 s outage plus the time until the node came back online.
func TestInstanceLogsWrittenWhileTheServerIsDownReachTheLogCenter(t *testing.T) {
	const perSec = 20
	env := testenv.New(t)
	node := env.AddNode("replay", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	node.Driver.ShareEnginePorts()
	var mu sync.Mutex
	written := 0
	node.Driver.OnLogs(func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
		if !follow {
			return io.NopCloser(strings.NewReader("")), nil, true
		}
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			tick := time.NewTicker(time.Second / perSec)
			defer tick.Stop()
			for range tick.C {
				mu.Lock()
				seq := written
				written++
				mu.Unlock()
				if _, err := fmt.Fprintf(pw, "INFO replay-seq %06d\n", seq); err != nil {
					return
				}
			}
		}()
		return pr, nil, true
	})
	env.WaitNodeOnline(t, node.Name)
	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := env.Store().NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "served_name": "replay", "quick_params": map[string]any{"max_model_len": 512}})
	if err := client.PostJSON("/api/v1/deployments", body, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitInstancesRunning(client, 1, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	instances, err := env.Store().ListInstances()
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %d, %v", len(instances), err)
	}
	instance := instances[0].UUID

	// Stop only once the instance's lines are reaching the log center, so the
	// case covers lines from before, during and after the outage.
	re := regexp.MustCompile(`replay-seq (\d+)`)
	search := func() (seen map[int]bool, first int, ok bool) {
		var page struct {
			Items []struct {
				Line string `json:"line"`
			} `json:"items"`
		}
		if err := client.GetJSON("/api/v1/logs/search?q=replay-seq&instance="+instance+"&limit=5000", &page); err != nil {
			return nil, -1, false
		}
		seen, first = map[int]bool{}, -1
		for _, it := range page.Items {
			if m := re.FindStringSubmatch(it.Line); m != nil {
				v, _ := strconv.Atoi(m[1])
				seen[v] = true
				if first < 0 || v < first {
					first = v
				}
			}
		}
		return seen, first, true
	}
	env.Eventually(t, 30*time.Second, func() bool {
		seen, _, ok := search()
		return ok && len(seen) >= perSec
	})
	env.StopServer(t)
	mu.Lock()
	atStop := written
	mu.Unlock()
	time.Sleep(5 * time.Second)
	mu.Lock()
	duringOutage := written - atStop
	mu.Unlock()
	env.RestartServer(t)
	env.WaitNodeOnline(t, node.Name)
	time.Sleep(3 * time.Second)
	mu.Lock()
	last := written - 1
	mu.Unlock()

	client, err = Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	var seen map[int]bool
	var first int
	env.Eventually(t, 10*time.Second, func() bool {
		var ok bool
		seen, first, ok = search()
		return ok && seen[last]
	})
	// The instance's first line is seq 0: every line it ever wrote must be there.
	var missing []int
	for seq := 0; seq <= last; seq++ {
		if !seen[seq] {
			missing = append(missing, seq)
		}
	}
	t.Logf("instance wrote 0..%d (earliest found %d), %d of them while the server was down; %d missing", last, first, duringOutage, len(missing))
	if duringOutage < perSec {
		t.Fatalf("only %d lines were written during the outage; the case did not exercise it", duringOutage)
	}
	if len(missing) > 0 {
		t.Fatalf("%d lines never reached the log center, from seq %d to %d", len(missing), missing[0], missing[len(missing)-1])
	}
}
