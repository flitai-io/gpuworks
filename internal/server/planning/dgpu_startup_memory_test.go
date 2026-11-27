package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDGPUStartupMemoryWarning(t *testing.T) {
	const engine = "exec python -m sglang.launch_server --dtype bfloat16 "
	for _, tc := range []struct {
		name   string
		script string
		change func(*LaunchRequest, *LaunchPlan)
		warn   bool
	}{
		{name: "renamed template", change: func(req *LaunchRequest, _ *LaunchPlan) {
			req.Selection.SpecName = "renamed-ascend-profile"
		}, warn: true},
		{name: "other model", change: func(req *LaunchRequest, _ *LaunchPlan) { req.Selection.ModelID = "qwen/qwen3-1.7b" }},
		{name: "other runner", change: func(req *LaunchRequest, _ *LaunchPlan) {
			req.Source.Image = "gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r3"
		}},
		{name: "unverified quota", change: func(_ *LaunchRequest, plan *LaunchPlan) { plan.Params.VRAMLimitBytes = 7 << 30 }},
		{name: "multi-node", change: func(_ *LaunchRequest, plan *LaunchPlan) { plan.Params.NNodes = 2 }},
		{name: "PD", change: func(req *LaunchRequest, _ *LaunchPlan) { req.PD = new(deploy.PDLaunch) }},
		{name: "bash assignment", script: engine + `--mem-fraction-static="$FRACTION"`},
		{name: "non-finite fraction", script: engine + "--mem-fraction-static=Inf"},
		{name: "helper flags", script: "echo --disable-cuda-graph; " + engine + "--mem-fraction-static 0.90", warn: true},
		{name: "graph disabled in engine command", script: engine + "--mem-fraction-static=0.90 --disable-cuda-graph"},
		{name: "padding only", change: func(_ *LaunchRequest, plan *LaunchPlan) {
			plan.Resolved.Argv = append(plan.Resolved.Argv, "--disable-cuda-graph-padding", "--cuda-graph-max-bs=256")
		}, warn: true},
		{name: "explicit manual argv", change: func(_ *LaunchRequest, plan *LaunchPlan) {
			plan.Resolved.CommandMode = string(deploy.CommandManual)
			plan.Resolved.Entrypoint = []string{"python3", "-m", "sglang.launch_server"}
		}, warn: true},
		{name: "mixed GPU preview", change: func(req *LaunchRequest, _ *LaunchPlan) {
			req.Placement.GPUIndexes = nil
			req.Node.GPUs = append(req.Node.GPUs, store.GPU{Index: 1, Vendor: "ascend", ArchID: "910B2"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := LaunchRequest{
				Engine: deploy.EngineSGLang, Source: LaunchSource{Image: qwen3DGPUImage},
				Selection: deploy.CatalogSelection{ModelID: qwen3DGPUModel},
				Placement: Placement{GPUIndexes: []int{0}},
				Node:      &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "ascend", ArchID: "910B4"}}},
			}
			plan := LaunchPlan{
				Params: deploy.Params{
					Vendor: "ascend", ArchID: "910B4", PartitionBackend: accelerator.BackendAscendLogical,
					VRAMLimitBytes: 8 << 30, AssignedGPUs: 1, TensorParallel: 1,
				},
				Resolved: deploy.ResolvedLaunch{CommandMode: string(deploy.CommandEngine), Argv: []string{"--dtype", "bfloat16", "--mem-fraction-static=0.90"}},
			}
			if tc.script != "" {
				plan.Resolved = deploy.ResolvedLaunch{
					CommandMode: string(deploy.CommandManual), Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", tc.script},
				}
			}
			if tc.change != nil {
				tc.change(&req, &plan)
			}
			warning := dgpuStartupMemoryWarning(req, plan)
			if tc.warn && (warning == nil || warning.Code != messages.WarningDGPUStartupMemory) {
				t.Fatalf("warning = %+v, want the dGPU startup memory warning", warning)
			}
			if !tc.warn && warning != nil {
				t.Fatalf("warning = %+v, want no dGPU startup memory warning", warning)
			}
		})
	}
}
