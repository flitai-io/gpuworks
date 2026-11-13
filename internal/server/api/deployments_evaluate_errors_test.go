package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateForFieldErrors runs one evaluate call and decodes the wire object,
// so assertions read the same keys a client reads.
func evaluateForFieldErrors(t *testing.T, h *apiHandlers, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not a JSON object: %s", rec.Body.String())
	}
	return rec.Code, raw
}

func fieldErrorsOf(t *testing.T, raw map[string]json.RawMessage) []dto.FieldError {
	t.Helper()
	encoded, ok := raw["errors"]
	if !ok {
		return nil
	}
	var errors []dto.FieldError
	if err := json.Unmarshal(encoded, &errors); err != nil {
		t.Fatal(err)
	}
	return errors
}

// The wire form of one field error is exactly three keys: the field path, the
// stable code and (only when the rule has interpolation) the params. Literal
// key/value assertions keep a renamed field or a dropped code from reaching
// clients silently.
func TestFieldErrorWireShape(t *testing.T) {
	encoded, err := json.Marshal(dto.FieldError{Field: "max_model_len", Code: "LAUNCH_CONFIG_CONFLICT", Params: map[string]any{"key": "max_model_len"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"field":"max_model_len","code":"LAUNCH_CONFIG_CONFLICT","params":{"key":"max_model_len"}}` {
		t.Fatalf("wire form = %s", encoded)
	}
	omitted, err := json.Marshal(dto.FieldError{Field: "replicas", Code: "REPLICAS_NEGATIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if string(omitted) != `{"field":"replicas","code":"REPLICAS_NEGATIVE"}` {
		t.Fatalf("params must be omitted when absent: %s", omitted)
	}
}

// A clean evaluation carries no errors key at all: the field is omitempty, so
// old clients see the response they always saw.
func TestEvaluateErrorsOmittedWhenClean(t *testing.T) {
	st, err := store.Open("file:evaluate_errors_omitted?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/clean", Revision: "main", Name: "clean", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm"}`, model.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, present := raw["errors"]; present {
		t.Fatalf("errors must be omitted on a clean evaluation: %s", raw["errors"])
	}
	if string(raw["schema_version"]) != "1" {
		t.Fatalf("schema_version = %s", raw["schema_version"])
	}
}

// Every rule the deploy form used to judge client-side is judged by evaluate
// and answered as a field error on a 200/ok=false response. Each case pairs a
// failing draft with the code and field it must produce; the passing twin for
// each rule is the fixture's clean evaluation above and the create cases the
// 400 contract tests already cover.
func TestEvaluateCollectsFieldErrorsInsteadOfRejecting(t *testing.T) {
	st, err := store.Open("file:evaluate_field_errors?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/field-errors", Revision: "main", Name: "field-errors", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	for name, tc := range map[string]struct {
		body string
		want dto.FieldError
	}{
		"unterminated quote in extra_args": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","extra_args":"--foo 'bar"}`, model.ID),
			want: dto.FieldError{Field: "extra_args", Code: ErrorExtraArgsInvalid},
		},
		"unknown engine": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"nosuchengine"}`, model.ID),
			want: dto.FieldError{Field: "engine", Code: ErrorEngineInvalid, Params: map[string]any{"engine": "nosuchengine"}},
		},
		"unknown quick_params_unset key": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params_unset":["not_a_key"]}`, model.ID),
			want: dto.FieldError{Field: "quick_params_unset", Code: ErrorFieldInvalid},
		},
		"omni json parameter is not valid json": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"limit_mm_per_prompt":"{bad"}}`, model.ID),
			want: dto.FieldError{Field: "limit_mm_per_prompt", Code: ErrorQuickParamJSON, Params: map[string]any{"key": "limit_mm_per_prompt"}},
		},
		"media path is not absolute": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"allowed_local_media_path":"relative/media"}}`, model.ID),
			want: dto.FieldError{Field: "allowed_local_media_path", Code: ErrorQuickParamPath, Params: map[string]any{"key": "allowed_local_media_path"}},
		},
		"mount container path is relative": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":[{"host_path":"/data","container_path":"data"}]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerMounts, Code: ErrorMountContainerPath, Params: map[string]any{"target": "data"}},
		},
		"mount host path is relative": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":[{"host_path":"data","container_path":"/data"}]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerMounts, Code: ErrorMountHostPath, Params: map[string]any{"source": "data"}},
		},
		"mount duplicates the model mount": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":[{"host_path":"/elsewhere","container_path":"/model"}]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerMounts, Code: "MOUNT_TARGET_DUPLICATE", Params: map[string]any{"target": "/model"}},
		},
		"mount targets nest": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":[{"host_path":"/a","container_path":"/a"},{"host_path":"/b","container_path":"/a/b"}]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerMounts, Code: ErrorMountTargetNested, Params: map[string]any{"parent": "/a", "child": "/a/b"}},
		},
		"mount rows do not decode": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":"not-a-list"}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerMounts, Code: ErrorFieldInvalid},
		},
		"docker args restate the shared memory field": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"docker_args":["--shm-size=8g"]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerDocker, Code: ErrorDockerArgUseShmField, Params: map[string]any{"arg": "--shm-size=8g", "option": "--shm-size"}},
		},
		"docker args restate a platform-managed option": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"docker_args":["--network","bridge"]}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerDocker, Code: ErrorDockerArgPlatformManaged, Params: map[string]any{"arg": "--network", "option": "--network"}},
		},
		"shared memory size under host IPC": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"docker_args":["--ipc=host"],"shm_bytes":34359738368}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerShm, Code: ErrorShmSizeWithHostIPC, Params: map[string]any{"arg": "--ipc=host"}},
		},
		"shared memory size below the CPU KV tier": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"kv_offloading_size":19},"container_overrides":{"shm_bytes":17179869184}}`, model.ID),
			want: dto.FieldError{Field: deployment.FieldContainerShm, Code: "SHM_BELOW_KV_CPU_TIER", Params: map[string]any{"shm_gib": 16, "tier_gib": 19}},
		},
		// A selector inside the placement is refused, never ignored: it
		// moved to node_requirements, and reading past it would widen the
		// draft's candidates.
		"placement still carries a selector": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","placement":{"node_labels":{"pool":"a"}}}`, model.ID),
			want: dto.FieldError{Field: "placement", Code: ErrorFieldInvalid},
		},
		"node requirement names an unknown system key": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","node_requirements":{"gpuworks.ai/gpu_model":["A100"]}}`, model.ID),
			want: dto.FieldError{Field: "node_requirements", Code: ErrorNodeRequirementsInvalid},
		},
		"named node does not satisfy the requirements": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","node_requirements":{"pool":["elsewhere"]}}`, model.ID),
			want: dto.FieldError{Field: "node_requirements", Code: ErrorNodeRequirementsUnmet},
		},
		"placement strategy unknown": {
			body: fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","placement_strategy":"random"}`, model.ID),
			want: dto.FieldError{Field: "placement_strategy", Code: ErrorPlacementStrategyInvalid, Params: map[string]any{"strategy": "random"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			if string(raw["ok"]) != "false" {
				t.Fatalf("ok = %s, want false whenever errors are present", raw["ok"])
			}
			errors := fieldErrorsOf(t, raw)
			if len(errors) != 1 {
				t.Fatalf("errors = %+v, want exactly the one this rule reports", errors)
			}
			got := errors[0]
			if got.Field != tc.want.Field || got.Code != tc.want.Code {
				t.Fatalf("field error = %+v, want %+v", got, tc.want)
			}
			for key, value := range tc.want.Params {
				if fmt.Sprint(got.Params[key]) != fmt.Sprint(value) {
					t.Fatalf("params[%q] = %v, want %v (all: %+v)", key, got.Params[key], value, got.Params)
				}
			}
		})
	}
}

// The mount rules that pass must reach the preview unchanged: a valid extra
// mount is carried into the assembled response, proving the validator is a
// gate and not a blanket rejection.
func TestEvaluateAcceptsValidExtraMounts(t *testing.T) {
	st, err := store.Open("file:evaluate_mounts_valid?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/mounts-valid", Revision: "main", Name: "mounts-valid", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"extra_mounts":[{"host_path":"/data","container_path":"/data"}]}}`, model.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, present := raw["errors"]; present {
		t.Fatalf("valid mount produced errors: %s", raw["errors"])
	}
	if string(raw["ok"]) != "true" {
		t.Fatalf("ok = %s body = %s", raw["ok"], raw)
	}
}

// A stated /dev/shm size reaches the preview as the resolved launch's value,
// and an unstated one shows what the container gets: the engine profile default,
// plus the CPU KV tier when the draft enables one (the engine keeps that tier in
// /dev/shm). Under --ipc=host the launch states no size at all.
func TestEvaluatePreviewCarriesTheResolvedShmSize(t *testing.T) {
	st, err := store.Open("file:evaluate_shm?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/shm", Revision: "main", Name: "shm", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	for body, want := range map[string]string{
		fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm"}`, model.ID):                                                 "17179869184",
		fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"shm_bytes":34359738368}}`, model.ID): "34359738368",
		// 16 GiB + 19 GiB: the very sizes of the measured startup failure.
		fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"kv_offloading_size":19}}`, model.ID):                                                      "37580963840",
		fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"kv_offloading_size":19},"container_overrides":{"shm_bytes":68719476736}}`, model.ID):      "68719476736",
		fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","quick_params":{"kv_offloading_size":19},"container_overrides":{"docker_args":["--ipc=host"]}}`, model.ID): "",
	} {
		status, raw := evaluateForFieldErrors(t, h, body)
		if status != http.StatusOK || string(raw["ok"]) != "true" {
			t.Fatalf("status = %d body = %s", status, raw)
		}
		if got, present := raw["shm_bytes"]; string(got) != want || (want == "") == present {
			t.Fatalf("shm_bytes = %s (present %v), want %q for %s", got, present, want, body)
		}
	}
}

// Two independent rule violations are reported together, because the form
// shows every field's error in one round trip.
func TestEvaluateCollectsIndependentFieldErrors(t *testing.T) {
	st, err := store.Open("file:evaluate_errors_combined?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/combined", Revision: "main", Name: "combined", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"nosuchengine","replicas":-1,"target_vendor":"strange"}`, model.ID)
	status, raw := evaluateForFieldErrors(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	errors := fieldErrorsOf(t, raw)
	fields := map[string]bool{}
	for _, item := range errors {
		fields[item.Field] = true
	}
	for _, want := range []string{"replicas", "target_vendor", "engine"} {
		if !fields[want] {
			t.Fatalf("errors = %+v, want a %q entry", errors, want)
		}
	}
}

func evaluateFieldErrorFixture(t *testing.T, st *store.Store) *apiHandlers {
	t.Helper()
	if _, err := st.RegisterNode("field-errors-node", store.HashToken("token"), "field-errors-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("field-errors-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "field-errors-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA GPU", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 16 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st}}
}

// FormValues is the display instruction: the effective configuration with the
// dictionary defaults already merged, plus the argument text after the moved
// flags left it. Literal key/value assertions keep the projection honest.
func TestEvaluateFormValuesAndDefaultsWire(t *testing.T) {
	st, err := store.Open("file:evaluate_form_values?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/form-values", Revision: "main", Name: "form-values", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","extra_args":"--max-model-len 8192 --foo"}`, model.ID)
	status, raw := evaluateForFieldErrors(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var response struct {
		FormValues map[string]any        `json:"form_values"`
		Defaults   *dto.EvaluateDefaults `json:"defaults"`
	}
	if err := json.Unmarshal(raw["form_values"], &response.FormValues); err != nil {
		t.Fatal(err)
	}
	// The extra_args flag moved into its field: the box keeps only the token
	// the dictionary does not own, and the field shows the moved value.
	if fmt.Sprint(response.FormValues["max_model_len"]) != "8192" {
		t.Fatalf("form_values must show the moved value: %v", response.FormValues)
	}
	if fmt.Sprint(response.FormValues["extra_args"]) != "--foo" {
		t.Fatalf("form_values extra_args must hold the user residual: %v", response.FormValues["extra_args"])
	}
	// The dictionary's defaults are part of what the form displays, as the
	// node's card allows them: this fixture's card is 16 GiB, where leaving the
	// 2 GiB reserve caps vLLM's own 0.9 at 0.87. Asserting 0.9 here would
	// assert a share the form must not show for this node.
	if fmt.Sprint(response.FormValues["gpu_memory_utilization"]) != "0.87" {
		t.Fatalf("form_values must carry the vllm defaults as this card allows them: %v", response.FormValues)
	}
	if err := json.Unmarshal(raw["defaults"], &response.Defaults); err != nil {
		t.Fatal(err)
	}
	if response.Defaults == nil || response.Defaults.ModelMount != "/model" || response.Defaults.MasterPort != 29500 || response.Defaults.Engine != "vllm" {
		t.Fatalf("defaults = %+v", response.Defaults)
	}
	var moved []map[string]any
	if err := json.Unmarshal(raw["moved"], &moved); err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0]["key"] != "max_model_len" {
		t.Fatalf("moved = %v", moved)
	}
}

// A clean evaluation with nothing stated still answers the defaults the form
// needs before the first keystroke: form_values holds the dictionary defaults
// and defaults holds the mount/port/engine constants.
func TestEvaluateFormValuesCarryDictionaryDefaults(t *testing.T) {
	st, err := store.Open("file:evaluate_form_defaults?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/form-defaults", Revision: "main", Name: "form-defaults", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm"}`, model.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var formValues map[string]any
	if err := json.Unmarshal(raw["form_values"], &formValues); err != nil {
		t.Fatal(err)
	}
	// 0.87 rather than vLLM's 0.9: the fixture's card is 16 GiB and the form is
	// shown the share that still leaves its reserve.
	for key, want := range map[string]string{"gpu_memory_utilization": "0.87", "dtype": "auto", "kv_cache_dtype": "auto", "performance_mode": "balanced"} {
		if fmt.Sprint(formValues[key]) != want {
			t.Fatalf("form_values[%q] = %v, want %v (all: %v)", key, formValues[key], want, formValues)
		}
	}
	if _, present := formValues["enable_chunked_prefill"]; present {
		t.Fatalf("a row without a default must stay out of form_values: %v", formValues)
	}
}
