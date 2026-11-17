package deploy

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestVLLMOmniLaunchStatesNoGPUMemoryShare(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		templateArgs []string
		extraArgs    []string
		omni         bool
	}{
		{name: "plain vLLM keeps the dictionary share", templateArgs: []string{"--trust-remote-code"}},
		{name: "template --omni", templateArgs: []string{"--omni", "--trust-remote-code"}, omni: true},
		{name: "user --omni", extraArgs: []string{"--omni"}, omni: true},
		{name: "--omni as a value is not the switch", templateArgs: []string{"--served-model-name", "omni"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// An omni launch keeps stage-specific memory shares and context
			// lengths even on a release accepting the plain-vLLM defaults.
			out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Command: &command, TemplateArgs: test.templateArgs, ExtraArgs: test.extraArgs, EngineVersion: "0.30.0"})
			if err != nil {
				t.Fatal(err)
			}
			launch := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: out.Quick, TemplateArgs: test.templateArgs, ExtraArgs: test.extraArgs})
			emitted := slices.Contains(launch.Argv, "--gpu-memory-utilization")
			if test.omni {
				if out.Quick.GPUMemUtil != 0 || emitted || !reflect.DeepEqual(out.EngineDecidesParamKeys, []string{"gpu_memory_utilization", "max_model_len"}) {
					t.Fatalf("omni launch quick=%v engine decides=%v argv=%v", out.Quick.GPUMemUtil, out.EngineDecidesParamKeys, launch.Argv)
				}
				return
			}
			if out.Quick.GPUMemUtil != 0.9 || !emitted || len(out.EngineDecidesParamKeys) != 0 {
				t.Fatalf("plain launch quick=%v engine decides=%v argv=%v", out.Quick.GPUMemUtil, out.EngineDecidesParamKeys, launch.Argv)
			}
		})
	}
	// Admission still charges the unstated share (planning falls back to it).
	if DefaultGPUMemUtil(EngineVLLM) != 0.9 {
		t.Fatalf("default share=%v", DefaultGPUMemUtil(EngineVLLM))
	}
}

// The share is only left unstated: a deployer or template that states one on
// an omni launch gets it, wherever it was written.
func TestVLLMOmniLaunchKeepsAStatedGPUMemoryShare(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input MaterializeInput
	}{
		{name: "quick", input: MaterializeInput{Raw: map[string]any{"gpu_memory_utilization": 0.5}}},
		{name: "extra args", input: MaterializeInput{ExtraArgs: []string{"--gpu-memory-utilization", "0.5"}}},
		{name: "template args", input: MaterializeInput{TemplateArgs: []string{"--omni", "--gpu-memory-utilization", "0.5"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := test.input
			input.Engine, input.Command = EngineVLLM, &command
			if len(input.TemplateArgs) == 0 {
				input.TemplateArgs = []string{"--omni"}
			}
			out, err := Materialize(input)
			if err != nil {
				t.Fatal(err)
			}
			if out.Quick.GPUMemUtil != 0.5 {
				t.Fatalf("stated share = %v, want 0.5", out.Quick.GPUMemUtil)
			}
		})
	}
}

func TestVLLMOmniTopologyCountsDiffusionParallelDegrees(t *testing.T) {
	for _, test := range []struct {
		name     string
		assigned int
		args     []string
		wantErr  string
	}{
		{name: "TP2 x Ulysses2 on four GPUs", assigned: 4, args: []string{"--omni", "--tensor-parallel-size", "2", "--usp", "2"}},
		{name: "Ulysses4 with TP1 on four GPUs", assigned: 4, args: []string{"--omni", "--tensor-parallel-size", "1", "--ulysses-degree=4"}},
		{name: "ring and CFG degrees multiply in", assigned: 8, args: []string{"--omni", "--tensor-parallel-size", "2", "--ring", "2", "--cfg-parallel-size", "2"}},
		{name: "HSDP and VAE patch parallel take no GPUs of their own", assigned: 2, args: []string{"--omni", "--tensor-parallel-size", "2", "--use-hsdp", "--vae-patch-parallel-size", "2"}},
		{name: "a world short of GPUs is refused", assigned: 2, args: []string{"--omni", "--tensor-parallel-size", "2", "--usp", "2"}, wantErr: "diffusion sequence/CFG parallel 2 requires 4 GPUs, but placement assigned 2"},
		{name: "without --omni the degrees are not vLLM's", assigned: 2, args: []string{"--tensor-parallel-size", "2", "--usp", "2"}},
		{name: "a symbolic degree waits for the expanded launch", assigned: 4, args: []string{"--omni", "--tensor-parallel-size", "2", "--usp", "${SP}"}},
		{name: "a degree below one is refused", assigned: 2, args: []string{"--omni", "--tensor-parallel-size", "2", "--usp", "0"}, wantErr: "--usp requires a positive integer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (vLLMEngine{}).ValidateTopology(ResolvedLaunch{Argv: test.args}, Params{AssignedGPUs: test.assigned})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v, want %q", err, test.wantErr)
			}
		})
	}
}
