package deploy

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestAscendCompatManualShellWarns(t *testing.T) {
	for name, test := range map[string]struct {
		overrideV2 string
		scriptV2   string
	}{
		"UI generated V2 manual":                {overrideV2: "1", scriptV2: "1"},
		"manual runner env edit is not trusted": {overrideV2: "1", scriptV2: "0"},
	} {
		t.Run(name, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CommandIncludesPreparation: true,
				CmdDisplay: "# environment\nVLLM_USE_V2_MODEL_RUNNER=" + test.scriptV2 + "\n\n# launch\nvllm serve /model --port 30000",
				Env:        map[string]string{"VLLM_USE_V2_MODEL_RUNNER": test.overrideV2},
			}
			launch, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, messages.WarningAscendCompatManualShell)
			if warnings[0].Params["runtime"] != proto.RuntimePreferenceUdocker || warnings[0].Params["image"] != ascendCompatTestImage {
				t.Fatalf("warning params = %#v", warnings[0].Params)
			}
			if !strings.Contains(launch.PreStartScript, ascendV030RC2A3V2CompatMarker) {
				t.Fatal("a warned manual launch must still carry the compatibility script")
			}
		})
	}
	t.Run("Docker manual needs no PRoot overlay", func(t *testing.T) {
		input := ascendCompatLaunchInput(proto.RuntimePreferenceDocker)
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = "vllm serve /model --port 30000"
		launch, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
		if launch.PreStartScript != "" {
			t.Fatalf("Docker manual = %+v", launch)
		}
	})
}

func TestMergedPreStartScriptLimitIsCheckedBeforeTheAgent(t *testing.T) {
	input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
	input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
	input.PreStartScript = strings.Repeat("x", maxResolvedPreStartScriptBytes)
	_, err := ResolveLaunch(input)
	product := requireLaunchProductError(t, err, messages.ErrorPreStartScriptTooLarge)
	if product.Params()["max_bytes"] != maxResolvedPreStartScriptBytes {
		t.Fatalf("error params = %#v", product.Params())
	}
	if got, ok := product.Params()["bytes"].(int); !ok || got <= maxResolvedPreStartScriptBytes {
		t.Fatalf("merged script size = %#v", product.Params()["bytes"])
	}
}

func TestEngramManualShellIsReadStatically(t *testing.T) {
	const command = "# environment\nVLLM_TEST=1\n\n# launch\nvllm serve /model --port 30000 --engram-config '" + engramTestConfig + "' 2>&1 | tee /tmp/engine.log"
	manual := func(runtime, cmd string) ResolveLaunchInput {
		input := engramLaunchInput(runtime)
		input.Params.TemplateArgs = nil
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual), CommandIncludesPreparation: true, CmdDisplay: cmd,
		}
		return input
	}
	t.Run("parseable manual command derives shared memory like engine mode", func(t *testing.T) {
		launch, warnings := resolveWithWarnings(t, manual(proto.RuntimePreferenceDocker, command))
		requireWarningCodes(t, warnings)
		if launch.ShmBytes != engramSharedMemoryDefaultBytes {
			t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
		}
		if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, []string{"memlock=-1:-1"}) {
			t.Fatalf("memlock args = %v in %v", got, launch.DockerArgs)
		}
		if got := engramCapabilityCount(launch.DockerArgs); got != 1 {
			t.Fatalf("IPC_LOCK count = %d in %v", got, launch.DockerArgs)
		}
	})
	t.Run("quote concatenation is resolved", func(t *testing.T) {
		launch, _ := resolveWithWarnings(t, manual(proto.RuntimePreferenceDocker,
			`vllm serve /model --engram-confi'g' '`+engramTestConfig+`'`))
		if launch.ShmBytes != engramSharedMemoryDefaultBytes {
			t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
		}
	})
	t.Run("parseable manual command on udocker warns", func(t *testing.T) {
		launch, warnings := resolveWithWarnings(t, manual(proto.RuntimePreferenceUdocker, command))
		requireWarningCodes(t, warnings, messages.WarningEngramSharedMemoryRuntime)
		if launch.ShmBytes != engramSharedMemoryDefaultBytes {
			t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
		}
	})
	t.Run("disabled shared memory in a manual command is silent", func(t *testing.T) {
		launch, warnings := resolveWithWarnings(t, manual(proto.RuntimePreferenceUdocker,
			`vllm serve /model --port 30000 --engram-config '{"cpu_offload":true,"dp_shared_memory":false}'`))
		requireWarningCodes(t, warnings)
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes {
			t.Fatalf("shm = %d", launch.ShmBytes)
		}
	})
	for name, cmd := range map[string]string{
		"variable argument":    `ARGS="--engram-config $CFG"` + "\nvllm serve /model --port 30000 $ARGS",
		"command substitution": "vllm serve /model --port 30000 --engram-config \"$(cat /etc/engram.json)\"",
		"nested shell":         `bash -c "vllm serve /model --port 30000 --engram-config '` + engramTestConfig + `'"`,
	} {
		t.Run("unreadable "+name+" only warns", func(t *testing.T) {
			launch, warnings := resolveWithWarnings(t, manual(proto.RuntimePreferenceDocker, cmd))
			requireWarningCodes(t, warnings, messages.WarningEngramManualUnchecked)
			if warnings[0].Params["flag"] != "--engram-config" {
				t.Fatalf("warning params = %#v", warnings[0].Params)
			}
			if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes {
				t.Fatalf("an unchecked command must not derive resources: shm=%d", launch.ShmBytes)
			}
		})
	}
	t.Run("ordinary manual udocker is unchanged", func(t *testing.T) {
		input := manual(proto.RuntimePreferenceUdocker, "vllm serve /model --port 30000")
		launch, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
			t.Fatalf("ordinary manual resources changed: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
		}
	})
}

func TestEngramLastValidConfigControlsResourcesAndPreflight(t *testing.T) {
	disabled := `{"cpu_offload":true,"dp_shared_memory":false}`
	for name, test := range map[string]struct {
		first, second string
		wantShared    bool
	}{
		"true then false is silent": {first: engramTestConfig, second: disabled},
		"false then true warns":     {first: disabled, second: engramTestConfig, wantShared: true},
	} {
		t.Run(name, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceUdocker)
			input.Params.TemplateArgs = []string{"--engram-config", test.first, "--engram-config=" + test.second}
			launch, warnings := resolveWithWarnings(t, input)
			if test.wantShared {
				requireWarningCodes(t, warnings, messages.WarningEngramSharedMemoryRuntime)
				return
			}
			requireWarningCodes(t, warnings)
			if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
				t.Fatalf("last disabled config kept Engram resources: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
			}
		})
	}
}

func TestShellJSONCommandsFollowTheSameRules(t *testing.T) {
	for _, mode := range []string{string(CommandManual), string(CommandEngine)} {
		t.Run("compat "+mode, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides.CommandMode = mode
			input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
			if mode == string(CommandManual) {
				input.ContainerOverrides.CmdDisplay = `["/bin/sh","-c","VLLM_USE_V2_MODEL_RUNNER=0 vllm serve /model"]`
			} else {
				input.Entrypoint = []string{"/bin/sh"}
				input.ContainerOverrides.CmdDisplay = `["-c","VLLM_USE_V2_MODEL_RUNNER=0 vllm serve /model"]`
			}
			_, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, messages.WarningAscendCompatManualShell)
		})
		t.Run("engram "+mode, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides.CommandMode = mode
			if mode == string(CommandManual) {
				input.ContainerOverrides.CmdDisplay = `["/bin/sh","-c","vllm serve /model --engram-config '{\"cpu_offload\":true,\"dp_shared_memory\":true}'"]`
			} else {
				input.Entrypoint = []string{"/bin/sh"}
				input.ContainerOverrides.CmdDisplay = `["-c","vllm serve /model --engram-config '{\"cpu_offload\":true,\"dp_shared_memory\":true}'"]`
			}
			launch, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, messages.WarningEngramSharedMemoryRuntime)
			if launch.ShmBytes != engramSharedMemoryDefaultBytes {
				t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
			}
		})
	}
}
