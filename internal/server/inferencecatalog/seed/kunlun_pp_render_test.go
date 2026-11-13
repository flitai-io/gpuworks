package seed

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// This checks the supported pipeline-parallel launches assembled from the
// importable P800 profile.
func TestKunlunP800ProfileRendersPipelineParallelTopologies(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "build", "inference-templates", "kunlun-p800", "catalog")
	var packed bytes.Buffer
	if err := inferencecatalog.PackPackage(&packed, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	archive, err := inferencecatalog.ReadPackageArchive(&packed)
	if err != nil {
		t.Fatal(err)
	}
	var spec inferencecatalog.InferenceSpec
	for _, model := range archive.Package.Catalog.Models {
		if model.ModelID == "qwen/qwen3-0.6b" {
			for _, candidate := range model.Specs {
				if candidate.Name == "bf16-vllm-kunlun-p800" {
					spec = candidate
				}
			}
		}
	}
	if spec.Name == "" {
		t.Fatal("importable P800 package lacks Qwen3-0.6B vLLM profile")
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, topology := range []struct {
		name          string
		tp, pp, nodes int
	}{
		{name: "one node two cards", tp: 1, pp: 2, nodes: 1},
		{name: "two nodes two cards each", tp: 2, pp: 2, nodes: 2},
	} {
		t.Run(topology.name, func(t *testing.T) {
			for rank := 0; rank < topology.nodes; rank++ {
				resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
					Count: 2, Vendor: "kunlunxin", MinMemoryGiB: 96,
				})
				if err != nil || resolved.Reason != "" {
					t.Fatalf("rank %d placement: result=%+v err=%v", rank, resolved, err)
				}
				launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
					Engine: engine,
					Params: deploy.Params{
						Vendor: "kunlunxin", ModelPath: "/model", APIPort: 30000, AssignedGPUs: 2,
						TensorParallel: topology.tp, PipelineParallel: topology.pp,
						NNodes: topology.nodes, NodeRank: rank, Headless: rank > 0,
						MasterAddr: "10.0.0.1", MasterPort: 29500,
						TemplateArgs: spec.Args, TemplateEnv: spec.Env,
					},
					Entrypoint: spec.Entrypoint, PreStartScript: spec.PreStartScript, PlacementVariables: resolved.Variables,
					APIContract: spec.API,
				})
				if err != nil {
					t.Fatalf("rank %d launch: %v", rank, err)
				}
				if launch.RequestedFeatures == nil || !launch.RequestedFeatures.CommandKnown || !launch.RequestedFeatures.PipelineParallel {
					t.Fatalf("catalog launch must expose recognizable PP engine argv: %+v", launch.RequestedFeatures)
				}
				command := append(append([]string{}, launch.Entrypoint...), launch.Argv...)
				for flag, want := range map[string]string{
					"--tensor-parallel-size":   strconv.Itoa(topology.tp),
					"--pipeline-parallel-size": strconv.Itoa(topology.pp),
				} {
					if got := commandFlagValue(command, flag); got != want || commandFlagCount(command, flag) != 1 {
						t.Errorf("rank %d %s = %q (%d occurrences), want %q once: %v", rank, flag, got, commandFlagCount(command, flag), want, command)
					}
				}
				if topology.nodes > 1 {
					for flag, want := range map[string]string{"--nnodes": "2", "--node-rank": strconv.Itoa(rank)} {
						if got := commandFlagValue(command, flag); got != want || commandFlagCount(command, flag) != 1 {
							t.Errorf("rank %d %s = %q, want %q once: %v", rank, flag, got, want, command)
						}
					}
				} else if commandFlagCount(command, "--nnodes") != 0 || commandFlagCount(command, "--node-rank") != 0 {
					t.Errorf("single-node launch contains distributed rank flags: %v", command)
				}
			}
		})
	}
}
