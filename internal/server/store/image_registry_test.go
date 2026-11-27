package store

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestImageCopiesOrderTemplateCatalogThenTheImage(t *testing.T) {
	st, err := Open("file:store_image_copies?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&Image{
		ImageRef: "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", Source: "official", Engine: "sglang", Enabled: true,
		RegistrySources: `["quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2","https://dl.example/runner.tar"]`,
	}); err != nil {
		t.Fatal(err)
	}
	got := st.ImageCopies("gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", []string{"ghcr.io/team/runner:r2"})
	want := []string{"ghcr.io/team/runner:r2", "quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copies = %#v, want %#v", got, want)
	}
	// A deployment that names one of the row's copies keeps running that
	// copy, and the row's own reference is among the others.
	got = st.ImageCopies("quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", nil)
	want = []string{"quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copies of a named copy = %#v, want %#v", got, want)
	}
	// An image the catalog does not know is its own only copy.
	if got := st.ImageCopies("vllm/vllm-openai:v1", nil); !reflect.DeepEqual(got, []string{"vllm/vllm-openai:v1"}) {
		t.Fatalf("uncatalogued copies = %#v", got)
	}
}

// A declared registry source resolves to its catalog row; a reference that
// merely matches as a LIKE pattern would (an "_" standing for any character)
// does not.
func TestResolveImageRefMatchesDeclaredSourcesExactly(t *testing.T) {
	st, err := Open("file:store_resolve_ref_exact?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.UpsertImage(&Image{ImageRef: "team/app:v1", Source: "custom", Engine: "vllm", Enabled: true,
		RegistrySources: `["mirror.example/team/app_x:v1"]`})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveImageRef("mirror.example/team/app_x:v1"); err != nil || got.ID != row.ID {
		t.Fatalf("declared source = %+v, %v", got, err)
	}
	for _, other := range []string{"docker.io/mirror.example/team/app_x:v1", "mirror.example/team/appyx:v1", "mirror.example/team/app%:v1", "mirror.example/team/app_x", "", "team/app"} {
		if got, err := st.ResolveImageRef(other); err != ErrNotFound {
			t.Fatalf("ResolveImageRef(%q) = %+v, %v; want not found", other, got, err)
		}
	}
}
