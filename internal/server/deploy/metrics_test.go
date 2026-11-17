package deploy

import (
	"slices"
	"testing"
)

func TestSGLangMetricsComeOnlyFromExplicitParameters(t *testing.T) {
	engine, err := SelectEngine(EngineSGLang)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		params  Params
		enabled bool
	}{
		{name: "no implicit metrics"},
		{name: "structured enabled", params: Params{Quick: LaunchConfig{EnableMetrics: true}}, enabled: true},
		{name: "catalog enabled", params: Params{TemplateArgs: []string{"--enable-metrics"}}, enabled: true},
		{name: "advanced override", params: Params{ExtraArgs: []string{"--enable-metrics"}}, enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			launch := engine.Build(test.params)
			if got := slices.Contains(launch.Entrypoint, "--enable-metrics"); got != test.enabled {
				t.Fatalf("metrics enabled=%v, want %v: %v", got, test.enabled, launch.Entrypoint)
			}
		})
	}
}
