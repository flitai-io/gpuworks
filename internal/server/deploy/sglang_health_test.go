package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// Every SGLang launch, PD role or not, keeps /health off the scheduler so a
// long prefill cannot make a busy engine look dead; the setting is a system
// default the deployment's own environment can still change or remove.
func TestResolveLaunchSGLangHealthSkipsGeneration(t *testing.T) {
	base := sglangSeverityInput(t)
	prefill := sglangSeverityInput(t)
	prefill.Params.PD = &PDLaunch{Mode: "sglang-nixl", Role: pdmode.RolePrefill, SideChannelHost: "10.0.0.5", SideChannelPort: 30001}
	for name, input := range map[string]ResolveLaunchInput{"plain": base, "pd-prefill": prefill} {
		t.Run(name, func(t *testing.T) {
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if launch.Env[sglangHealthGenerationEnv] != "0" || launch.EnvLayers.System[sglangHealthGenerationEnv] != "0" {
				t.Fatalf("health generation not disabled as a system default: env=%v layers=%+v", launch.Env, launch.EnvLayers)
			}
		})
	}

	override := sglangSeverityInput(t)
	override.ContainerOverrides = ContainerOverrides{Env: map[string]string{sglangHealthGenerationEnv: "1"}}
	launch, err := ResolveLaunch(override)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env[sglangHealthGenerationEnv] != "1" {
		t.Fatalf("deployment env did not win: %v", launch.Env)
	}
	unset := sglangSeverityInput(t)
	unset.ContainerOverrides = ContainerOverrides{EnvUnset: []string{sglangHealthGenerationEnv}}
	if launch, err = ResolveLaunch(unset); err != nil {
		t.Fatal(err)
	}
	if _, ok := launch.Env[sglangHealthGenerationEnv]; ok {
		t.Fatalf("env_unset left the variable: %v", launch.Env)
	}
}
