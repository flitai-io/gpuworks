package seed

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const strataModelID = "ista-daslab/qwen3.8-flash-next-strata"

// roundTrip renders row index of the catalog as the editor's YAML, decodes it
// the way the import endpoint does, and projects it back to the row the import
// would store. The document is validated together with the models its specs
// mount as components, as the import endpoint does with the models the server
// already has. rows is the catalog's own projection.
func roundTrip(t *testing.T, catalog inferencecatalog.FlatCatalog, rows []store.InferenceModel, index int) (text string, got store.InferenceModel) {
	t.Helper()
	id := rows[index].ModelID
	data, err := inferencecatalog.EditableYAML(rows[index], "")
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	decoded, err := inferencecatalog.DecodeYAML(data)
	if err != nil {
		t.Fatalf("%s: editor YAML does not decode: %v\n%s", id, err, data)
	}
	if len(decoded.Models) != 1 || decoded.Models[0].Source != "user" {
		t.Fatalf("%s: want one source:user model, got %+v", id, decoded.Models)
	}
	// The import stores the model under the owner it already has; the editor's
	// document always says user.
	decoded.Models[0].Source = catalog.Models[index].Source
	context := inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{decoded.Models[0]}}
	needed := map[string]bool{}
	for _, spec := range decoded.Models[0].Specs {
		for _, component := range spec.Components {
			needed[strings.ToLower(component.ModelID)] = true
		}
	}
	for _, model := range catalog.Models {
		if needed[model.ModelID] && model.ModelID != id {
			context.Models = append(context.Models, model)
		}
	}
	position := 0
	if len(decoded.Models[0].Specs) == 0 {
		// A component-only model is valid only while some model mounts it.
		context.Models = append([]inferencecatalog.InferenceModel(nil), catalog.Models...)
		context.Models[index] = decoded.Models[0]
		position = index
	}
	again, err := inferencecatalog.ToFlatStoreModels(context)
	if err != nil {
		t.Fatalf("%s: editor YAML does not import: %v\n%s", id, err, data)
	}
	return string(data), again[position]
}

// Every shipped template must survive the editor unchanged: the YAML text is
// written by the server from the types the importer reads, so no field the
// catalog has can be dropped or invented on the way through.
func TestEditableYAMLRoundTripsEverySeedTemplate(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := inferencecatalog.ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for index := range catalog.Models {
		_, got := roundTrip(t, catalog, rows, index)
		if want := rows[index]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s changed through the editor:\n got %+v\nwant %+v", want.ModelID, got, want)
		}
	}
}

// The Strata template declares its 12 GiB per-card admission in the
// placement resolver. A cloned copy whose admission line was lost in the
// editor was sized on its 63 GiB of weights and refused.
func TestEditableYAMLKeepsStrataAdmissionContract(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := inferencecatalog.ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	index := -1
	for i, model := range catalog.Models {
		if model.ModelID == strataModelID {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("%s is not in the seed catalog", strataModelID)
	}
	text, got := roundTrip(t, catalog, rows, index)
	if !strings.Contains(text, "accept(gpu_memory_mib_per_device=12288)") {
		t.Fatalf("editor YAML lost the admission contract:\n%s", text)
	}
	if len(got.Specs) != len(rows[index].Specs) {
		t.Fatal("editor round trip changed the number of Strata specs")
	}
	for i, spec := range got.Specs {
		if spec.Placement != rows[index].Specs[i].Placement || spec.WeightBytes != rows[index].Specs[i].WeightBytes {
			t.Errorf("editor round trip changed %s admission or weight bytes", spec.Name)
		}
	}
	// The declared metrics mapping and placement resolver travel as written.
	for _, want := range []string{"format: json", "live.state!=idle: requests_running", "resolver: |"} {
		if !strings.Contains(text, want) {
			t.Errorf("editor YAML lacks %q:\n%s", want, text)
		}
	}
}
