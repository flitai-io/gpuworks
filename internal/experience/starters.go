package experience

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

// starter is a deployment a new environment opens with, so monitoring,
// usage and evaluation show a platform in use before the visitor deploys
// anything. Together they hold about 70% of the cards, the way a busy
// cluster runs, and leave room to deploy.
type starter struct {
	Name, Repo, Vendor, Engine string
	// Node pins the replicas to one node; empty lets the scheduler spread
	// them over the vendor's nodes. Card pins a single replica to a card.
	Node     string
	Card     *int
	TP       int
	Replicas int
	// VRAMGiB slices the card (dGPU); zero takes whole cards.
	VRAMGiB int
	// Days is how long ago the deployment went live, for its history; Share
	// its part of the platform's requests; TTFT and TPOT (ms) its latency at
	// rest.
	Days       int
	Share      float64
	TTFT, TPOT float64
}

func card(i int) *int { return &i }

var starters = []starter{
	// The small model on a dGPU slice of one A100: the chat assistant.
	{Name: "qwen3-8b-chat", Repo: "Qwen/Qwen3-8B", Vendor: "nvidia", Engine: engines.VLLM, Node: "gpu-a100-01", Card: card(0), TP: 1, Replicas: 1, VRAMGiB: 24,
		Days: 30, Share: 0.14, TTFT: 240, TPOT: 13},
	// The main model, three TP2 replicas on the H100 server.
	{Name: "qwen3-32b", Repo: "Qwen/Qwen3-32B", Vendor: "nvidia", Engine: engines.VLLM, Node: "gpu-h100-01", TP: 2, Replicas: 3,
		Days: 30, Share: 0.26, TTFT: 360, TPOT: 19},
	// Customer service on the RTX 4090s, one replica per card.
	{Name: "qwen3-8b-service", Repo: "Qwen/Qwen3-8B", Vendor: "nvidia", Engine: engines.VLLM, Node: "gpu-4090-01", TP: 1, Replicas: 4,
		Days: 30, Share: 0.20, TTFT: 280, TPOT: 15},
	// Knowledge base Q&A, added later on the A100s.
	{Name: "qwen3-32b-rag", Repo: "Qwen/Qwen3-32B", Vendor: "nvidia", Engine: engines.VLLM, Node: "gpu-a100-01", TP: 2, Replicas: 2,
		Days: 16, Share: 0.14, TTFT: 430, TPOT: 23},
	// The domestic PPU servers, replicas spread over both.
	{Name: "qwen3.8-27b", Repo: "Qwen/Qwen3.8-27B", Vendor: "ppu", Engine: engines.VLLM, TP: 2, Replicas: 6,
		Days: 30, Share: 0.18, TTFT: 470, TPOT: 26},
	// The Ascend server, brought in most recently, on SGLang.
	{Name: "qwen3-32b-ascend", Repo: "Qwen/Qwen3-32B", Vendor: "ascend", Engine: engines.SGLang, Node: "npu-910b-01", TP: 2, Replicas: 2,
		Days: 9, Share: 0.08, TTFT: 520, TPOT: 29},
}

// The sample batch and evaluation run on this chat deployment.
var sampleDeployment = starters[0]

// createStarters deploys each starter with the catalog's own spec for its
// vendor and engine, as the deploy form would.
func createStarters(ctx context.Context, c *apiClient, members map[string]member) error {
	var models struct {
		Items []struct {
			ID       uint   `json:"id"`
			SourceID string `json:"source_id"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/models", &models); err != nil {
		return err
	}
	ids := map[string]uint{}
	for _, m := range models.Items {
		ids[strings.ToLower(m.SourceID)] = m.ID
	}
	for _, st := range starters {
		image, err := catalogImage(st.Repo, st.Engine, st.Vendor)
		if err != nil {
			return err
		}
		modelID := ids[strings.ToLower(st.Repo)]
		if modelID == 0 {
			return fmt.Errorf("%s is not registered", st.Repo)
		}
		params := map[string]any{}
		if st.Engine == engines.VLLM {
			params = map[string]any{
				"dtype": "auto", "gpu_memory_utilization": 0.9, "kv_cache_dtype": "auto", "max_model_len": 32768,
				"enable_request_metrics": true, "enable_cache_report": true, "performance_mode": "balanced",
			}
		}
		body := map[string]any{
			"name": st.Name, "served_name": st.Name, "model_id": modelID,
			"engine": st.Engine, "image": image, "target_vendor": st.Vendor, "nnodes": 1, "replicas": st.Replicas,
			"model_source": map[string]any{"provider": "huggingface"}, "quick_params": params,
		}
		switch {
		case st.Card != nil:
			body["node_uuid"] = members[st.Node].UUID
			body["placement"] = map[string]any{"gpus": []int{*st.Card}, "tp": st.TP}
			body["placement_mode"] = "manual"
		case st.Node != "":
			body["node_uuid"] = members[st.Node].UUID
			body["placement"] = map[string]any{"tp": st.TP}
			body["placement_mode"], body["placement_strategy"] = "auto", "binpack"
			body["node_requirements"] = map[string][]string{"gpuworks.ai/node": {members[st.Node].UUID}}
		default:
			// The form names the node it evaluated first; the scheduler places
			// each replica on any node of the vendor that fits.
			for _, n := range scenario {
				if n.Vendor == st.Vendor {
					body["node_uuid"] = members[n.Hostname].UUID
					break
				}
			}
			body["placement"] = map[string]any{"tp": st.TP}
			body["placement_mode"], body["placement_strategy"] = "auto", "spread"
			body["node_requirements"] = map[string][]string{"gpuworks.ai/vendor": {st.Vendor}}
		}
		if st.VRAMGiB > 0 {
			body["vram_limit_gib"] = st.VRAMGiB
		}
		if err := c.post(ctx, "/api/v1/deployments", body, nil); err != nil {
			return fmt.Errorf("deploy %s: %w", st.Name, err)
		}
	}
	return nil
}

// catalogImage is the image of the catalog's spec for repo on engine and
// vendor.
func catalogImage(repo, engine, vendor string) (string, error) {
	catalog, err := seed.Catalog()
	if err != nil {
		return "", err
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.Engine != engine || spec.GPUs.Vendor != vendor {
				continue
			}
			if vendor == "ascend" && !strings.Contains(spec.Image, "910b") {
				continue // the A3 image is for the other Ascend generation
			}
			for _, source := range spec.Weights.Sources {
				if strings.EqualFold(source.RepoID, repo) {
					return spec.Image, nil
				}
			}
		}
	}
	return "", fmt.Errorf("the catalog has no %s %s spec for %s", vendor, engine, repo)
}

// deployment is a running deployment, by the console's list.
type deployment struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	ServedName   string `json:"served_name"`
	RuntimeState string `json:"runtime_state"`
}

func listDeployments(ctx context.Context, c *apiClient) ([]deployment, error) {
	var list struct {
		Items []deployment `json:"items"`
	}
	err := c.get(ctx, "/api/v1/deployments", &list)
	return list.Items, err
}

// waitRunning returns the named deployment once it is running.
func waitRunning(ctx context.Context, c *apiClient, name string) (deployment, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if list, err := listDeployments(ctx, c); err == nil {
			for _, d := range list {
				if d.Name == name && d.RuntimeState == "running" {
					return d, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return deployment{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return deployment{}, fmt.Errorf("deployment %s is not running", name)
}
