package deployment

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A built-in and a user spec of the same name each launch with the assets of
// their own package: the selection of the user spec carries the user
// package's asset, by that package's digest.
func TestSameNamedSpecsSelectTheAssetsOfTheirOwnPackage(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "spec-assets.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	pkgs, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	const modelID = "acme/shared-assets"
	put := func(source, image string, asset []byte) store.InferenceModel {
		t.Helper()
		model := inferencecatalog.InferenceModel{ModelID: modelID, Source: source, DisplayTitle: "Canonical title", Specs: []inferencecatalog.InferenceSpec{{
			Name: "general", Engine: "vllm", Image: image,
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
			Assets:  []inferencecatalog.Asset{{File: "assets/launch.py", SHA256: fmt.Sprintf("%x", sha256.Sum256(asset)), UnpackTo: "/opt/gpuworks/launch.py"}},
		}}}
		modelYAML, err := inferencecatalog.MarshalModelYAML(model)
		if err != nil {
			t.Fatal(err)
		}
		digest, _, err := pkgs.Put(fstest.MapFS{
			"manifest.yaml":                              {Data: []byte("schema_version: 1\npublished_at: 2026-09-01T00:00:00Z\nmodels: 1\nspecs: 1\n")},
			"models/acme/shared-assets/model.yaml":       {Data: modelYAML},
			"models/acme/shared-assets/assets/launch.py": {Data: asset},
		})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}})
		if err != nil {
			t.Fatal(err)
		}
		rows[0].PackageDigest = digest
		for i := range rows[0].Specs {
			rows[0].Specs[i].PackageDigest = digest
		}
		return rows[0]
	}
	upstream := put("upstream", "upstream/vllm:v1", []byte("print('built-in')\n"))
	user := put("user", "user/vllm:v1", []byte("print('user')\n"))
	if upstream.PackageDigest == user.PackageDigest {
		t.Fatal("the two packages share a digest; the test could not tell them apart")
	}
	upstream.Specs = append(upstream.Specs, user.Specs...)
	if err := database.ReconcileInferenceCatalog([]store.InferenceModel{upstream}); err != nil {
		t.Fatal(err)
	}
	stored, err := database.InferenceModelByModelID(modelID)
	if err != nil || len(stored.Specs) != 2 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	s := New(Deps{Store: database, CatalogPackages: pkgs})
	for _, source := range []string{"upstream", "user"} {
		want := upstream.PackageDigest
		if source == "user" {
			want = user.PackageDigest
		}
		req := dto.DeployRequest{TemplateID: stored.ID, TemplateSpecSource: source, TemplateSpecName: "general"}
		selection, err := s.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
		if err != nil {
			t.Fatal(err)
		}
		if selection.Spec.Source != source || selection.Spec.Image != source+"/vllm:v1" || len(selection.Assets) != 1 || selection.Assets[0].PackageDigest != want {
			t.Fatalf("%s selection = spec %s image %s assets %+v, want package %s", source, selection.Spec.Source, selection.Spec.Image, selection.Assets, want)
		}
	}
}

// A digest-pinned image the template itself names is the template's to
// trust; the same image a request states over another template's, or with no
// template, is not.
func TestTemplateOwnsOnlyTheImageItNames(t *testing.T) {
	const pinned = "registry.example/vendor/runtime@sha256:33d94768ff9a895bdc8874cae09940aede1b0e88218e52ed52231b8f86a91beb"
	if !templateOwnsImage(templateSelection{Spec: &store.InferenceSpecRow{Image: pinned}}, pinned) {
		t.Fatal("the template's own digest image is not trusted")
	}
	if templateOwnsImage(templateSelection{Spec: &store.InferenceSpecRow{Image: "registry.example/vendor/runtime:v1"}}, pinned) {
		t.Fatal("an image the template does not name is trusted")
	}
	if templateOwnsImage(templateSelection{}, pinned) {
		t.Fatal("an image without a template is trusted")
	}
}
