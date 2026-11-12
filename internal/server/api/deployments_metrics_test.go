package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var metricsAPIDBSequence atomic.Uint64

func metricsAPIFixture(t *testing.T, entrypoint, args []string) (*store.Store, *apiHandlers, uint) {
	t.Helper()
	return metricsAPIFixtureImage(t, "lmsysorg/sglang:latest", entrypoint, args)
}

func metricsAPIFixtureImage(t *testing.T, image string, entrypoint, args []string) (*store.Store, *apiHandlers, uint) {
	t.Helper()
	dbName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "_" + strconv.FormatUint(metricsAPIDBSequence.Add(1), 10)
	st, err := store.Open("file:"+dbName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/metrics"
	fixture.Specs[0].Name = "metrics"
	fixture.Specs[0].Engine = deploy.EngineSGLang
	fixture.Specs[0].Image = image
	entrypointJSON, err := json.Marshal(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Specs[0].Entrypoint = string(entrypointJSON)
	fixture.Specs[0].Args = string(argsJSON)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "metrics-node")
	return st, &apiHandlers{d: Deps{Store: st}}, template.ID
}

func metricsAPIRequest(t *testing.T, h *apiHandlers, endpoint string, templateID uint, quick map[string]any, overrides map[string]any, extraArgs string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"template_id":        templateID,
		"template_spec_name": "metrics",
		"node_uuid":          "metrics-node",
		"quick_params":       quick,
	}
	if overrides != nil {
		body["container_overrides"] = overrides
	}
	if extraArgs != "" {
		body["extra_args"] = extraArgs
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if endpoint == "/api/v1/evaluate" {
		h.evaluate(recorder, request)
	} else {
		h.createDeployment(recorder, request)
	}
	return recorder
}

// requireMetricsPreview asserts what evaluate answers about the metrics
// switch: whether its default is left to the engine on this launch, and
// whether the previewed command carries the flag.
func requireMetricsPreview(t *testing.T, recorder *httptest.ResponseRecorder, wantEngineDecides bool, wantFlag bool) dto.EvaluateResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("evaluate=%d: %s", recorder.Code, recorder.Body.String())
	}
	var response dto.EvaluateResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("evaluate not ok: %+v", response.Errors)
	}
	// A diffusion launch leaves the LLM server's keys to the engine as well;
	// the fixture's image tag names no release, so the metrics switch is
	// left with them.
	if got := slices.Contains(response.EngineDecidesParamKeys, "enable_metrics"); got != wantEngineDecides {
		t.Fatalf("engine_decides_param_keys=%v, enable_metrics left to the engine=%v, want %v", response.EngineDecidesParamKeys, got, wantEngineDecides)
	}
	command := append(append([]string(nil), response.Entrypoint...), response.Argv...)
	if got := countFlag(command, "--enable-metrics") > 0; got != wantFlag {
		t.Fatalf("metrics flag present=%v, want %v: %v", got, wantFlag, command)
	}
	return response
}

// A catalog spec writes booleans as bare switches next to valued flags. Reading
// the switch must not swallow the argument that follows it.
func TestCatalogMetricsSwitchDoesNotConsumeTheNextArgument(t *testing.T) {
	out, err := deploy.Materialize(deploy.MaterializeInput{
		Engine:       deploy.EngineSGLang,
		TemplateArgs: []string{"--enable-metrics", "--context-length", "4096"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Quick.EnableMetrics || out.Quick.MaxModelLen != 4096 || len(out.TemplateResidual) != 0 {
		t.Fatalf("quick=%+v residual=%v", out.Quick, out.TemplateResidual)
	}
}

func TestMetricsApplicabilityFlowsThroughEvaluateAndCreate(t *testing.T) {
	t.Run("diffusion leaves the switch to the engine and keeps a stated value", func(t *testing.T) {
		_, h, templateID := metricsAPIFixture(t, []string{"sglang", "serve"}, []string{"--model-type", "diffusion"})
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{}, nil, ""), true, false)
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"enable_metrics": false}, nil, ""), true, false)
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"enable_metrics": true}, nil, ""), true, true)
		created := metricsAPIRequest(t, h, "/api/v1/deployments", templateID, map[string]any{"enable_metrics": true}, nil, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("create=%d: %s", created.Code, created.Body.String())
		}
	})

	t.Run("a template that states the switch keeps it unless turned off", func(t *testing.T) {
		_, h, templateID := metricsAPIFixture(t, []string{"sglang", "serve"}, []string{"--model-type", "diffusion", "--enable-metrics"})
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{}, nil, ""), true, true)
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"enable_metrics": false}, nil, ""), true, false)
	})

	t.Run("LLM supports metrics whether or not the template enables them", func(t *testing.T) {
		_, offHandler, offTemplateID := metricsAPIFixture(t, []string{"python", "-m", "sglang.launch_server"}, nil)
		requireMetricsPreview(t, metricsAPIRequest(t, offHandler, "/api/v1/evaluate", offTemplateID, map[string]any{"enable_metrics": true}, nil, ""), false, true)

		_, onHandler, onTemplateID := metricsAPIFixture(t, []string{"python", "-m", "sglang.launch_server"}, []string{"--enable-metrics"})
		requireMetricsPreview(t, metricsAPIRequest(t, onHandler, "/api/v1/evaluate", onTemplateID, map[string]any{"enable_metrics": false}, nil, ""), false, false)
		created := metricsAPIRequest(t, onHandler, "/api/v1/deployments", onTemplateID, map[string]any{"enable_metrics": false}, nil, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("create=%d: %s", created.Code, created.Body.String())
		}
	})
}

// A diffusion launch states only the defaults its release's diffusion server
// takes, and the command carries exactly what the form shows: on SGLang
// v0.5.21 the metrics and cache-report switches default on, and the LLM
// server's defaults (--mem-fraction-static, ...) are left to the engine and
// absent from the command. Nothing is refused: a deployer who states a memory
// share gets it.
func TestDiffusionLaunchStatesOnlyWhatItsReleaseTakes(t *testing.T) {
	llmOnly := []string{"dtype", "enable_auto_tool_choice", "enforce_eager", "gpu_memory_utilization", "kv_cache_dtype", "lora"}
	diffusion := []string{"sglang", "serve"}
	for _, test := range []struct {
		name, image          string
		engineDecides, flags []string
	}{
		{"v0.5.21", "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", nil, []string{"--enable-metrics", "--enable-cache-report"}},
		{"v0.5.18", "gpuworks/gpuworks-runner:cuda12.9-sglang0.5.18-r2", []string{"enable_metrics"}, []string{"--enable-cache-report"}},
		{"release nobody recorded", "lmsysorg/sglang:latest", []string{"enable_cache_report", "enable_metrics"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, h, templateID := metricsAPIFixtureImage(t, test.image, diffusion, []string{"--model-type", "diffusion"})
			recorder := metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{}, nil, "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", recorder.Code, recorder.Body.String())
			}
			var response dto.EvaluateResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			want := append(append([]string(nil), llmOnly...), test.engineDecides...)
			slices.Sort(want)
			if !slices.Equal(response.EngineDecidesParamKeys, want) {
				t.Fatalf("engine_decides_param_keys=%v, want %v", response.EngineDecidesParamKeys, want)
			}
			command := append(append([]string(nil), response.Entrypoint...), response.Argv...)
			for _, flag := range []string{"--enable-metrics", "--enable-cache-report"} {
				if got, want := countFlag(command, flag) > 0, slices.Contains(test.flags, flag); got != want {
					t.Fatalf("%s present=%v, want %v: %v", flag, got, want, command)
				}
			}
			for _, flag := range []string{"--mem-fraction-static", "--context-length", "--chunked-prefill-size", "--max-running-requests"} {
				if countFlag(command, flag) > 0 {
					t.Fatalf("diffusion command carries %s: %v", flag, command)
				}
			}
			for _, key := range want {
				if _, stated := response.FormValues[key]; stated {
					t.Fatalf("form states the default of %s=%v", key, response.FormValues[key])
				}
			}
		})
	}

	t.Run("a stated memory share is kept", func(t *testing.T) {
		_, h, templateID := metricsAPIFixtureImage(t, "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", diffusion, []string{"--model-type", "diffusion"})
		recorder := metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"gpu_memory_utilization": 0.8}, nil, "")
		var response dto.EvaluateResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		command := append(append([]string(nil), response.Entrypoint...), response.Argv...)
		if !response.OK || !slices.Contains(command, "--mem-fraction-static") || response.FormValues["gpu_memory_utilization"] != 0.8 {
			t.Fatalf("ok=%v errors=%+v command=%v", response.OK, response.Errors, command)
		}
	})
}

func TestMetricsApplicabilityUsesTheFinalExplicitEntrypoint(t *testing.T) {
	t.Run("deployment override changes diffusion template to LLM", func(t *testing.T) {
		_, h, templateID := metricsAPIFixture(t, []string{"sglang", "serve"}, []string{"--model-type", "diffusion"})
		overrides := map[string]any{"entrypoint_display": `["python","-m","sglang.launch_server"]`}
		response := requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"enable_metrics": true}, overrides, ""), false, true)
		if !slices.Equal(response.Entrypoint, []string{"python", "-m", "sglang.launch_server"}) {
			t.Fatalf("entrypoint=%v, want deployment override", response.Entrypoint)
		}
	})

	t.Run("deployment override changes LLM template to diffusion", func(t *testing.T) {
		_, h, templateID := metricsAPIFixture(t, []string{"python", "-m", "sglang.launch_server"}, nil)
		overrides := map[string]any{"entrypoint_display": `["/usr/local/bin/sglang","serve"]`}
		requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{}, overrides, "--model-type llm --model-type=diffusion"), true, false)
	})

	t.Run("manual script remains opaque", func(t *testing.T) {
		_, h, templateID := metricsAPIFixture(t, []string{"sglang", "serve"}, []string{"--model-type", "diffusion"})
		overrides := map[string]any{
			"command_mode":       "manual",
			"entrypoint_display": `["/bin/sh"]`,
			"cmd_display":        "echo --model-type diffusion --enable-metrics",
		}
		response := requireMetricsPreview(t, metricsAPIRequest(t, h, "/api/v1/evaluate", templateID, map[string]any{"enable_metrics": true}, overrides, ""), false, false)
		if response.CommandMode != "manual" || !slices.Equal(response.Entrypoint, []string{"/bin/sh"}) {
			t.Fatalf("manual command=%v mode=%q", response.Command, response.CommandMode)
		}
	})
}

func TestCustomDeploymentMetricsApplicabilityUsesEntrypointOverride(t *testing.T) {
	st, err := store.Open("file:custom_metrics_applicability?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/custom-metrics", Name: "custom-metrics", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "metrics-node")
	h := &apiHandlers{d: Deps{Store: st}}
	request := func(entrypoint string, extraArgs string) *httptest.ResponseRecorder {
		body := map[string]any{
			"model_id": model.ID, "node_uuid": "metrics-node", "engine": deploy.EngineSGLang,
			"image": "private.example/sglang:custom", "quick_params": map[string]any{"enable_metrics": true},
			"container_overrides": map[string]any{"entrypoint_display": entrypoint}, "extra_args": extraArgs,
		}
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		recorder := httptest.NewRecorder()
		h.evaluate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", bytes.NewReader(raw)))
		return recorder
	}

	response := requireMetricsPreview(t, request(`["python","-m","sglang.launch_server"]`, ""), false, true)
	if !slices.Equal(response.Entrypoint, []string{"python", "-m", "sglang.launch_server"}) {
		t.Fatalf("custom LLM entrypoint=%v", response.Entrypoint)
	}
	// The request turned the switch on, so a diffusion override keeps it.
	requireMetricsPreview(t, request(`["/usr/local/bin/sglang","serve"]`, "--model-type diffusion"), true, true)
}
