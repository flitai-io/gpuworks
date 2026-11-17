package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestFleetAIAssistantReads is the assistant's read cost at fleet size: with
// a fleet of nodes, a few pending ones and three instances per node, a
// scripted model asks the real /ai/ask route for the reads an operator's
// cluster-wide questions take - the overview, whole lists with and without
// jq, a node by name, the metric catalog - and the case holds what each tool
// call costs the server and how much of it reaches the model.
//
// Gated like the other fleet cases and run on an idle host with
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run TestFleetAIAssistantReads
//
// sized by GPUWORKS_FLEET_NODES and GPUWORKS_FLEET_INSTANCES_PER_NODE.
func TestFleetAIAssistantReads(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 1000)
	perNode := EnvInt("GPUWORKS_FLEET_INSTANCES_PER_NODE", 3)
	pendingCount := 40

	env := testenv.NewOnDisk(t)
	names := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("fleet-%04d", i)
		node := env.AddNode(name, testenv.WithGPUs(perNode+1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
		node.Driver.ShareEnginePorts()
		names = append(names, name)
	}
	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatalf("log in: %v", err)
	}
	start := time.Now()
	if _, err := WaitFleetOnline(client, nodeCount, 10*time.Minute); err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("%d nodes online in %s", nodeCount, time.Since(start).Round(time.Millisecond))
	// Pending nodes put rows in the overview's attention list, as a fleet
	// being onboarded has.
	for i := 0; i < pendingCount; i++ {
		env.CreatePendingNode(t, fmt.Sprintf("fleet-pending-%02d", i))
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
	for _, name := range names {
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: byName[name].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
			t.Fatal(err)
		}
	}
	start = time.Now()
	deploys := make(chan string)
	var deployGroup sync.WaitGroup
	for w := 0; w < 32; w++ {
		deployGroup.Add(1)
		go func() {
			defer deployGroup.Done()
			for nodeUUID := range deploys {
				body, _ := json.Marshal(map[string]any{
					"model_id": model.ID, "node_uuid": nodeUUID, "served_name": "fleet-" + nodeUUID,
					"quick_params": map[string]any{"max_model_len": 512},
				})
				if err := client.PostJSON("/api/v1/deployments", body, nil); err != nil {
					t.Errorf("deploy %s: %v", nodeUUID, err)
				}
			}
		}()
	}
	for _, name := range names {
		for i := 0; i < perNode; i++ {
			deploys <- byName[name].UUID
		}
	}
	close(deploys)
	deployGroup.Wait()
	want := nodeCount * perNode
	if running, err := WaitInstancesRunning(client, want, 15*time.Minute); err != nil {
		t.Fatalf("%v", err)
	} else {
		t.Logf("%d instances running in %s", running, time.Since(start).Round(time.Millisecond))
	}

	// What the UI reads in one go, for scale: the size the tools shape down.
	for _, path := range []string{"/api/v1/nodes", "/api/v1/deployments", "/api/v1/instances", "/api/v1/gpus", "/api/v1/monitor/metrics"} {
		took, size, err := timedRead(client, path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		t.Logf("raw GET %-26s %8.1f KiB %6s", path, float64(size)/1024, took.Round(time.Millisecond))
	}

	llm := &fleetLLM{scripts: [][]scriptedCall{
		{
			{"cluster_overview", `{}`},
			{"call_api", `{"path":"/api/v1/nodes"}`},
			{"call_api", `{"path":"/api/v1/nodes","jq":".items | group_by(.state) | map({state: .[0].state, n: length})"}`},
			{"call_api", `{"path":"/api/v1/nodes","query":{"state":"connecting"},"jq":"[.items[] | {name, state}]","limit":200}`},
			{"call_api", `{"path":"/api/v1/nodes/` + names[nodeCount/2] + `"}`},
			{"call_api", `{"path":"/api/v1/nodes/` + names[nodeCount-1] + `/replicas","jq":"[.items[] | {deployment_id, state}]"}`},
		},
		{
			{"call_api", `{"path":"/api/v1/deployments","jq":".items | group_by(.runtime_state) | map({state: .[0].runtime_state, n: length})"}`},
			{"call_api", `{"path":"/api/v1/deployments","jq":"[.items[] | {id, name, runtime_state, node_names}]","limit":200}`},
			{"call_api", `{"path":"/api/v1/instances","jq":"[.items[] | select(.state != \"running\")] | length"}`},
			{"call_api", `{"path":"/api/v1/gpus","jq":".overview"}`},
			{"search_metrics", `{"query":"gpu utilization"}`},
			{"search_api", `{"query":"instances of a deployment"}`},
		},
	}}
	srv := httptest.NewServer(llm)
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{"name": "fleet", "base_url": srv.URL, "api_key": "sk-fleet", "model": "m", "is_default": true})
	if err := client.PostJSON("/api/v1/ai/configs", body, nil); err != nil {
		t.Fatalf("configure the scripted model: %v", err)
	}

	var problems []string
	for i, script := range llm.scripts {
		body, _ := json.Marshal(map[string]any{"task": aihelper.TaskAsk, "question": "script " + strconv.Itoa(i)})
		var answer struct {
			Steps []struct {
				Tool  string `json:"tool"`
				Args  string `json:"args"`
				MS    int64  `json:"ms"`
				Error string `json:"error"`
			} `json:"steps"`
		}
		asked := time.Now()
		if err := client.PostJSON("/api/v1/ai/ask", body, &answer); err != nil {
			t.Fatalf("ask script %d: %v", i, err)
		}
		t.Logf("ask script %d: %d steps in %s", i, len(answer.Steps), time.Since(asked).Round(time.Millisecond))
		if len(answer.Steps) != len(script) {
			t.Errorf("script %d ran %d of %d tool calls", i, len(answer.Steps), len(script))
		}
		for j, step := range answer.Steps {
			reply := llm.reply(i, j)
			t.Logf("  %-16s %6d ms %6d chars  %s", step.Tool, step.MS, len(reply), clipArgs(step.Args))
			if step.Error != "" {
				problems = append(problems, fmt.Sprintf("%s %s: %s", step.Tool, step.Args, step.Error))
			}
			if step.MS > aiReadBudget.Milliseconds() {
				problems = append(problems, fmt.Sprintf("%s %s took %d ms, over the %s budget", step.Tool, step.Args, step.MS, aiReadBudget))
			}
			if len(reply) > 12000 {
				problems = append(problems, fmt.Sprintf("%s %s handed the model %d chars, over the 12000 cap", step.Tool, step.Args, len(reply)))
			}
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
	client.CloseIdleConnections()
	env.StopNodes(t)
}

// aiReadBudget is what one tool call may cost the server at fleet size: an
// ask takes several, and the model's own turns already take seconds.
const aiReadBudget = 2 * time.Second

func timedRead(c *Client, path string) (time.Duration, int, error) {
	start := time.Now()
	resp, err := c.Get(path)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return time.Since(start), int(n), err
}

func clipArgs(s string) string {
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

type scriptedCall struct{ tool, args string }

// fleetLLM plays each ask's script of tool calls, one per turn, then
// answers; it keeps every tool reply the server handed it.
type fleetLLM struct {
	scripts [][]scriptedCall
	mu      sync.Mutex
	replies map[[2]int]string
}

var scriptQuestion = regexp.MustCompile(`script (\d+)`)

func (l *fleetLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req aihelper.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.URL.Path != "/chat/completions" {
		http.Error(w, "unexpected", http.StatusBadRequest)
		return
	}
	script, tools := -1, 0
	for _, m := range req.Messages {
		if m.Role == "user" {
			if found := scriptQuestion.FindStringSubmatch(m.Content); found != nil {
				script, _ = strconv.Atoi(found[1])
			}
		}
		if m.Role == "tool" {
			l.mu.Lock()
			if l.replies == nil {
				l.replies = map[[2]int]string{}
			}
			l.replies[[2]int{script, tools}] = m.Content
			l.mu.Unlock()
			tools++
		}
	}
	if script < 0 || script >= len(l.scripts) {
		http.Error(w, "no script", http.StatusBadRequest)
		return
	}
	if tools >= len(l.scripts[script]) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		return
	}
	call := l.scripts[script][tools]
	args, _ := json.Marshal(call.args)
	fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, tools, call.tool, args)
}

func (l *fleetLLM) reply(script, step int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replies[[2]int{script, step}]
}
