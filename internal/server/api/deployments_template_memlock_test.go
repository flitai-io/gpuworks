package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The preview is the launch the instance will get, so the runtime block the
// deploy page and the instance detail show carries the ulimit the spec asked
// for, and the deployment's own memlock setting replaces it.
func TestEvaluatePreviewCarriesTheTemplateMemlockUlimit(t *testing.T) {
	st, err := store.Open("file:evaluate_template_memlock?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Memlock", Revision: "main", Name: "Acme/Memlock", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	spec := func(name string, memlock bool) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest", WeightBytes: 1 << 30, MemlockUnlimited: memlock,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Memlock"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/memlock", DisplayTitle: "Memlock", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []store.InferenceSpecRow{spec("pinned", true), spec("plain", false)},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/memlock")
	if err != nil {
		t.Fatal(err)
	}
	body := func(specName, extra string) string {
		return fmt.Sprintf(`{"template_id":%d,"template_spec_name":%q,"node_uuid":"field-errors-node"%s}`, template.ID, specName, extra)
	}
	for name, test := range map[string]struct {
		body string
		want []string // docker_args on the wire; nil when the key is absent
	}{
		"the spec asks for unlimited locked memory": {body("pinned", ""), []string{"--ulimit", "memlock=-1:-1"}},
		"a spec stating none adds nothing":          {body("plain", ""), nil},
		"the draft's own limit replaces the spec's": {body("pinned", `,"container_overrides":{"docker_args":["--ulimit","memlock=4096"]}`), []string{"--ulimit", "memlock=4096"}},
		"other docker args keep their place":        {body("pinned", `,"container_overrides":{"docker_args":["--cap-add","SYS_NICE"]}`), []string{"--cap-add", "SYS_NICE", "--ulimit", "memlock=-1:-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, test.body)
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			encoded, present := raw["docker_args"]
			if (test.want == nil) == present {
				t.Fatalf("docker_args = %s (present %v), want %v", encoded, present, test.want)
			}
			if test.want == nil {
				return
			}
			var got []string
			if err := json.Unmarshal(encoded, &got); err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("docker_args = %s (%v), want %v", encoded, err, test.want)
			}
		})
	}
}
