package api

import (
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluatePreviewCarriesTheTemplateShmSize(t *testing.T) {
	st, err := store.Open("file:evaluate_template_shm?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Shm", Revision: "main", Name: "Acme/Shm", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	spec := func(name string, shmBytes int64) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest", WeightBytes: 1 << 30, ShmBytes: shmBytes,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Shm"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/shm", DisplayTitle: "Shm", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []store.InferenceSpecRow{spec("large-shm", 400<<30), spec("small-shm", 4<<30), spec("no-shm", 0)},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/shm")
	if err != nil {
		t.Fatal(err)
	}
	body := func(specName, extra string) string {
		return fmt.Sprintf(`{"template_id":%d,"template_spec_name":%q,"node_uuid":"field-errors-node"%s}`, template.ID, specName, extra)
	}
	for name, test := range map[string]struct {
		body string
		want string // wire value of shm_bytes; empty when the key is absent
	}{
		"template above the engine default":             {body("large-shm", ""), "429496729600"},
		"template below the engine default":             {body("small-shm", ""), "17179869184"},
		"template stating none":                         {body("no-shm", ""), "17179869184"},
		"template above default plus the CPU tier":      {body("large-shm", `,"quick_params":{"kv_offloading_size":19}`), "429496729600"},
		"template below default plus the CPU tier":      {body("small-shm", `,"quick_params":{"kv_offloading_size":19}`), "37580963840"},
		"the draft's own size wins over the template":   {body("large-shm", `,"container_overrides":{"shm_bytes":68719476736}`), "68719476736"},
		"the draft's own smaller size wins as well":     {body("large-shm", `,"container_overrides":{"shm_bytes":8589934592}`), "8589934592"},
		"host IPC states no size whatever the template": {body("large-shm", `,"container_overrides":{"docker_args":["--ipc=host"]}`), ""},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, test.body)
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			if got, present := raw["shm_bytes"]; string(got) != test.want || (test.want == "") == present {
				t.Fatalf("shm_bytes = %s (present %v), want %q", got, present, test.want)
			}
		})
	}

	// A size the draft states that cannot hold the CPU tier is still refused,
	// whatever the template asks for.
	status, raw := evaluateForFieldErrors(t, h, body("large-shm", `,"quick_params":{"kv_offloading_size":19},"container_overrides":{"shm_bytes":8589934592}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	errs := fieldErrorsOf(t, raw)
	if len(errs) != 1 || errs[0].Code != "SHM_BELOW_KV_CPU_TIER" {
		t.Fatalf("errors = %+v, want SHM_BELOW_KV_CPU_TIER", errs)
	}
}
