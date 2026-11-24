package deploy

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func commandJSON(t *testing.T, args []string) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestInspectShellCommandForms(t *testing.T) {
	const script = "vllm serve /model"
	for name, test := range map[string]struct {
		entrypoint []string
		argv       []string
		want       shellCommandShape
	}{
		"sh short option cluster": {
			entrypoint: []string{"/bin/sh"}, argv: []string{"-ec", script},
			want: shellCommandShape{script: script, shell: true, commandString: true},
		},
		"bash login": {
			entrypoint: []string{"bash"}, argv: []string{"-lc", script},
			want: shellCommandShape{script: script, shell: true, commandString: true},
		},
		"env bash": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"bash", "-c", script},
			want: shellCommandShape{script: script, shell: true, commandString: true},
		},
		"env options and assignment": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"-i", "FOO=1", "/bin/sh", "-lc", script},
			want: shellCommandShape{script: script, shell: true, commandString: true},
		},
		"env split string": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"-S", "bash -ec 'vllm serve /model'"},
			want: shellCommandShape{script: script, shell: true, commandString: true},
		},
		"shell script file": {
			entrypoint: []string{"bash"}, argv: []string{"/opt/serve.sh"},
			want: shellCommandShape{shell: true},
		},
		"direct executable": {
			entrypoint: []string{"vllm"}, argv: []string{"serve", "/model"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := inspectShellCommand(test.entrypoint, test.argv); got != test.want {
				t.Fatalf("inspectShellCommand(%v, %v) = %+v; want %+v", test.entrypoint, test.argv, got, test.want)
			}
		})
	}
}

func TestStaticShellWordsResolveLiteralFragments(t *testing.T) {
	script := `# environment
VLLM_TEST=1

vllm serve /model --engram-confi'g' '{"cpu_offload":true,"dp_shared_memory":true}'`
	words, err := staticShellWords(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"VLLM_TEST=1", "vllm", "serve", "/model", "--engram-config", engramTestConfig}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words = %#v, want %#v", words, want)
	}
	if !inspectEngramArguments(words).present {
		t.Fatal("statically reconstructed Engram option was not recognized")
	}
}

func TestStaticShellWordsSkipsHeredocData(t *testing.T) {
	script := "cat <<'JSON'\n{\"command\":\"vllm --engram-config\",\"dynamic\":\"$COMMAND\"}\nJSON\n" +
		"vllm serve /model --engram-confi'g' '" + engramTestConfig + "'"
	words, err := staticShellWords(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cat", "vllm", "serve", "/model", "--engram-config", engramTestConfig}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words = %#v, want %#v", words, want)
	}
	if !inspectEngramArguments(words).shared {
		t.Fatal("the engine config after the heredoc was not recognized")
	}
}

func TestStaticShellWordsRejectDynamicShapes(t *testing.T) {
	for _, script := range []string{
		"vllm serve /model $ENGRAM_ARGS",
		"vllm serve /model `cat args`",
		"vllm serve /model --engram-confi'g",
		"vllm serve /model --engram-confi{g,}",
		"cat <<'JSON'\n{\"unterminated\":true}\n",
	} {
		t.Run(script, func(t *testing.T) {
			if _, err := staticShellWords(script); err == nil {
				t.Fatalf("staticShellWords(%q) succeeded for a dynamic shape", script)
			}
		})
	}
}

func TestStaticShellWordsTreatCommandSeparatorsAsWordBoundaries(t *testing.T) {
	words, err := staticShellWords("cd /tmp && vllm serve /model --engram-config 'x' | tee out.log; echo done &")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cd", "/tmp", "vllm", "serve", "/model", "--engram-config", "x", "tee", "out.log", "echo", "done"}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words = %#v, want %#v", words, want)
	}
}

func TestAscendCompatWarnsAboutShellOptionAndEnvForms(t *testing.T) {
	for name, command := range map[string][]string{
		"sh cluster":       {"/bin/sh", "-ec", "vllm serve /model"},
		"bash cluster":     {"/bin/bash", "-euc", "vllm serve /model"},
		"env bash":         {"/usr/bin/env", "bash", "-c", "vllm serve /model"},
		"env with options": {"/usr/bin/env", "-i", "FOO=1", "/bin/bash", "-lc", "vllm serve /model"},
		"unknown shell":    {"/bin/sh", "--unsupported", "-c", "vllm serve /model"},
	} {
		t.Run(name, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, command),
				Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"},
			}
			_, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, messages.WarningAscendCompatManualShell)
		})
	}
}

// Commands that never mention Engram are not this check's business, whatever
// shell syntax they use and on whichever vendor they run.
func TestShellCommandsWithoutEngramPassSilently(t *testing.T) {
	commands := map[string][]string{
		"variables":         {"/bin/sh", "-c", `NAME=demo; vllm serve /model --served-model-name "$NAME"`},
		"and list":          {"/bin/sh", "-c", "cd /tmp && vllm serve /model"},
		"pipe":              {"/bin/sh", "-c", "vllm serve /model 2>&1 | tee /tmp/engine.log"},
		"redirect":          {"bash", "-c", "vllm serve /model > /tmp/out.log 2>&1"},
		"glob":              {"/bin/sh", "-c", "cp /opt/*.json /tmp && vllm serve /model"},
		"command subst":     {"bash", "-lc", `vllm serve /model --max-model-len "$(cat /tmp/len)"`},
		"nested shell":      {"/bin/sh", "-ec", "bash -c 'vllm serve /model'"},
		"eval":              {"/bin/sh", "-ec", "eval 'vllm serve /model'"},
		"source":            {"/bin/sh", "-ec", ". /tmp/engine-args\nvllm serve /model"},
		"xargs":             {"/bin/sh", "-ec", "xargs vllm serve </tmp/engine-args"},
		"env wrapper":       {"/usr/bin/env", "bash", "-euc", "export A=1; vllm serve /model"},
		"unknown wrapper":   {"/bin/sh", "--unsupported", "-c", "vllm serve /model"},
		"argument sh value": {"vllm", "serve", "/model", "--chat-template", "sh"},
	}
	for _, vendor := range []string{"nvidia", "ppu"} {
		for name, command := range commands {
			t.Run(vendor+"/"+name+"/json", func(t *testing.T) {
				input := engramLaunchInput(proto.RuntimePreferenceDocker)
				input.Params.Vendor, input.Params.TemplateArgs = vendor, nil
				input.ContainerOverrides = ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, command)}
				_, warnings := resolveWithWarnings(t, input)
				requireWarningCodes(t, warnings)
			})
		}
		t.Run(vendor+"/manual text", func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceDocker)
			input.Params.Vendor, input.Params.TemplateArgs = vendor, nil
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CommandIncludesPreparation: true,
				CmdDisplay: "# environment\nNAME=demo\nexport CUDA_VISIBLE_DEVICES=$(seq -s, 0 3)\n\n# launch\ncd /tmp && vllm serve /model --port 30000 --served-model-name \"$NAME\" 2>&1 | tee /tmp/engine.log",
			}
			launch, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings)
			if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
				t.Fatalf("unrelated manual command changed resources: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
			}
		})
	}
}

func TestEngramShellCommandsAreReadThroughWrappers(t *testing.T) {
	t.Run("env wrapper with literal quote concatenation", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.Params.TemplateArgs = nil
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual),
			CmdDisplay:  commandJSON(t, []string{"/usr/bin/env", "bash", "-euc", `vllm serve /model --engram-confi'g' '` + engramTestConfig + `'`}),
		}
		launch, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
		if launch.ShmBytes != engramSharedMemoryDefaultBytes {
			t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
		}
	})
	for name, command := range map[string][]string{
		"dynamic expansion": {"/bin/sh", "-ec", "vllm serve /model --engram-config $ENGRAM_CFG"},
		"glob":              {"/bin/sh", "-ec", "vllm serve /model --engram-config /etc/engram/*.json"},
		"nested shell":      {"/bin/sh", "-ec", `bash -c "vllm serve /model --engram_config '` + engramTestConfig + `'"`},
	} {
		t.Run(name+" only warns", func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceDocker)
			input.Params.TemplateArgs = nil
			input.ContainerOverrides = ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, command)}
			_, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, messages.WarningEngramManualUnchecked)
		})
	}
	t.Run("a path that merely contains the word is silent", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.Params.TemplateArgs = nil
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, []string{"/bin/sh", "-c", "vllm serve /models/DeepSeek-Engram-Lite"}),
		}
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
	})
	t.Run("engine script file remains allowed", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.Entrypoint = []string{"bash", "/opt/gpuworks/tensorfold/serve.sh"}
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
	})
	t.Run("static ordinary shell remains allowed", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual),
			CmdDisplay:  commandJSON(t, []string{"/usr/bin/env", "bash", "-euc", "# environment\nVLLM_TEST=1\nvllm serve /model"}),
		}
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
	})
}
