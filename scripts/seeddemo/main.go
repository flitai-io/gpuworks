// Command seeddemo inserts a realistic multi-GPU node + deployment into a
// gpuworks database so the topology graph and VRAM-breakdown UI can be exercised
// without real hardware. Dev tool only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func main() {
	db := flag.String("db", "./data/gpuworks.db", "sqlite path")
	flag.Parse()
	st, err := store.Open(*db, bus.New())
	if err != nil {
		log.Fatal(err)
	}
	if err := app.SyncInferenceCatalog(st); err != nil {
		log.Fatal(err)
	}
	// The node arrives the way an agent's does: registered, then described
	// by a status report whose topology is `nvidia-smi topo -m` output.
	// 8x H100, two NVLink cliques of 4 across two NUMA nodes.
	// The seed is for an empty database: rerunning it would add a second
	// deployment and instance next to the first.
	const nodeUUID = "demo-dgx-8xh100"
	if _, err := st.NodeByUUID(nodeUUID); err == nil {
		log.Fatalf("%s already holds the demo node; seed an empty database", *db)
	}
	// No agent connects as the demo node, so its token is random and unknown.
	if _, err := st.RegisterNode(nodeUUID, store.HashToken(auth.NewToken(32)), "dgx-a"); err != nil {
		log.Fatal(err)
	}
	var topo strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&topo, "\tGPU%d", i)
	}
	topo.WriteString("\tCPU Affinity\tNUMA Affinity\n")
	cards := make([]proto.GPUInfo, 0, 8)
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&topo, "GPU%d", i)
		for j := 0; j < 8; j++ {
			switch {
			case i == j:
				topo.WriteString("\t X ")
			case i/4 == j/4:
				topo.WriteString("\tNV18")
			default:
				topo.WriteString("\tSYS")
			}
		}
		fmt.Fprintf(&topo, "\t0-47\t%d\n", i/4)
		cards = append(cards, proto.GPUInfo{
			Index: i, UUID: "GPU-demo-" + string(rune('a'+i)), Name: "H100 80GB HBM3",
			ComputeCapability: "9.0", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, NUMANode: i / 4,
			Healthy: true, Ownership: accelerator.CardOwnedByNode,
		})
	}
	if err := st.UpdateNodeStatus(nodeUUID, proto.NodeStatus{
		Sys: proto.SysInfo{
			Hostname: "dgx-a", OS: "linux", Arch: "amd64", Kernel: "6.5.0", CPUModel: "AMD EPYC 9654", CPUCores: 96,
			MemTotalKB: 2 << 30, DiskTotalKB: 8 << 30, NvidiaDriver: "550.90.07",
		},
		GPUs:    cards,
		TopoRaw: topo.String(),
	}); err != nil {
		log.Fatal(err)
	}
	node, err := st.NodeByUUID(nodeUUID)
	if err != nil {
		log.Fatal(err)
	}

	cfg := `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":40,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":32768,"vocab_size":152064,"torch_dtype":"bfloat16"}`
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2.5-32B-Instruct", Revision: "master",
		Name: "Qwen2.5-32B-Instruct", Arch: "Qwen2ForCausalLM", WeightBytes: 65 << 30, Dtype: "bfloat16",
		Meta: cfg})
	if err != nil {
		log.Fatal(err)
	}

	dep, err := st.CreateDeployment(&store.Deployment{Name: "qwen32b", ModelID: model.ID, NodeID: node.ID, ServedName: "qwen2.5-32b",
		State: "active", Replicas: 1})
	if err != nil {
		log.Fatal(err)
	}

	explain := map[string]any{
		"explain": map[string]any{
			"chosen": map[string]any{"gpus": []int{0, 1, 2, 3}, "tp": 4, "per_gpu_bytes": int64(23) << 30, "score": 40,
				"reasons": []map[string]any{
					{"code": "topology", "message": "tp=4 on a fully NVLink-interconnected GPU group", "score": 30},
					{"code": "packing", "message": "binpack: prefer fewest GPUs", "score": -150},
					{"code": "tight_fit", "message": "capacity closely matches requirement", "score": 3},
				}},
			"topo_highlight": map[string]any{"gpus": []int{0, 1, 2, 3}, "numa": []int{0},
				"links": [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}},
			"rejected": []map[string]any{{"tp": 1, "reason": "need 71.8 GiB/GPU for tp=1, only 0 GPU(s) large enough"}},
		},
		"breakdown": map[string]any{
			"weight_bytes": int64(65) << 30, "kv_bytes_per_token": 131072.0, "kv_total_bytes": int64(16) << 30,
			"reserve_bytes": int64(1536) << 20,
			"per_gpu_bytes": int64(23) << 30, "total_bytes": int64(92) << 30, "grade": "exact", "tensor_parallel": 4, "max_model_len": 32768,
		},
	}
	explJSON, _ := json.Marshal(explain)
	measured, _ := json.Marshal(map[string]any{"ModelWeightsGiB": 16.1, "KVCacheGiB": 3.9, "KVCacheTokens": 262144,
		"GraphCaptureGiB": 0.58, "MaxConcurrency": 8.0, "NumGPUBlocks": 16384, "Found": true})
	desired, _ := json.Marshal(proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{
			CommandMode: string(deploy.CommandEngine),
			Entrypoint:  []string{"vllm", "serve", "/model"},
			Argv:        []string{"--served-model-name", "qwen2.5-32b", "--host", "0.0.0.0", "--port", "30000", "--tensor-parallel-size", "4", "--max-model-len", "32768"},
		},
		UUID: "demo-inst-1", DeploymentID: dep.ID, DesiredState: "running", Engine: "vllm",
		Image: "vllm/vllm-openai:latest", GPUIndexes: []int{0, 1, 2, 3}, Ports: []int{30000}, APIPort: 30000,
	})
	if _, err := st.CreateInstance(&store.Instance{UUID: "demo-inst-1", DeploymentID: dep.ID, NodeID: node.ID, State: "running",
		GPUIndexes: "[0,1,2,3]", Ports: "[30000]", APIPort: 30000, Image: "vllm/vllm-openai:latest",
		DesiredStateJSON: string(desired), ScheduleExpl: string(explJSON), Measured: string(measured), DirectAddr: "127.0.0.1:30000"}); err != nil {
		log.Fatal(err)
	}

	log.Println("seeded demo node + deployment into", *db)
}
