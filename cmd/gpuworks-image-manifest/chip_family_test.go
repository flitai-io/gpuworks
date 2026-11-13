package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// The server picks a runner image for a node's chip family from the model
// segment of the reference (accelerator.RunnerImageChipFamily). That reader
// shares this file's tag grammar, so every matrix id must read back to the
// family its model segment names: a new model in tagPattern that the reader
// does not know would silently stop excluding it on the wrong chip.
func TestMatrixTagsReadBackToTheirChipFamily(t *testing.T) {
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	ascendFamily := map[string]string{"910b": "910b", "a3": "910c", "310p": "310p", "950": "950"}
	withModel := 0
	for _, item := range matrix.Items {
		version, tag, err := versionFromItem(item, matrix.Defaults.Platforms, DefaultRegistry)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if tag.Backend == "cann" {
			want = ascendFamily[tag.Model]
			if tag.Model != "" && want == "" {
				t.Fatalf("matrix id %s: model %q has no chip family; teach accelerator.RunnerImageChipFamily", item.ID, tag.Model)
			}
		}
		if want != "" {
			withModel++
		}
		if got := accelerator.RunnerImageChipFamily(version.ImageRef); got != want {
			t.Errorf("%s: chip family of %s = %q, want %q", item.ID, version.ImageRef, got, want)
		}
	}
	if withModel == 0 {
		t.Fatal("no CANN matrix id names a model; the check read nothing")
	}
}
