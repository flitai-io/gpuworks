package app_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The gateway, records and OTLP sender share the same assembled App across
// restarts. A fresh listener must not keep writing through closed adapters.
func TestRestartedAppRetainsRequestHistoryAndQuality(t *testing.T) {
	collector := &restartTraceCollector{}
	collectorServer := httptest.NewServer(collector)
	defer collectorServer.Close()
	env := testenv.NewWithPrivateHTTPHosts(t, "127.0.0.1")
	env.AddNode("audit-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "audit-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("fixture nodes=%d err=%v", len(nodes), err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "restart-model", Arch: "qwen3", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	api.Client.Timeout = 8 * time.Second
	exportBody, _ := json.Marshal(map[string]any{"enabled": true, "endpoint": collectorServer.URL, "sample_ratio": 1})
	exportReq, _ := http.NewRequest(http.MethodPut, env.URL()+"/api/v1/telemetry/export", strings.NewReader(string(exportBody)))
	exportReq.Header.Set("Content-Type", "application/json")
	exportResponse, err := api.Do(exportReq)
	if err != nil {
		t.Fatal(err)
	}
	exportReply, _ := io.ReadAll(exportResponse.Body)
	exportResponse.Body.Close()
	if exportResponse.StatusCode != http.StatusOK {
		t.Fatalf("configure traces = %d %s", exportResponse.StatusCode, exportReply)
	}
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "restart-model", "quick_params": map[string]any{"max_model_len": 512}})
	res, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var dep struct {
		ID uint `json:"id"`
	}
	if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &dep) != nil || dep.ID == 0 {
		t.Fatalf("fixture HTTP create deployment=%d %s", res.StatusCode, raw)
	}
	env.Eventually(t, 25*time.Second, func() bool {
		instances, _ := env.Store().InstancesForDeployment(dep.ID)
		for _, in := range instances {
			if in.State == proto.InstRunning {
				return true
			}
		}
		return false
	})
	key, err := env.Store().CreateAPIKey("restart-key", 0)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	chat := func() string {
		t.Helper()
		var status int
		var reply []byte
		var requestID string
		env.Eventually(t, 15*time.Second, func() bool {
			req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"restart-model","messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, callErr := client.Do(req)
			if callErr != nil {
				return false
			}
			status = resp.StatusCode
			requestID = resp.Header.Get("X-Request-ID")
			reply, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			return status == http.StatusOK
		})
		if requestID == "" || !json.Valid(reply) {
			t.Fatalf("chat response has no request ID or invalid JSON: %d %s", status, reply)
		}
		return requestID
	}
	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := api.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, raw
	}
	history := fmt.Sprintf("/api/v1/monitor/deployments/%d/requests?view=history&range=1h", dep.ID)
	ids := []string{}
	for round := 1; round <= 2; round++ {
		ids = append(ids, chat())
		// The next listener reads what the preceding Run's final flush wrote;
		// neither the UX ticker nor the five-minute stats flush is awaited.
		env.RestartServer(t)
		code, raw := get(history)
		var historyBody struct {
			Requests []struct {
				ID     string `json:"id"`
				Status int    `json:"status"`
				Result string `json:"result"`
			} `json:"requests"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &historyBody) != nil {
			t.Fatalf("round %d history after restart = %d %s", round, code, raw)
		}
		for _, id := range ids {
			found := false
			for _, request := range historyBody.Requests {
				if request.ID == id && request.Status == 200 && request.Result == "completed" {
					found = true
				}
			}
			if !found {
				t.Fatalf("round %d history lost completed request %s: %s", round, id, raw)
			}
		}
		now := time.Now()
		qualityCode, quality := get(fmt.Sprintf("/api/v1/analytics/quality?from=%d&to=%d&deployment=%d", now.Add(-time.Hour).UnixMilli(), now.Add(time.Minute).UnixMilli(), dep.ID))
		var qualityBody struct {
			Totals struct {
				Completed int `json:"completed"`
			} `json:"totals"`
		}
		if qualityCode != 200 || json.Unmarshal(quality, &qualityBody) != nil || qualityBody.Totals.Completed != round {
			t.Fatalf("round %d quality after restart = %d %s", round, qualityCode, quality)
		}
	}
	env.Eventually(t, 5*time.Second, func() bool {
		return collector.has(ids[0]) && collector.has(ids[1])
	})
}

// restartTraceCollector validates the actual OTLP wire object and records the
// request ids from its spans. The collector is otherwise a normal 200 receiver.
type restartTraceCollector struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (c *restartTraceCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" || r.Header.Get("Content-Encoding") != "gzip" {
		http.Error(w, "invalid OTLP request", http.StatusBadRequest)
		return
	}
	reader, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, "invalid gzip", http.StatusBadRequest)
		return
	}
	defer reader.Close()
	var document struct {
		Resources []struct {
			Scopes []struct {
				Spans []struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							Text string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if json.NewDecoder(reader).Decode(&document) != nil {
		http.Error(w, "invalid OTLP JSON", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if c.ids == nil {
		c.ids = map[string]bool{}
	}
	for _, resource := range document.Resources {
		for _, scope := range resource.Scopes {
			for _, span := range scope.Spans {
				for _, attribute := range span.Attributes {
					if attribute.Key == "gpuworks.request.id" {
						c.ids[attribute.Value.Text] = true
					}
				}
			}
		}
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
}

func (c *restartTraceCollector) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ids[id]
}
