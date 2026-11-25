package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

func TestInstanceLogsStreamFromAgentThroughHub(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("logs-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "logs-node")
	nodes, _ := env.Store().ListNodes()
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "logs-model", Revision: "main", Name: "logs-model", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "logs-model", "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%v err=%v", respStatus(resp), err)
	}
	resp.Body.Close()
	var inst store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		rows, _ := env.Store().ListInstances()
		if len(rows) != 1 || rows[0].State != proto.InstRunning {
			return false
		}
		inst = rows[0]
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.URL()+"/api/v1/instances/"+inst.UUID+"/logs", nil)
	req.Header.Set("Authorization", "Bearer "+api.Token)
	got, err := api.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != http.StatusOK || !strings.Contains(string(data), "Application startup complete") {
		t.Fatalf("logtail status=%d data=%q", got.StatusCode, data)
	}
	_ = node
}

func respStatus(r *http.Response) any {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprint(r.StatusCode)
}

func TestCollectorMetricsReachAuthenticatedAPI(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("metrics-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	node.Prober.SetMetrics([]gpu.Metric{{Index: 0, UUID: "metric-gpu-0", Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 73}}})
	env.WaitNodeOnline(t, "metrics-node")
	api := env.API(t)
	var last []apiwire.MetricSeries
	env.Eventually(t, 15*time.Second, func() bool {
		q := url.Values{}
		q.Set("metric", "gpuworks_gpu_utilization")
		q.Set("range", "1h")
		q.Set("step", "1s")
		q.Set("node", nodeUUID(t, env))
		q.Set("gpu", "0")
		resp, err := api.Get("/api/v1/metrics/query?" + q.Encode())
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return false
		}
		if err := apiwire.ReadList(resp.Body, &last); err != nil {
			t.Fatalf("decode /api/v1/metrics/query: %v", err)
		}
		return len(last) > 0 && len(last[0].Points) > 0 && last[0].Points[len(last[0].Points)-1].Value == 73
	})
}

func TestLogSearchKeepsInstanceIsolation(t *testing.T) {
	ls, err := logstore.OpenSQLite(t.TempDir() + "/logs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	now := time.Now().UnixMilli()
	if err := ls.Append([]logstore.Entry{
		{Instance: "inst-1", Node: "node-a", TimeMS: now, Line: "ERROR CUDA out of memory"},
		{Instance: "inst-2", Node: "node-a", TimeMS: now + 1, Line: "INFO serving"},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := ls.Search(logstore.Filter{Query: "CUDA out of memory", Limit: 10})
	if err != nil || len(hits.Entries) != 1 || hits.Entries[0].Instance != "inst-1" {
		t.Fatalf("OOM search = %+v err=%v", hits.Entries, err)
	}
	if scoped, _ := ls.Search(logstore.Filter{Query: "memory", Instance: "inst-2", Limit: 10}); len(scoped.Entries) != 0 {
		t.Fatalf("instance filter leaked hits: %+v", scoped.Entries)
	}
}

func nodeUUID(t *testing.T, env *testenv.Env) string {
	t.Helper()
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	return nodes[0].UUID
}
