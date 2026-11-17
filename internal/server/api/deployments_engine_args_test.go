package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func engineArgsFixture(t *testing.T) (*apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/engine-args", Name: "engine-args", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "engine-args-node")
	if err := st.UpdateNodeStatus("engine-args-node", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "engine-args-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{
			{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true},
			{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, model.ID
}

func engineArgsRequest(t *testing.T, h *apiHandlers, modelID uint, create bool, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"model_id": modelID, "node_uuid": "engine-args-node", "engine": "sglang",
		"image": "lmsysorg/sglang:v0.5.21", "name": "engine-args", "served_name": "engine-args",
	}
	for key, value := range fields {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if create {
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(raw))))
	} else {
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(raw))))
	}
	return rec
}

func requireEngineArgsRefused(t *testing.T, rec *httptest.ResponseRecorder, create bool, field, detail string) {
	t.Helper()
	if create {
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), detail) {
			t.Fatalf("create=%d, want 400 naming %s: %s", rec.Code, detail, rec.Body.String())
		}
		return
	}
	var wire dto.EvaluateResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &wire) != nil || wire.OK {
		t.Fatalf("evaluate=%d, want 200/ok=false: %s", rec.Code, rec.Body.String())
	}
	if len(wire.Errors) != 1 || wire.Errors[0].Field != field || wire.Errors[0].Code != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(wire.Errors[0].Params), detail) {
		t.Fatalf("expected FIELD_INVALID beside %s naming %s: %s", field, detail, rec.Body.String())
	}
}

func TestEngineArgsRejectInvalidTargetEnumBeforeLaunch(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	for _, extra := range []string{
		"--kv-cache-dtype fp8", "--kv-cache-dtype=fp8", "--kv-cache-dtype",
		"--speculative-algorithm mtp", "--speculative-algorithm=draft_model",
		"--context-length not-a-number",
	} {
		t.Run(extra, func(t *testing.T) {
			flag, _, _ := strings.Cut(strings.Fields(extra)[0], "=")
			for _, create := range []bool{false, true} {
				rec := engineArgsRequest(t, h, modelID, create, map[string]any{"extra_args": extra})
				requireEngineArgsRefused(t, rec, create, "extra_args", flag)
			}
		})
	}
}

func TestEngineArgsWarnAnotherEnginesKnownFlagBeforeLaunch(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	// The 24 flag-bearing vLLM-only keys share 21 on flags (four KV disk
	// keys share --kv-transfer-config). Include the chunked-prefill off form,
	// glued -O3, and common keys whose spelling belongs to the other engine.
	// kv_disk_limit_gib is platform-only and covered by the typed matrix.
	for index, tc := range []struct{ engine, extra, flag string }{
		{"sglang", "--allowed-local-media-path /media", "--allowed-local-media-path"},
		{"sglang", "--api-server-count 2", "--api-server-count"},
		{"sglang", "--async-scheduling", "--async-scheduling"},
		{"sglang", "--cpu-offload-gb 4", "--cpu-offload-gb"},
		{"sglang", "--cpu-offload-gb 4 --cpu-offload-gb=4", "--cpu-offload-gb"},
		{"sglang", "--enable-chunked-prefill", "--enable-chunked-prefill"},
		{"sglang", "--enable-per-request-metrics", "--enable-per-request-metrics"},
		{"sglang", "--no-enable-chunked-prefill", "--no-enable-chunked-prefill"},
		{"sglang", "--kv-cache-memory-bytes 1048576", "--kv-cache-memory-bytes"},
		{"sglang", `--kv-transfer-config '{"kv_connector":"OffloadingConnector","kv_role":"kv_both","kv_connector_extra_config":{"spec_name":"TieringOffloadingSpec","cpu_bytes_to_use":8589934592,"secondary_tiers":[{"type":"fs","root_dir":"/cache","n_read_threads":32,"n_write_threads":32}]}}'`, "--kv-transfer-config"},
		{"sglang", "--kv-offloading-size 8", "--kv-offloading-size"},
		{"sglang", `--limit-mm-per-prompt '{"image":1}'`, "--limit-mm-per-prompt"},
		{"sglang", "--long-prefill-token-threshold 2048", "--long-prefill-token-threshold"},
		{"sglang", "--max-cudagraph-capture-size 32", "--max-cudagraph-capture-size"},
		{"sglang", `--media-io-kwargs '{}'`, "--media-io-kwargs"},
		{"sglang", "--mm-encoder-tp-mode data", "--mm-encoder-tp-mode"},
		{"sglang", `--mm-processor-kwargs '{}'`, "--mm-processor-kwargs"},
		{"sglang", "-O3", "-O"},
		{"sglang", "--performance-mode throughput", "--performance-mode"},
		{"sglang", "--prefix-caching-hash-algo sha256", "--prefix-caching-hash-algo"},
		{"sglang", `--rope-scaling '{"type":"yarn"}'`, "--rope-scaling"},
		{"sglang", "--stream-interval 2", "--stream-interval"},
		{"sglang", "--watermark 0.8", "--watermark"},
		{"sglang", "--max-model-len auto", "--max-model-len"},
		{"sglang", "--gpu-memory-utilization=0.9", "--gpu-memory-utilization"},
		{"sglang", "--max-num-seqs 8", "--max-num-seqs"},
		{"sglang", "--max-num-batched-tokens 8192", "--max-num-batched-tokens"},
		{"sglang", "--data-parallel-size 2", "--data-parallel-size"},
		{"sglang", "--enable-prefix-caching", "--enable-prefix-caching"},
		{"sglang", "--no-enable-prefix-caching", "--no-enable-prefix-caching"},
		{"sglang", "--enable-prompt-tokens-details", "--enable-prompt-tokens-details"},
		{"sglang", "--enforce-eager", "--enforce-eager"},
		{"sglang", `--speculative-config '{"method":"mtp"}'`, "--speculative-config"},
		{"vllm", "--context-length 16384", "--context-length"},
		{"vllm", "--disable-cuda-graph", "--disable-cuda-graph"},
	} {
		t.Run(tc.extra, func(t *testing.T) {
			fields := map[string]any{
				"engine": tc.engine, "image": "runner:test", "extra_args": tc.extra,
				"name": fmt.Sprintf("engine-args-warning-%d", index), "served_name": fmt.Sprintf("engine-args-warning-%d", index),
			}
			if tc.extra == "--data-parallel-size 2" {
				fields["placement"] = map[string]any{"gpus": []int{0, 1}, "tp": 1}
			}
			preview := engineArgsRequest(t, h, modelID, false, fields)
			var wire dto.EvaluateResponse
			if preview.Code != http.StatusOK || json.Unmarshal(preview.Body.Bytes(), &wire) != nil || !wire.OK {
				t.Fatalf("unverified is not a refusal: evaluate=%d: %s", preview.Code, preview.Body.String())
			}
			found := false
			for _, warning := range wire.Warnings {
				if warning.Code != "EXTRA_ARGS_ENGINE_UNVERIFIED" {
					continue
				}
				owner := "vllm"
				if tc.engine == "vllm" {
					owner = "sglang"
				}
				if found || warning.Params["flag"] != tc.flag || warning.Params["engine"] != tc.engine || warning.Params["documented_engine"] != owner || len(warning.Params) != 3 {
					t.Fatalf("unverified warning does not name the dictionary evidence exactly once: %+v", wire.Warnings)
				}
				found = true
			}
			if !found {
				t.Fatalf("missing unverified warning for %s: %+v", tc.flag, wire.Warnings)
			}
			created := engineArgsRequest(t, h, modelID, true, fields)
			if created.Code != http.StatusCreated {
				t.Fatalf("unverified flag prevented create=%d: %s", created.Code, created.Body.String())
			}
			var stored dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &stored); err != nil {
				t.Fatal(err)
			}
			args, err := deploy.ParseArgsText(stored.ExtraArgs)
			want, parseErr := deploy.ParseArgsText(tc.extra)
			if err != nil || parseErr != nil || !slices.Equal(args, want) {
				t.Fatalf("unverified extra_args were rewritten: %q, want %q", stored.ExtraArgs, tc.extra)
			}
		})
	}
}

func TestEngineArgsRejectUnsupportedStructuredValues(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	// Literal non-neutral examples for every published vLLM-only key. Values
	// the dictionary calls neutral are intentionally covered by a passing case.
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"allowed_local_media_path", "/media"},
		{"api_server_count", 2},
		{"async_scheduling", true},
		{"cpu_offload_gb", 2},
		{"enable_chunked_prefill", true},
		{"enable_request_metrics", false},
		{"kv_cache_memory_bytes", 1048576},
		{"kv_disk_limit_gib", 8},
		{"kv_disk_offload", true},
		{"kv_offload_read_threads", 32},
		{"kv_offload_write_threads", 32},
		{"kv_offloading_size", 8},
		{"limit_mm_per_prompt", `{"image":1}`},
		{"long_prefill_token_threshold", 2048},
		{"max_cudagraph_capture_size", 32},
		{"media_io_kwargs", `{}`},
		{"mm_encoder_tp_mode", "data"},
		{"mm_processor_kwargs", `{}`},
		{"optimization_level", "O1"},
		{"performance_mode", "throughput"},
		{"prefix_caching_hash_algo", "sha256"},
		{"rope_scaling", `{"type":"yarn"}`},
		{"shared_kv_dir", "/cache"},
		{"stream_interval", 2},
		{"watermark", 0.8},
		// Shared keys whose values are not interchangeable between engines.
		{"max_model_len", -1},
		{"kv_cache_dtype", "fp8"},
		{"speculative_algorithm", "mtp"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			for _, create := range []bool{false, true} {
				rec := engineArgsRequest(t, h, modelID, create, map[string]any{"quick_params": map[string]any{tc.key: tc.value}})
				requireEngineArgsRefused(t, rec, create, tc.key, tc.key)
			}
		})
	}
}

func TestEngineArgsKeepTargetValuesAndAdvancedPassthrough(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	for index, tc := range []struct {
		name   string
		fields map[string]any
		flags  map[string]string
	}{
		{"target flags and unknown future flags", map[string]any{"extra_args": "--context-length=16384 --mem-fraction-static 0.92 --future-custom-mode=on"}, map[string]string{"--context-length": "16384", "--mem-fraction-static": "0.92", "--future-custom-mode": "on"}},
		{"open parser plugins", map[string]any{"extra_args": "--reasoning-parser future_reasoning_plugin --tool-call-parser future_tool_plugin --foo"}, map[string]string{"--reasoning-parser": "future_reasoning_plugin", "--tool-call-parser": "future_tool_plugin"}},
		{"shared compatible values", map[string]any{"quick_params": map[string]any{"max_model_len": 16384, "kv_cache_dtype": "fp8_e4m3", "gpu_memory_utilization": 0.92, "enable_prefix_caching": false}}, map[string]string{"--context-length": "16384", "--kv-cache-dtype": "fp8_e4m3", "--mem-fraction-static": "0.92"}},
		{"case-normalized speculative method", map[string]any{"quick_params": map[string]any{"speculative_algorithm": "eagle3", "speculative_draft_model": "/models/draft", "speculative_tokens": 5}}, map[string]string{"--speculative-algorithm": "EAGLE3", "--speculative-draft-model-path": "/models/draft", "--speculative-num-draft-tokens": "5"}},
		{"neutral unsupported values keep their contract", map[string]any{"quick_params": map[string]any{"performance_mode": "balanced", "enable_request_metrics": true, "kv_offload_read_threads": 16, "kv_offload_write_threads": 16}}, nil},
		{"manual command owns its dialect", map[string]any{"extra_args": "--kv-cache-dtype fp8 --max-model-len auto", "container_overrides": map[string]any{"command_mode": "manual", "cmd_display": `["python3","custom_server.py"]`}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fields["name"] = fmt.Sprintf("engine-args-ok-%d", index)
			tc.fields["served_name"] = fmt.Sprintf("engine-args-ok-%d", index)
			preview := engineArgsRequest(t, h, modelID, false, tc.fields)
			var wire dto.EvaluateResponse
			if preview.Code != http.StatusOK || json.Unmarshal(preview.Body.Bytes(), &wire) != nil || !wire.OK {
				t.Fatalf("evaluate=%d: %s", preview.Code, preview.Body.String())
			}
			for _, warning := range wire.Warnings {
				if warning.Code == "EXTRA_ARGS_ENGINE_UNVERIFIED" {
					t.Fatalf("manual/target/unknown plugin argument received a foreign warning: %+v", warning)
				}
			}
			argv := append(append([]string(nil), wire.Entrypoint...), wire.Argv...)
			for flag, want := range tc.flags {
				if got, ok := flagValue(argv, flag); !ok || got != want || countFlag(argv, flag) != 1 {
					t.Errorf("%s=%q (%t), want %q exactly once: %v", flag, got, ok, want, argv)
				}
			}
			if tc.name == "shared compatible values" && countFlag(argv, "--disable-radix-cache") != 1 {
				t.Fatalf("explicit prefix-caching=false lost its SGLang spelling: %v", argv)
			}
			if tc.name == "manual command owns its dialect" && strings.Join(argv, " ") != "python3 custom_server.py" {
				t.Fatalf("manual command was interpreted as an engine command: %v", argv)
			}
			created := engineArgsRequest(t, h, modelID, true, tc.fields)
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
			var stored dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fmt.Sprint(tc.fields["extra_args"]), "--foo") && !strings.Contains(stored.ExtraArgs, "--foo") {
				t.Fatalf("unknown --foo was lost: %s", created.Body.String())
			}
			if strings.Contains(fmt.Sprint(tc.fields["extra_args"]), "--future-custom-mode=on") && !strings.Contains(stored.ExtraArgs, "--future-custom-mode=on") {
				t.Fatalf("unknown future argument was lost: %s", created.Body.String())
			}
			if tc.name == "manual command owns its dialect" && stored.ExtraArgs != tc.fields["extra_args"] {
				t.Fatalf("manual advanced arguments changed: %s", created.Body.String())
			}
		})
	}
}

func TestEngineArgsKeepCuratedTemplatePassthrough(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	fixture := deploymentFlatFixture()
	fixture.ModelID = "org/engine-args-template"
	fixture.Specs[0].Engine = "sglang"
	fixture.Specs[0].Image = "lmsysorg/sglang:v0.5.21"
	fixture.Specs[0].Args = `["--max-model-len","32768"]`
	fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"org/engine-args"}]`
	if err := h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := h.d.Store.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{"template_id": template.ID, "template_spec_name": fixture.Specs[0].Name}
	preview := engineArgsRequest(t, h, modelID, false, fields)
	var wire dto.EvaluateResponse
	if preview.Code != http.StatusOK || json.Unmarshal(preview.Body.Bytes(), &wire) != nil || !wire.OK {
		t.Fatalf("evaluate=%d: %s", preview.Code, preview.Body.String())
	}
	for _, warning := range wire.Warnings {
		if warning.Code == "EXTRA_ARGS_ENGINE_UNVERIFIED" {
			t.Fatalf("curated template received a user-argument warning: %+v", warning)
		}
	}
	argv := append(append([]string(nil), wire.Entrypoint...), wire.Argv...)
	if value, ok := flagValue(argv, "--max-model-len"); !ok || value != "32768" {
		t.Fatalf("curated template's passthrough changed: %v", argv)
	}
	created := engineArgsRequest(t, h, modelID, true, fields)
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d: %s", created.Code, created.Body.String())
	}
}

func TestEngineArgsPreviewPreservesOnlyUnchangedStoredPassthrough(t *testing.T) {
	h, modelID := engineArgsFixture(t)
	node, err := h.d.Store.NodeWithGPUs("engine-args-node")
	if err != nil {
		t.Fatal(err)
	}
	// A historical row can carry an advanced value that the current dictionary
	// cannot decode. Seeding it models an older deployment, not a new request
	// that should be allowed to bypass the write boundary.
	stored, err := h.d.Store.CreateDeployment(&store.Deployment{
		Name: "historical", ServedName: "historical", ModelID: modelID, NodeID: node.ID,
		Engine: "sglang", Image: "lmsysorg/sglang:v0.5.21", Replicas: 1,
		QuickParams: `{}`, ExtraArgs: "--kv-cache-dtype future_dtype", Placement: `{"tp":1,"gpus":[0]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, engine, extra string
		deploymentID        uint
		allowed             bool
	}{
		{"only a name changes", "sglang", "--kv-cache-dtype future_dtype", stored.ID, true},
		{"equivalent whitespace preserves the tokens", "sglang", "  --kv-cache-dtype\tfuture_dtype  ", stored.ID, true},
		{"new extra_args are checked", "sglang", "--kv-cache-dtype another_bad_dtype", stored.ID, false},
		{"another engine does not inherit the allowance", "vllm", "--kv-cache-dtype future_dtype", stored.ID, false},
		{"new deployment is checked", "sglang", "--kv-cache-dtype future_dtype", 0, false},
		{"missing deployment cannot grant an allowance", "sglang", "--kv-cache-dtype future_dtype", stored.ID + 99999, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := engineArgsRequest(t, h, modelID, false, map[string]any{
				"deployment_id": tc.deploymentID, "engine": tc.engine, "name": "renamed",
				"served_name": stored.ServedName, "extra_args": tc.extra,
			})
			if !tc.allowed {
				requireEngineArgsRefused(t, rec, false, "extra_args", "--kv-cache-dtype")
				return
			}
			var preview dto.EvaluateResponse
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &preview) != nil || !preview.OK {
				t.Fatalf("name-only preview rewrote the historical launch contract: %d %s", rec.Code, rec.Body.String())
			}
			argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
			if got, ok := flagValue(argv, "--kv-cache-dtype"); !ok || got != "future_dtype" || preview.FormValues["extra_args"] != stored.ExtraArgs {
				t.Fatalf("historical extra_args changed: argv=%v form=%v", argv, preview.FormValues)
			}
		})
	}
}
