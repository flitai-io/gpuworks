package aiassistant

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Secrets planted where an administrator's platform keeps them; none may
// reach the model through any route the assistant reads, as an administrator
// reads it.
var planted = map[string]string{
	"deployment env HF_TOKEN":              "hf_plantedHFtoken0001",
	"deployment env AWS_SECRET_ACCESS_KEY": "plantedAWSsecret0002",
	"deployment env DB_PASSWORD":           "plantedDBpass0003",
	"extra args --api-key":                 "sk-plantedEngineKey0004",
	"storage mount option password=":       "plantedMountPass0005",
	"model source URL userinfo":            "plantedURLpass0006",
	"AI endpoint key":                      "sk-plantedAIkey0007",
}

// TestAssistantReadsNoPlantedSecret walks every GET route the assistant may
// read, with a node, a model, a deployment, its instance and a storage to
// fill the path parameters, through the real /ai/ask route and call_api,
// and holds that no planted secret - nor the node's agent token - is in what
// the model is handed.
func TestAssistantReadsNoPlantedSecret(t *testing.T) {
	env := testenv.NewWithPrivateHTTPHosts(t, "127.0.0.1")
	env.AddNode("secrets-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "secrets-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %v %v", nodes, err)
	}
	node := nodes[0]
	secrets := map[string]string{"node agent token": node.Token}
	for what, v := range planted {
		secrets[what] = v
	}

	model, err := env.Store().UpsertModel(&store.Model{
		Source: "url", SourceID: "secrets-model", Revision: "main", Name: "secrets-model", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		SourceURL: "https://loader:" + planted["model source URL userinfo"] + "@weights.example.com/secrets-model/",
		Meta:      `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	storage, err := env.Store().CreateStorage(&store.Storage{
		Name: "secrets-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/secrets-nfs", Export: "nfs.example.com:/models",
		MountOptions: "vers=4.1,username=loader,password=" + planted["storage mount option password="],
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := env.API(t)
	post := func(path string, body any) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp, err := client.PostJSON(path, raw)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, out)
		}
		return out
	}
	var deployment struct {
		ID uint `json:"id"`
	}
	_ = json.Unmarshal(post("/api/v1/deployments", map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "served_name": "secrets", "quick_params": map[string]any{"max_model_len": 512},
		"extra_args": "--api-key " + planted["extra args --api-key"],
		"container_overrides": map[string]any{"env": map[string]string{
			"HF_TOKEN": planted["deployment env HF_TOKEN"], "AWS_SECRET_ACCESS_KEY": planted["deployment env AWS_SECRET_ACCESS_KEY"],
			"DB_PASSWORD": planted["deployment env DB_PASSWORD"], "LOG_LEVEL": "info",
		}},
	}), &deployment)
	var instance store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		rows, _ := env.Store().ListInstances()
		if len(rows) != 1 || rows[0].State != proto.InstRunning {
			return false
		}
		instance = rows[0]
		return true
	})

	llm := &routeReadingLLM{}
	srv := httptest.NewServer(llm)
	defer srv.Close()
	post("/api/v1/ai/configs", map[string]any{"name": "secrets", "base_url": srv.URL, "api_key": planted["AI endpoint key"], "model": "m", "is_default": true})

	params := map[string]string{
		"/api/v1/nodes/{uuid}":        node.UUID,
		"/api/v1/instances/{uuid}":    instance.UUID,
		"/api/v1/deployments/{id}":    strconv.FormatUint(uint64(deployment.ID), 10),
		"/api/v1/models/{id}":         strconv.FormatUint(uint64(model.ID), 10),
		"/api/v1/storages/{id}":       strconv.FormatUint(uint64(storage.ID), 10),
		"/api/v1/ai/instances/{uuid}": instance.UUID,
		"/api/v1/ai/models/{id}":      strconv.FormatUint(uint64(model.ID), 10),
	}
	paths, skipped := readablePaths(t, params)
	t.Logf("reading %d routes; %d need parameters this fixture does not have: %s", len(paths), len(skipped), strings.Join(skipped, " "))
	for _, must := range []string{"/api/v1/deployments/" + params["/api/v1/deployments/{id}"], "/api/v1/instances/" + instance.UUID + "/launch-facts", "/api/v1/storages", "/api/v1/models/" + params["/api/v1/models/{id}"], "/api/v1/nodes/" + node.UUID} {
		if !contains(paths, must) {
			t.Fatalf("the walk misses %s", must)
		}
	}

	const perAsk = 8
	for start := 0; start < len(paths); start += perAsk {
		batch := paths[start:min(start+perAsk, len(paths))]
		llm.set(batch)
		post("/api/v1/ai/ask", map[string]any{"task": aihelper.TaskAsk, "question": "read the routes"})
		replies := llm.take()
		if len(replies) != len(batch) {
			t.Fatalf("ask read %d of %d routes: %v", len(replies), len(batch), batch)
		}
		for i, reply := range replies {
			for what, secret := range secrets {
				if strings.Contains(reply, secret) {
					t.Errorf("GET %s hands the model the %s", batch[i], what)
				}
			}
		}
	}
	env.StopNodes(t)
}

// readablePaths turns each GET route the assistant may read into a concrete
// path, filling its parameters from the fixture by the route's prefix.
func readablePaths(t *testing.T, params map[string]string) (paths, skipped []string) {
	routes, ok := api.Router(api.Deps{}).(chi.Routes)
	if !ok {
		t.Fatal("Router is not a chi.Routes")
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	_ = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method != http.MethodGet || !aihelper.Readable(route) {
			return nil
		}
		path := route
		for prefix, value := range params {
			if strings.HasPrefix(path, prefix) {
				path = value + "\x00" + strings.TrimPrefix(path, prefix)
				path = strings.TrimSuffix(prefix, prefix[strings.LastIndex(prefix, "/"):]) + "/" + strings.Replace(path, "\x00", "", 1)
				break
			}
		}
		if param.MatchString(path) {
			skipped = append(skipped, route)
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, skipped
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// routeReadingLLM calls call_api on each path of its batch, one per turn,
// with the widest limit, and keeps what each call handed it.
type routeReadingLLM struct {
	mu      sync.Mutex
	batch   []string
	replies []string
}

func (l *routeReadingLLM) set(batch []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batch, l.replies = batch, nil
}

func (l *routeReadingLLM) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replies
}

func (l *routeReadingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req aihelper.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var tools []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			tools = append(tools, m.Content)
		}
	}
	l.mu.Lock()
	l.replies = tools
	batch := l.batch
	l.mu.Unlock()
	if len(tools) >= len(batch) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		return
	}
	args, _ := json.Marshal(fmt.Sprintf(`{"path":%q,"limit":200}`, batch[len(tools)]))
	fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c%d","type":"function","function":{"name":"call_api","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, len(tools), args)
}
