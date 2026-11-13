package planning

import (
	"math"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStartupReferenceEstimateUsesCheckpointBytesAndKeepsUnknowns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		model       *store.Model
		selection   int64
		wantWeight  int64
		wantSeconds float64
	}{
		{name: "checkpoint", model: &store.Model{WeightBytes: 1 << 30}, wantWeight: 1 << 30, wantSeconds: 302},
		{name: "selected checkpoint overrides base", model: &store.Model{WeightBytes: 1 << 30}, selection: 2 << 30, wantWeight: 2 << 30, wantSeconds: 304},
		{name: "selection without model metadata", selection: 1 << 30, wantWeight: 1 << 30, wantSeconds: 302},
		{name: "missing model"},
		{name: "unknown bytes", model: &store.Model{}},
		{name: "invalid negative bytes", model: &store.Model{WeightBytes: -1}},
		{name: "large checkpoint does not overflow", model: &store.Model{WeightBytes: math.MaxInt64}, wantWeight: math.MaxInt64, wantSeconds: float64(math.MaxInt64)/(512*1024*1024) + 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := LaunchRequest{Engine: deploy.EngineVLLM, Model: tc.model}
			req.Selection.WeightBytes = tc.selection
			got := launchStartupEstimate(req, deploy.ResolvedLaunch{CommandMode: string(deploy.CommandEngine)})
			if got.WeightBytes != tc.wantWeight {
				t.Fatalf("weight=%d want=%d", got.WeightBytes, tc.wantWeight)
			}
			if tc.wantSeconds == 0 {
				if got.Method != "missing_weight" || got.EstimatedSeconds != nil || got.ReferenceMiBPerSec != 0 || got.InitializationSeconds != 0 {
					t.Fatalf("invented inputs or ETA: %+v", got)
				}
				return
			}
			if got.Method != "weight_reference" || got.EstimatedSeconds == nil || *got.EstimatedSeconds != tc.wantSeconds || math.IsNaN(*got.EstimatedSeconds) || math.IsInf(*got.EstimatedSeconds, 0) || got.ReferenceMiBPerSec != 512 || got.InitializationSeconds != 300 {
				t.Fatalf("estimate=%+v", got)
			}
		})
	}
}

func TestStartupReferenceEstimateDoesNotInventACollectiveCriticalPath(t *testing.T) {
	for _, mode := range []string{"sglang", "distributed", "headless", "pd", "components", "manual", "callable"} {
		t.Run(mode, func(t *testing.T) {
			req := LaunchRequest{Engine: deploy.EngineVLLM, Model: &store.Model{WeightBytes: 1 << 30}}
			launch := deploy.ResolvedLaunch{CommandMode: string(deploy.CommandEngine)}
			switch mode {
			case "sglang":
				req.Engine = deploy.EngineSGLang
			case "distributed":
				req.Topology.NNodes = 2
			case "headless":
				req.Topology.Headless = true
			case "pd":
				req.PD = &deploy.PDLaunch{}
			case "components":
				req.Selection.Components = []deploy.ComponentBinding{{}}
			case "manual":
				launch.CommandMode = string(deploy.CommandManual)
			case "callable":
				req.Engine = deploy.EngineCallable
			}
			got := launchStartupEstimate(req, launch)
			if mode == "sglang" {
				if got.EstimatedSeconds == nil || *got.EstimatedSeconds != 242 {
					t.Fatalf("supported estimate=%+v", got)
				}
			} else if got.Method != "unsupported" || got.EstimatedSeconds != nil || got.ReferenceMiBPerSec != 0 || got.InitializationSeconds != 0 {
				t.Fatalf("unsupported estimate=%+v", got)
			}
		})
	}
}

// The estimate follows the effective launch, rather than stale template args.
func TestStartupReferenceInitializationFollowsEffectiveEngineMode(t *testing.T) {
	for _, tc := range []struct {
		name, engine       string
		entrypoint, argv   []string
		templateArgs       []string
		wantInitialization int
	}{
		{name: "vllm", engine: deploy.EngineVLLM, wantInitialization: 300},
		{name: "sglang", engine: deploy.EngineSGLang, wantInitialization: 240},
		{name: "omni final argv", engine: deploy.EngineVLLM, argv: []string{"--omni"}, wantInitialization: 420},
		{name: "omni final entrypoint", engine: deploy.EngineVLLM, entrypoint: []string{"vllm", "serve", "/model", "--omni"}, wantInitialization: 420},
		{name: "removed template omni", engine: deploy.EngineVLLM, templateArgs: []string{"--omni"}, wantInitialization: 300},
		{name: "omni text is not a flag", engine: deploy.EngineVLLM, argv: []string{"--served-model-name", "omni-model"}, wantInitialization: 300},
		{name: "omni json is not a flag", engine: deploy.EngineVLLM, argv: []string{"--extra-config", `{"note":"--omni"}`}, wantInitialization: 300},
		{name: "opaque shell is not an engine mode", engine: deploy.EngineVLLM, entrypoint: []string{"/bin/sh"}, argv: []string{"-c", "echo --omni"}, wantInitialization: 300},
		{name: "eager does not invent a measured discount", engine: deploy.EngineVLLM, argv: []string{"--enforce-eager"}, wantInitialization: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := LaunchRequest{Engine: tc.engine, Model: &store.Model{WeightBytes: 1 << 30}, Node: &store.Node{RuntimeDriver: "udocker"}}
			req.Selection.ExtraArgs = tc.templateArgs
			launch := deploy.ResolvedLaunch{CommandMode: string(deploy.CommandEngine), Entrypoint: tc.entrypoint, Argv: tc.argv}
			got := launchStartupEstimate(req, launch)
			if got.EstimatedSeconds == nil || *got.EstimatedSeconds != float64(tc.wantInitialization+2) || got.InitializationSeconds != tc.wantInitialization {
				t.Fatalf("estimate=%+v want initialization=%d", got, tc.wantInitialization)
			}
		})
	}
}
