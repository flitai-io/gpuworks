package deployment

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCatalogMetricsCanBeKeptEnabledOrDisabled(t *testing.T) {
	for _, test := range []struct {
		name        string
		catalogArgs string
		wire        string
		enabled     bool
		deviated    bool
	}{
		{"catalog default on", `["--enable-metrics"]`, `{}`, true, false},
		{"keep on", `["--enable-metrics"]`, `{"quick_params":{"enable_metrics":true}}`, true, false},
		{"turn off", `["--enable-metrics"]`, `{"quick_params":{"enable_metrics":false}}`, false, true},
		// A catalog spec that is silent on the switch gets the dictionary
		// default (on for an LLM-server entrypoint), so keeping it on is the
		// spec's own configuration and turning it off is the deviation.
		{"silent catalog takes the default", `[]`, `{}`, true, false},
		{"keep the default on a silent catalog", `[]`, `{"quick_params":{"enable_metrics":true}}`, true, false},
		{"turn off a silent catalog", `[]`, `{"quick_params":{"enable_metrics":false}}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			fixture := deploymentFlatFixture()
			fixture.Specs[0].Engine = deploy.EngineSGLang
			fixture.Specs[0].Image = "lmsysorg/sglang:latest"
			fixture.Specs[0].Args = test.catalogArgs
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
				t.Fatal(err)
			}
			model, err := st.InferenceModelByModelID(fixture.ModelID)
			if err != nil {
				t.Fatal(err)
			}
			var req dto.DeployRequest
			if err := json.Unmarshal([]byte(test.wire), &req); err != nil {
				t.Fatal(err)
			}
			req.TemplateID, req.TemplateSpecName = model.ID, fixture.Specs[0].Name
			selection, err := (New(Deps{Store: st})).applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
			if err != nil {
				t.Fatal(err)
			}
			launched, err := materializeDeployRequest(req.Engine, &req, selection, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			selection.ExtraArgs = launched.TemplateResidual
			snapshot := snapshotTemplateSelection(selection, req.Image)
			engine, _ := deploy.SelectEngine(req.Engine)
			launch := engine.Build(deploy.Params{ModelPath: "/model", ServedName: "public-model", Quick: launched.Quick, TemplateArgs: snapshot.ExtraArgs})
			if slices.Contains(snapshot.ExtraArgs, "--enable-metrics") {
				t.Fatalf("a dictionary flag must not remain in the snapshot residual: %v", snapshot.ExtraArgs)
			}
			if got := slices.Contains(launch.Entrypoint, "--enable-metrics"); got != test.enabled {
				t.Fatalf("enabled=%v, want %v: %v", got, test.enabled, launch.Entrypoint)
			}
			if selection.Deviated != test.deviated {
				t.Fatalf("deviated=%v, want %v: %v", selection.Deviated, test.deviated, selection.Diff)
			}
		})
	}
}
