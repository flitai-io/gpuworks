package store

import (
	"path/filepath"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A catalog read only in part must not cost the rows it could not read.
func TestReconcileInferenceCatalogKeepingLeavesNamedUpstreamRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-reconcile-keep.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const oldDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	spec := func(name, image, digest string) InferenceSpecRow {
		return InferenceSpecRow{Name: name, Engine: "vllm", Image: image, Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`, Source: "upstream", PackageDigest: digest}
	}
	model := func(id, digest string, specs ...InferenceSpecRow) InferenceModel {
		return InferenceModel{ModelID: id, Source: "upstream", PackageDigest: digest, Specs: specs}
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{
		model("org/partial", oldDigest, spec("plain", "img:old", oldDigest), spec("newer", "img:old", oldDigest), spec("dropped", "img:old", oldDigest)),
		model("org/whole", oldDigest, spec("a", "img:old", oldDigest), spec("b", "img:old", oldDigest)),
		model("org/gone", oldDigest, spec("a", "img:old", oldDigest)),
	}); err != nil {
		t.Fatal(err)
	}
	// The next catalog: "newer" of org/partial and all of org/whole are stated
	// in a form this server skipped; "dropped" and org/gone are really gone;
	// org/fresh#unreadable never existed here.
	if err := st.ReconcileInferenceCatalogKeeping([]InferenceModel{
		model("org/partial", newDigest, spec("plain", "img:new", newDigest)),
		model("org/fresh", newDigest, spec("plain", "img:new", newDigest)),
	}, []InferenceCatalogKeep{
		{ModelID: "Org/Partial", Spec: "newer"},
		{ModelID: "org/whole"},
		{ModelID: "org/fresh", Spec: "unreadable"},
		{ModelID: "org/never-seen"},
	}); err != nil {
		t.Fatal(err)
	}
	specs := func(id string) map[string]string {
		t.Helper()
		got, err := st.InferenceModelByModelID(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		out := map[string]string{}
		for _, row := range got.Specs {
			out[row.Name] = row.Image + "@" + row.PackageDigest[:1]
		}
		return out
	}
	equal := func(id string, want map[string]string) {
		t.Helper()
		got := specs(id)
		if len(got) != len(want) {
			t.Fatalf("%s specs = %v, want %v", id, got, want)
		}
		for name, value := range want {
			if got[name] != value {
				t.Fatalf("%s specs = %v, want %v", id, got, want)
			}
		}
	}
	equal("org/partial", map[string]string{"plain": "img:new@b", "newer": "img:old@a"})
	equal("org/whole", map[string]string{"a": "img:old@a", "b": "img:old@a"})
	equal("org/fresh", map[string]string{"plain": "img:new@b"})
	for _, id := range []string{"org/gone", "org/never-seen"} {
		if _, err := st.InferenceModelByModelID(id); err != ErrNotFound {
			t.Fatalf("%s error = %v", id, err)
		}
	}
	if digest, err := st.UpstreamInferencePackageDigest(); err != nil || digest != newDigest {
		t.Fatalf("catalog in force = %q, %v", digest, err)
	}
	// The kept rows still reference the package their assets live in.
	digests, err := st.ReferencedCatalogPackageDigests()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(digests)
	if len(digests) != 2 || digests[0] != oldDigest || digests[1] != newDigest {
		t.Fatalf("referenced digests = %v", digests)
	}
}
