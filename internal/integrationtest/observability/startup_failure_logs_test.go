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

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A process that fails during model inspection can exit before the periodic
// log follower starts. Exercise the actual drain, agent transport, persisted
// store and authenticated history endpoint after the container is removed.
func TestStartupFailureLogsRemainSearchableAfterContainerRemoval(t *testing.T) {
	env := testenv.New(t)
	env.SetControllerRetryPolicy(time.Hour, 1)
	driver := testenv.NewFakeDriver("docker")
	diagnostic := "ValueError: Model architectures ['Qwen3ForCausalLM'] failed to be inspected"
	driver.AfterRun(func(spec runtime.WorkloadSpec) { driver.Exit(spec.Name, 1) })
	driver.OnLogs(func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
		status, err := driver.Status(context.Background(), id)
		if err != nil || !status.Exists {
			return nil, fmt.Errorf("container %s no longer exists", id), true
		}
		return io.NopCloser(strings.NewReader("Traceback: model registry inspection\n" + diagnostic + "\n")), nil, true
	})
	env.AddNode("startup-log-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(driver))
	env.WaitNodeOnline(t, "startup-log-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/startup-failure", Name: "startup-failure", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d", resp.StatusCode)
	}
	var failed store.Instance
	env.Eventually(t, 15*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		if len(instances) != 1 || instances[0].State != proto.InstFailed {
			return false
		}
		failed = instances[0]
		containers, err := driver.ListContainers(context.Background())
		return err == nil && len(containers) == 0
	})
	if !strings.Contains(failed.Message, diagnostic) {
		t.Fatalf("startup failure lost diagnostic: %q", failed.Message)
	}
	env.Eventually(t, 10*time.Second, func() bool {
		resp, err := api.Get("/api/v1/logs/search?instance=" + url.QueryEscape(failed.UUID))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("log history status=%d", resp.StatusCode)
		}
		var page struct {
			Items []struct {
				Instance string `json:"instance"`
				Line     string `json:"line"`
			} `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Items {
			if entry.Instance != failed.UUID {
				t.Fatalf("history returned another instance: %+v", entry)
			}
			if entry.Line == diagnostic {
				return true
			}
		}
		return false
	})
}
