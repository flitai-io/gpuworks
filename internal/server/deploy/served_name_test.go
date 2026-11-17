package deploy

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func launchTokens(l ResolvedLaunch) []string {
	return append(append([]string(nil), l.Entrypoint...), l.Argv...)
}

// A deployment whose alias was cleared still tells the engine its name: the
// model's mount target, which is what the engine would default to inside a
// container and what the gateway sends. Spelled out, it survives a runtime
// that translates the model path (a host-process agent) unchanged.
func TestClearedAliasNamesTheModelAfterItsMount(t *testing.T) {
	cleared := ""
	for _, c := range []struct {
		name   string
		engine string
		params Params
		want   string // "" = no --served-model-name
	}{
		{"vllm --model", EngineVLLM, Params{ModelPath: "/model", Quick: LaunchConfig{ServedModelName: &cleared}}, "/model"},
		{"vllm serve positional", EngineVLLM, Params{ModelPath: "/model", PositionalModel: true, Quick: LaunchConfig{ServedModelName: &cleared}}, "/model"},
		{"vllm legacy no alias", EngineVLLM, Params{ModelPath: "/model"}, "/model"},
		{"sglang", EngineSGLang, Params{ModelPath: "/model", Quick: LaunchConfig{ServedModelName: &cleared}}, "/model"},
		{"vllm alias wins", EngineVLLM, Params{ModelPath: "/model", ServedName: "qwen"}, "qwen"},
		{"sglang alias wins", EngineSGLang, Params{ModelPath: "/model", ServedName: "qwen"}, "qwen"},
		{"user flag wins", EngineVLLM, Params{ModelPath: "/model", Quick: LaunchConfig{ServedModelName: &cleared}, ExtraArgs: []string{"--served-model-name", "mine"}}, "mine"},
		{"vllm headless follower", EngineVLLM, Params{ModelPath: "/model", Headless: true, Quick: LaunchConfig{ServedModelName: &cleared}}, ""},
		{"sglang headless follower", EngineSGLang, Params{ModelPath: "/model", Headless: true, Quick: LaunchConfig{ServedModelName: &cleared}}, ""},
		{"vllm old host agent", EngineVLLM, Params{ModelPath: "/model", ImplicitServedName: true, Quick: LaunchConfig{ServedModelName: &cleared}}, ""},
		{"sglang old host agent", EngineSGLang, Params{ModelPath: "/model", ImplicitServedName: true, Quick: LaunchConfig{ServedModelName: &cleared}}, ""},
		{"old host agent keeps an alias", EngineVLLM, Params{ModelPath: "/model", ImplicitServedName: true, ServedName: "qwen"}, "qwen"},
	} {
		t.Run(c.name, func(t *testing.T) {
			engine, err := SelectEngine(c.engine)
			if err != nil {
				t.Fatal(err)
			}
			launch := engine.Build(c.params)
			tokens := launchTokens(launch)
			got := ServedModelName(proto.ResolvedLaunch{Entrypoint: launch.Entrypoint, Argv: launch.Argv})
			if got != c.want {
				t.Fatalf("served name = %q, want %q: %v", got, c.want, tokens)
			}
			if n := countFlag(tokens, "--served-model-name"); (c.want == "" && n != 0) || (c.want != "" && n != 1) {
				t.Fatalf("%d --served-model-name flags: %v", n, tokens)
			}
			if c.params.PositionalModel && !slices.Equal(launch.Entrypoint[:3], []string{EngineVLLM, "serve", "/model"}) {
				t.Fatalf("positional model moved: %v", tokens)
			}
		})
	}
}
