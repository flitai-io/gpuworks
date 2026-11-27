package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const hostModelPath = "/Users/a/models/q"

// flaglessHostDesired is a desired state as a server stored it before launches
// on host-process nodes named their model: the engine is told its model path
// only, which the host agent rewrites to the mount's source.
const flaglessHostDesired = `{"launch":{"entrypoint":["vllm","serve"],"argv":["--model","/model","--port","30000"],"model_mount":"/model"},"model_path":"` + hostModelPath + `"}`

// hostEngine answers like vLLM: only under the name it registered (any of
// names, hostModelPath when empty), with vLLM's 404 for any other model, and
// echoing the name in the reply's model field, streamed or not.
type hostEngine struct {
	mu     sync.Mutex
	names  []string
	models []string
}

func (e *hostEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	model, _ := body["model"].(string)
	e.mu.Lock()
	e.models = append(e.models, model)
	names := e.names
	e.mu.Unlock()
	if len(names) == 0 {
		names = []string{hostModelPath}
	}
	known := false
	for _, name := range names {
		known = known || name == model
	}
	w.Header().Set("Content-Type", "application/json")
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"object":"error","message":"The model %s does not exist.","type":"NotFoundError","param":null,"code":404}`, "`"+model+"`")
		return
	}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, content := range []string{"h", "i"} {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-h\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", model, content)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-h","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, model)
}

func setRuntimeDriver(t *testing.T, st *store.Store, nodeID uint, driver string) {
	t.Helper()
	if err := st.DB().Model(&store.Node{}).Where("id = ?", nodeID).Update("runtime_driver", driver).Error; err != nil {
		t.Fatal(err)
	}
}

// A replica launched on a Mac before the served-name change answers under
// its host model path. Every way the gateway reaches it asks for that path:
// the route table, the uncached fallback, and a pinned playground request.
func TestGatewayReachesAFlaglessHostProcessEngine(t *testing.T) {
	for _, driver := range []string{proto.RuntimeDriverHost, "docker"} {
		t.Run(driver, func(t *testing.T) {
			engine := &hostEngine{}
			upstream := httptest.NewServer(engine)
			t.Cleanup(upstream.Close)
			st, err := store.Open("file:proxy_host_engine_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/q", Revision: "main", Name: "q", Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
			if err != nil {
				t.Fatal(err)
			}
			node, err := st.RegisterNode("node-mac", store.HashToken("node"), "node-mac")
			if err != nil {
				t.Fatal(err)
			}
			setRuntimeDriver(t, st, node.ID, driver)
			dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "qwen", Engine: "vllm", State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "mac-0", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, Ports: "[30000]", DesiredStateJSON: flaglessHostDesired}); err != nil {
				t.Fatal(err)
			}
			key, err := st.CreateAPIKey("host-engine", 0)
			if err != nil {
				t.Fatal(err)
			}
			p := New(storeplane.New(st), storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstream.URL, "http://")}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
			t.Cleanup(p.Close)

			want := hostModelPath
			if driver != proto.RuntimeDriverHost {
				want = "/model" // a container engine is asked for its mount
			}
			snapshot, err := p.routes.build()
			if err != nil {
				t.Fatal(err)
			}
			if rds := snapshot.byName["qwen"]; len(rds) != 1 || len(rds[0].candidates) != 1 || rds[0].candidates[0].modelName != want {
				t.Fatalf("route table candidates = %+v, want model %q", rds, want)
			}
			if tgt, rejection, _ := p.selectDeploymentInstances(dep, "qwen", "", admission{}); rejection != nil || tgt.modelName != want {
				t.Fatalf("uncached route = %+v %v, want model %q", tgt, rejection, want)
			}
			if driver != proto.RuntimeDriverHost {
				return
			}

			for _, entry := range []struct {
				handler http.HandlerFunc
				path    string
				pin     string
				stream  bool
			}{
				{p.Handler(), "/v1/chat/completions", "", false},
				{p.Handler(), "/v1/chat/completions", "", true},
				{p.PlaygroundHandler(nil), "/api/v1/playground/v1/chat/completions", "mac-0", false},
				{p.PlaygroundHandler(nil), "/api/v1/playground/v1/chat/completions", "mac-0", true},
			} {
				chat := fmt.Sprintf(`{"model":"qwen","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, entry.stream)
				req := httptest.NewRequest(http.MethodPost, entry.path, strings.NewReader(chat))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				if entry.pin != "" {
					req.Header.Set(instanceHeader, entry.pin)
				}
				rec := httptest.NewRecorder()
				entry.handler(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: status %d: %s (engine asked for %q)", entry.path, rec.Code, rec.Body.String(), engine.models)
				}
				assertPublicReplyModel(t, rec.Body.String(), "qwen", entry.stream)
			}
			for _, asked := range engine.models {
				if asked != hostModelPath {
					t.Fatalf("engine asked for %q, want %q", asked, hostModelPath)
				}
			}
		})
	}
}

// A PD prefill candidate on a host-process node carries its host model path
// too: the candidate goes through admit with the name already resolved.
func TestPDPrefillCandidateOnAHostProcessNodeCarriesItsHostModelPath(t *testing.T) {
	h := newPDHarness(t, 1, "")
	setRuntimeDriver(t, h.st, h.dep.NodeID, proto.RuntimeDriverHost)
	if err := h.st.UpdateInstanceDesiredState("prefill-0", flaglessHostDesired); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.proxy.routes.build()
	if err != nil {
		t.Fatal(err)
	}
	pd := snapshot.byName["pd"][0].pd
	if pd == nil || len(pd.prefill) != 1 || pd.prefill[0].modelName != hostModelPath {
		t.Fatalf("prefill candidates = %+v, want model %q", pd, hostModelPath)
	}
}

// assertPublicReplyModel checks that every reply object a client received,
// whole or streamed, names the deployment's public name and that the host
// model path, which carries the Mac account's user name, never reaches it.
func assertPublicReplyModel(t *testing.T, reply, public string, stream bool) {
	t.Helper()
	if strings.Contains(reply, hostModelPath) || strings.Contains(reply, "/Users/") {
		t.Fatalf("reply leaks the host model path: %s", reply)
	}
	var objects []string
	if stream {
		for _, line := range strings.Split(reply, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
				objects = append(objects, data)
			}
		}
	} else {
		objects = []string{reply}
	}
	if len(objects) == 0 {
		t.Fatalf("no reply objects in %q", reply)
	}
	for _, object := range objects {
		var decoded struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(object), &decoded); err != nil || decoded.Model != public {
			t.Fatalf("reply object %s: model %q (%v), want %q", object, decoded.Model, err, public)
		}
	}
}
