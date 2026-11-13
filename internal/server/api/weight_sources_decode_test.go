package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// weightSourcesColumnValues are the shapes an inference_specs.weight_sources
// value can take, keyed by a short name. "partial" decodes its first element
// and fails on the second, which encoding/json still appends with the fields
// it could read.
var weightSourcesColumnValues = []struct{ name, raw string }{
	{"blank", ""},
	{"whitespace", "  "},
	{"null", "null"},
	{"empty", "[]"},
	{"undecodable", "not json"},
	{"partial", `[{"provider":"huggingface","repo_id":"org/A"},{"provider":"modelscope","repo_id":5}]`},
	{"valid", `[{"provider":"huggingface","repo_id":"org/A","revision":"r1"},{"provider":"modelscope","repo_id":"org/A-ms"}]`},
}

// The api reads a spec's declared weight sources in several places, each with
// its own tolerance for a blank or unreadable column. These pin what each one
// makes of every shape, as wire bytes where the result reaches a response
// (the acquisition providers' reading is pinned in package deployment).
func TestSpecWeightSourcesReadersKeepTheirHandlingOfEveryColumnShape(t *testing.T) {
	wantSpecSources := map[string]string{
		"blank": `null`, "whitespace": `null`, "null": `null`, "empty": `[]`, "undecodable": `null`,
		"partial": `[{"provider":"huggingface","repo_id":"org/A"},{"provider":"modelscope","repo_id":""}]`,
		"valid":   `[{"provider":"huggingface","repo_id":"org/A","revision":"r1"},{"provider":"modelscope","repo_id":"org/A-ms"}]`,
	}
	for _, value := range weightSourcesColumnValues {
		t.Run(value.name, func(t *testing.T) {
			spec := &store.InferenceSpecRow{Name: "s", WeightSources: value.raw}

			encoded, err := json.Marshal(deployment.SpecWeightSources(spec))
			if err != nil || string(encoded) != wantSpecSources[value.name] {
				t.Fatalf("deployment.SpecWeightSources = %s, %v; want %s", encoded, err, wantSpecSources[value.name])
			}

			view := inferenceTemplateView(store.InferenceModel{ModelID: "org/a", Specs: []store.InferenceSpecRow{*spec}})
			encoded, err = json.Marshal(view.Specs[0].WeightSources)
			if err != nil || string(encoded) != wantSpecSources[value.name] {
				t.Fatalf("template view weight_sources = %s, %v; want %s", encoded, err, wantSpecSources[value.name])
			}
		})
	}
	if got := deployment.SpecWeightSources(nil); got != nil {
		t.Fatalf("deployment.SpecWeightSources(nil) = %#v, want nil", got)
	}
}

// The template index lists each profile's declared repositories; a profile
// whose column yields none carries no weight_repo_ids key at all.
func TestInferenceTemplateIndexWeightRepoIDsForEveryColumnShape(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedWeightSourcesColumnValues(t, st)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listInferenceTemplateIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/index", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var wire struct {
		Items []struct {
			Profiles []struct {
				Name          string           `json:"name"`
				WeightRepoIDs *json.RawMessage `json:"weight_repo_ids"`
			} `json:"profiles"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || len(wire.Items) != 1 {
		t.Fatalf("index = %s, %v", rec.Body, err)
	}
	got := map[string]string{}
	for _, profile := range wire.Items[0].Profiles {
		got[profile.Name] = "absent"
		if profile.WeightRepoIDs != nil {
			got[profile.Name] = string(*profile.WeightRepoIDs)
		}
	}
	want := map[string]string{
		"blank": "absent", "whitespace": "absent", "null": "absent", "empty": "absent", "undecodable": "absent",
		"partial": `["org/A"]`, "valid": `["org/A","org/A-ms"]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weight_repo_ids = %v, want %v", got, want)
	}
}

// seedWeightSourcesColumnValues stores catalog model org/a with one spec per
// column shape, named after it. The store refuses a blank or malformed column
// on write, so those are put in place underneath it, as a damaged database
// would hold them.
func seedWeightSourcesColumnValues(t *testing.T, st *store.Store) {
	t.Helper()
	model := flatAPIFixture("org/a", "unused")
	model.Specs = nil
	for _, value := range weightSourcesColumnValues {
		model.Specs = append(model.Specs, flatAPIFixture("org/a", value.name).Specs[0])
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	for _, value := range weightSourcesColumnValues {
		if err := st.DB().Model(&store.InferenceSpecRow{}).Where("name = ?", value.name).Update("weight_sources", value.raw).Error; err != nil {
			t.Fatal(err)
		}
	}
}
