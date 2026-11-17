package inferencecatalog

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// tolerantPackage is a package of several models, written the way a newer
// build would publish it. edit receives each model's YAML and may add what
// this build does not know.
func tolerantPackage(t *testing.T, models []InferenceModel, edit func(modelID, yaml string) string) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	specs := 0
	for _, model := range models {
		data, err := MarshalModelYAML(model)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if edit != nil {
			text = edit(model.ModelID, text)
		}
		files["models/"+model.ModelID+"/model.yaml"] = &fstest.MapFile{Data: []byte(text)}
		specs += len(model.Specs)
	}
	files["manifest.yaml"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("schema_version: %d\npublished_at: 2026-08-31T00:00:00Z\nmodels: %d\nspecs: %d\n", PackageSchemaVersion, len(models), specs))}
	return files
}

// tolerantModel is a valid model of two specs; the second is named "next".
func tolerantModel(id string) InferenceModel {
	model := flatFixture().Models[0]
	model.ModelID = id
	next := model.Specs[0]
	next.Name, next.DisplayNameZHCN, next.DisplayNameEN = "next", "下一代", "Next"
	model.Specs = []InferenceSpec{model.Specs[0], next}
	return model
}

// afterSpec inserts a line into the spec named name, right under its name.
func afterSpec(t *testing.T, yaml, name, line string) string {
	t.Helper()
	marker := "- name: " + name + "\n"
	index := strings.Index(yaml, marker)
	if index < 0 {
		t.Fatalf("spec %s not found in\n%s", name, yaml)
	}
	indent := strings.Repeat(" ", index-strings.LastIndex(yaml[:index], "\n")-1+2)
	return yaml[:index+len(marker)] + indent + line + "\n" + yaml[index+len(marker):]
}

func skippedNames(skipped []Skipped) []string {
	names := make([]string, len(skipped))
	for i, item := range skipped {
		names[i] = item.ModelID + "#" + item.Spec
	}
	sort.Strings(names)
	return names
}

func TestLoadPackageTolerantOfACleanPackageMatchesTheStrictLoad(t *testing.T) {
	files := tolerantPackage(t, []InferenceModel{tolerantModel("org/a"), tolerantModel("org/b")}, nil)
	strict, err := LoadPackage(files)
	if err != nil {
		t.Fatal(err)
	}
	tolerant, skipped, err := LoadPackageTolerant(files)
	if err != nil || len(skipped) != 0 || !reflect.DeepEqual(strict, tolerant) {
		t.Fatalf("err=%v skipped=%+v equal=%v", err, skipped, reflect.DeepEqual(strict, tolerant))
	}
}

func TestLoadPackageTolerantLeavesOutWhatThisBuildCannotRead(t *testing.T) {
	asset := []byte("print('ready')\n")
	const assetSHA = "e13cdff7de4a58aa927d0812d424a163af37d0619bebb12c0e68f042cd02a511"
	field := tolerantModel("org/field")
	field.Specs[1].Assets = []Asset{{File: "assets/next.py", SHA256: assetSHA, UnpackTo: "/opt/gpuworks/next.py"}}
	value := tolerantModel("org/value")
	whole := tolerantModel("org/whole")
	fine := tolerantModel("org/fine")
	files := tolerantPackage(t, []InferenceModel{field, value, whole, fine}, func(modelID, yaml string) string {
		switch modelID {
		case "org/field":
			return afterSpec(t, yaml, "next", "field_of_a_newer_server: true")
		case "org/value":
			return strings.Replace(yaml, "scenario: balanced", "scenario: not-yet-invented", 1)
		case "org/whole":
			return "model_field_of_a_newer_server: 1\n" + yaml
		}
		return yaml
	})
	files["models/org/field/assets/next.py"] = &fstest.MapFile{Data: asset}

	if _, err := LoadPackage(files); err == nil {
		t.Fatal("the strict load accepted the package")
	}
	pkg, skipped, err := LoadPackageTolerant(files)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := skippedNames(skipped), []string{"org/field#next", "org/value#bf16-balanced", "org/whole#"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v (%+v)", got, want, skipped)
	}
	for _, item := range skipped {
		if item.Reason == "" || strings.Contains(item.Reason, "\n") {
			t.Fatalf("reason of %s#%s = %q", item.ModelID, item.Spec, item.Reason)
		}
	}
	kept := map[string][]string{}
	for _, model := range pkg.Catalog.Models {
		for _, spec := range model.Specs {
			kept[model.ModelID] = append(kept[model.ModelID], spec.Name)
		}
	}
	want := map[string][]string{"org/field": {"bf16-balanced"}, "org/value": {"next"}, "org/fine": {"bf16-balanced", "next"}}
	if !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	// The manifest still states the package as it was published.
	if pkg.Manifest.Models != 4 || pkg.Manifest.Specs != 8 {
		t.Fatalf("manifest = %+v", pkg.Manifest)
	}

	var archive bytes.Buffer
	if err := PackPackage(&archive, files); err != nil {
		t.Fatalf("a stored package with skipped specs does not pack: %v", err)
	}
	if _, again, err := ReadPackageArchiveTolerant(&archive); err != nil || !reflect.DeepEqual(skippedNames(again), skippedNames(skipped)) {
		t.Fatalf("archive: err=%v skipped=%+v", err, again)
	}
}

// A spec may reuse a block another spec anchors, and a model may lose every
// spec it stated: neither is a reason of its own.
func TestLoadPackageTolerantReadsAliasesAndNamesAModelOnce(t *testing.T) {
	anchored := tolerantModel("org/anchored")
	emptied := tolerantModel("org/emptied")
	files := tolerantPackage(t, []InferenceModel{anchored, emptied, tolerantModel("org/fine")}, func(modelID, yaml string) string {
		switch modelID {
		case "org/anchored":
			// The first spec anchors its args and is itself unreadable; the
			// second, readable, spec refers to them.
			first := strings.Index(yaml, "args:")
			second := strings.LastIndex(yaml, "args:")
			if first < 0 || first == second {
				t.Fatalf("fixture has no two args blocks:\n%s", yaml)
			}
			end := second + strings.Index(yaml[second:], "\n") + 1
			for strings.HasPrefix(strings.TrimLeft(yaml[end:], " "), "- ") && !strings.HasPrefix(strings.TrimLeft(yaml[end:], " "), "- name:") {
				end += strings.Index(yaml[end:], "\n") + 1
			}
			yaml = yaml[:second] + "args: *shared\n" + yaml[end:]
			yaml = yaml[:first] + "args: &shared" + yaml[first+len("args:"):]
			return afterSpec(t, yaml, "bf16-balanced", "field_of_a_newer_server: true")
		case "org/emptied":
			return afterSpec(t, afterSpec(t, yaml, "next", "field_of_a_newer_server: true"), "bf16-balanced", "field_of_a_newer_server: true")
		}
		return yaml
	})
	pkg, skipped, err := LoadPackageTolerant(files)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := skippedNames(skipped), []string{"org/anchored#bf16-balanced", "org/emptied#bf16-balanced", "org/emptied#next"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v (%+v)", got, want, skipped)
	}
	for _, model := range pkg.Catalog.Models {
		if model.ModelID == "org/anchored" {
			if len(model.Specs) != 1 || model.Specs[0].Name != "next" || !reflect.DeepEqual(model.Specs[0].Args, anchored.Specs[0].Args) {
				t.Fatalf("anchored = %+v", model.Specs)
			}
			return
		}
	}
	t.Fatal("org/anchored was not kept")
}

func TestLoadPackageTolerantFollowsComponentReferences(t *testing.T) {
	target := flatFixture().Models[0]
	target.ModelID = "org/target"
	target.Specs[0].Args = append(target.Specs[0].Args, "--speculative-config", `{"model":"/models/draft"}`)
	target.Specs[0].Components = []Component{
		{Name: "target", Role: "primary", Weights: target.Specs[0].Weights, WeightBytes: target.Specs[0].WeightBytes, MountPath: "/models/target"},
		{Name: "draft", Role: "speculative_draft", ModelID: "org/draft", Weights: target.Specs[0].Weights, WeightBytes: target.Specs[0].WeightBytes, MountPath: "/models/draft"},
	}
	draft := target
	draft.ModelID = "org/draft"
	draft.Specs = nil
	files := tolerantPackage(t, []InferenceModel{target, draft, tolerantModel("org/fine")}, func(modelID, yaml string) string {
		if modelID == "org/draft" {
			return "model_field_of_a_newer_server: 1\n" + yaml
		}
		return yaml
	})
	pkg, skipped, err := LoadPackageTolerant(files)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := skippedNames(skipped), []string{"org/draft#", "org/target#bf16-balanced"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v (%+v)", got, want, skipped)
	}
	if len(pkg.Catalog.Models) != 1 || pkg.Catalog.Models[0].ModelID != "org/fine" {
		t.Fatalf("kept = %+v", pkg.Catalog.Models)
	}
}

func TestLoadPackageTolerantStillRefusesADamagedPackage(t *testing.T) {
	newer := func(modelID, yaml string) string {
		if modelID == "org/a" {
			return afterSpec(t, yaml, "next", "field_of_a_newer_server: true")
		}
		return yaml
	}
	models := func() []InferenceModel { return []InferenceModel{tolerantModel("org/a"), tolerantModel("org/b")} }
	for _, tc := range []struct {
		name   string
		damage func(files fstest.MapFS)
		want   string
	}{
		{"manifest counts", func(files fstest.MapFS) {
			files["manifest.yaml"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("schema_version: %d\npublished_at: 2026-08-31T00:00:00Z\nmodels: 2\nspecs: 3\n", PackageSchemaVersion))}
		}, "manifest says"},
		{"newer schema", func(files fstest.MapFS) {
			files["manifest.yaml"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("schema_version: %d\npublished_at: 2026-08-31T00:00:00Z\nmodels: 2\nspecs: 4\n", PackageSchemaVersion+1))}
		}, "unsupported inference catalog package schema_version"},
		{"not a model document", func(files fstest.MapFS) {
			files["models/org/b/model.yaml"] = &fstest.MapFile{Data: []byte("- just\n- a list\n")}
		}, "parse inference catalog model models/org/b/model.yaml"},
		{"unexpected file", func(files fstest.MapFS) {
			files["models/org/b/dependency.whl"] = &fstest.MapFile{Data: []byte("wheel")}
		}, "unexpected model file"},
		{"unreferenced asset of a model read in full", func(files fstest.MapFS) {
			files["models/org/b/assets/stray.py"] = &fstest.MapFile{Data: []byte("x")}
		}, "unreferenced asset"},
		{"model stored under another identity", func(files fstest.MapFS) {
			files["models/org/c/model.yaml"] = files["models/org/b/model.yaml"]
			delete(files, "models/org/b/model.yaml")
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tolerantPackage(t, models(), newer)
			tc.damage(files)
			if _, _, err := LoadPackageTolerant(files); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("nothing readable", func(t *testing.T) {
		files := tolerantPackage(t, models(), func(_, yaml string) string { return "model_field_of_a_newer_server: 1\n" + yaml })
		if _, _, err := LoadPackageTolerant(files); err == nil || !strings.Contains(err.Error(), "no model this server can read") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an unknown manifest field is not damage", func(t *testing.T) {
		files := tolerantPackage(t, models(), nil)
		files["manifest.yaml"] = &fstest.MapFile{Data: append(files["manifest.yaml"].Data, []byte("field_of_a_newer_server: 1\n")...)}
		if _, skipped, err := LoadPackageTolerant(files); err != nil || len(skipped) != 0 {
			t.Fatalf("err=%v skipped=%+v", err, skipped)
		}
	})
}
