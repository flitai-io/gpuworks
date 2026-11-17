package proxy

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The gateway sends an engine the name its launch tells the engine to answer
// under. For a deployment whose alias was cleared that is the model mount,
// stated on the command line so a runtime that maps the model path to the
// host's keeps it; the agent's port identity check reads the same name from
// ServedModelName.
func TestGatewayModelNameIsTheLaunchServedName(t *testing.T) {
	cleared := ""
	for _, c := range []struct {
		name   string
		engine string
		params deploy.Params
	}{
		{"vllm --model", deploy.EngineVLLM, deploy.Params{ModelPath: "/model"}},
		{"vllm serve positional", deploy.EngineVLLM, deploy.Params{ModelPath: "/model", PositionalModel: true}},
		{"sglang", deploy.EngineSGLang, deploy.Params{ModelPath: "/model"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			engine, err := deploy.SelectEngine(c.engine)
			if err != nil {
				t.Fatal(err)
			}
			c.params.Quick.ServedModelName = &cleared
			built := engine.Build(c.params)
			launch := proto.ResolvedLaunch{Entrypoint: built.Entrypoint, Argv: built.Argv, ModelMount: "/model"}
			served := deploy.ServedModelName(launch)
			if served != "/model" {
				t.Fatalf("launch names the model %q, want /model: %v %v", served, launch.Entrypoint, launch.Argv)
			}
			desired, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: launch, ServedModelName: served})
			if err != nil {
				t.Fatal(err)
			}
			got := instanceModelName(&store.Deployment{Engine: c.engine}, &store.Instance{DesiredStateJSON: string(desired)})
			if got != served {
				t.Fatalf("gateway sends %q, launch serves %q", got, served)
			}
		})
	}
}
