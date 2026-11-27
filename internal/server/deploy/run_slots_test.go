package deploy

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRunSlots(t *testing.T) {
	for _, test := range []struct {
		name   string
		engine string
		args   []string
		slots  int
		known  bool
	}{
		{"vllm flag value", EngineVLLM, []string{"vllm", "serve", "/m", "--max-num-seqs", "64"}, 64, true},
		{"sglang flag value", EngineSGLang, []string{"python3", "-m", "sglang.launch_server", "--max-running-requests", "32"}, 32, true},
		{"inline value", EngineVLLM, []string{"--max-num-seqs=64"}, 64, true},
		{"last occurrence wins", EngineVLLM, []string{"--max-num-seqs", "16", "--max-num-seqs=48"}, 48, true},
		{"not stated", EngineVLLM, []string{"vllm", "serve", "/m", "--max-model-len", "4096"}, 0, false},
		{"the other engine's flag", EngineVLLM, []string{"--max-running-requests", "32"}, 0, false},
		{"sglang reads only its own flag", EngineSGLang, []string{"--max-num-seqs", "64"}, 0, false},
		{"flag without a value", EngineVLLM, []string{"--max-num-seqs"}, 0, false},
		{"not a number", EngineVLLM, []string{"--max-num-seqs", "many"}, 0, false},
		{"zero", EngineVLLM, []string{"--max-num-seqs", "0"}, 0, false},
		{"negative", EngineSGLang, []string{"--max-running-requests=-1"}, 0, false},
		{"shell still expands it", EngineVLLM, []string{"--max-num-seqs", "$SEQS"}, 0, false},
		{"data parallel 1", EngineVLLM, []string{"--max-num-seqs", "64", "--data-parallel-size", "1"}, 64, true},
		{"vllm data parallel above 1", EngineVLLM, []string{"--max-num-seqs", "64", "--data-parallel-size", "2"}, 0, false},
		{"vllm short data parallel", EngineVLLM, []string{"--max-num-seqs", "64", "-dp", "4"}, 0, false},
		{"sglang dp size", EngineSGLang, []string{"--max-running-requests", "32", "--dp-size=2"}, 0, false},
		{"sglang long data parallel", EngineSGLang, []string{"--max-running-requests", "32", "--data-parallel-size", "2"}, 0, false},
		{"data parallel the launch does not fix", EngineSGLang, []string{"--max-running-requests", "32", "--dp-size", "$DP"}, 0, false},
		{"engine without the parameter", "llamacpp", []string{"--max-num-seqs", "64"}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			slots, known := RunSlots(test.engine, test.args)
			if slots != test.slots || known != test.known {
				t.Fatalf("RunSlots(%s, %q) = %d, %v; want %d, %v", test.engine, test.args, slots, known, test.slots, test.known)
			}
		})
	}
}

// specOf is the desired state JSON an instance stores for launch, as the
// store writes it.
func specOf(t *testing.T, launch ResolvedLaunch) string {
	t.Helper()
	raw, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: launch, UUID: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// specRunSlots reads a stored launch back the way the gateway does: decoded,
// split by LaunchArgs, then RunSlots.
func specRunSlots(engine, spec string) (int, bool) {
	var desired proto.InstanceDesiredState
	if json.Unmarshal([]byte(spec), &desired) != nil {
		return 0, false
	}
	args, _, ok := LaunchArgs(desired.ResolvedLaunch)
	if !ok {
		return 0, false
	}
	return RunSlots(engine, args)
}

func TestRunSlotsOfStoredLaunches(t *testing.T) {
	// The form's parameter, through each engine's adapter.
	vllm := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Quick: LaunchConfig{MaxNumSeqs: 64}})
	if slots, ok := specRunSlots(EngineVLLM, specOf(t, vllm)); !ok || slots != 64 {
		t.Fatalf("vllm form = %d, %v; argv %q", slots, ok, priorityArgv(vllm))
	}
	sg := sglangEngine{}.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Quick: LaunchConfig{MaxNumSeqs: 32}})
	if slots, ok := specRunSlots(EngineSGLang, specOf(t, sg)); !ok || slots != 32 {
		t.Fatalf("sglang form = %d, %v; argv %q", slots, ok, priorityArgv(sg))
	}
	// The user's own arguments, in the inline form, over a template's value.
	user := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, TemplateArgs: []string{"--max-num-seqs", "8"}, ExtraArgs: []string{"--max-num-seqs=128"}})
	if slots, ok := specRunSlots(EngineVLLM, specOf(t, user)); !ok || slots != 128 {
		t.Fatalf("user args = %d, %v; argv %q", slots, ok, priorityArgv(user))
	}
	// Nothing states it: unknown, never the engine's default.
	bare := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000})
	if slots, ok := specRunSlots(EngineVLLM, specOf(t, bare)); ok {
		t.Fatalf("bare launch = %d, want unknown; argv %q", slots, priorityArgv(bare))
	}
	// A manual command's shell script is read token by token.
	script := ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "set -a; exec python3 -m sglang.launch_server --model-path /m --max-running-requests 24"}}
	if slots, ok := specRunSlots(EngineSGLang, specOf(t, script)); !ok || slots != 24 {
		t.Fatalf("manual script = %d, %v", slots, ok)
	}
	unparsed := ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "exec vllm serve '/m --max-num-seqs 24"}}
	for name, spec := range map[string]string{"unparsable script": specOf(t, unparsed), "empty": "", "not json": "{", "other engine": specOf(t, vllm)} {
		engine := EngineVLLM
		if name == "other engine" {
			engine = "llamacpp"
		}
		if slots, ok := specRunSlots(engine, spec); ok {
			t.Fatalf("%s = %d, want unknown", name, slots)
		}
	}
}
