package seed

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

func TestEmbeddedFlatCatalogSeedsIdempotentlyAndPreservesUserModels(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) < 260 {
		t.Fatalf("embedded catalog models=%d, want at least 260", len(catalog.Models))
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "seed.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	user := store.InferenceModel{ModelID: "me/custom", Source: "user", Specs: []store.InferenceSpecRow{{Name: "default", Engine: "vllm", Image: "custom:image", Quantization: "bf16", WeightSources: `[{"provider":"huggingface","repo_id":"me/custom"}]`, GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`}}}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{user}, false); err != nil {
		t.Fatal(err)
	}
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatal(err)
	}
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if _, err := st.InferenceModelByModelID("me/custom"); err != nil {
		t.Fatalf("user model removed by seed: %v", err)
	}
	model, err := st.InferenceModelByModelID("deepseek-ai/deepseek-v4-pro")
	if err != nil || len(model.Specs) == 0 {
		t.Fatalf("seeded model=%+v err=%v", model, err)
	}
}

func TestEmbeddedCatalogProvidesSmallFP16RuntimeSpecsForTuring(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	matchedModels := 0
	foundSmallChat := false
	for _, model := range catalog.Models {
		modelMatched := false
		for _, spec := range model.Specs {
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
			if !chipmatch.Evaluate(chipmatch.GPU{Vendor: "nvidia", Name: "NVIDIA GeForce RTX 2080 Ti", CC: 7.5}, predicate).Matched {
				continue
			}
			// The conservative contract below is what a vLLM workload on a
			// small Turing card must look like. Other engines own their own
			// safety (a callable spec carries its placement resolver and API
			// contract), so they are out of this test's scope and do not count
			// toward the models it requires.
			if spec.Engine != "vllm" {
				continue
			}
			modelMatched = true
			wantImage, wantDtype := "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2", "half"
			if slices.Contains(spec.Args, "--omni") {
				// There is no CUDA 12 vllm-omni runner. The CUDA 13 one runs on
				// Turing, but vllm-omni's multi-token prediction path mixes
				// fp16 and fp32 tensors and fails in fp16, so omni specs run fp32.
				wantImage, wantDtype = "gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.30.0-r4", "float32"
			}
			if spec.Weights.Quantization != "bf16" ||
				!containsArgumentPair(spec.Args, "--dtype", wantDtype) ||
				spec.Image != wantImage ||
				slices.Contains(spec.Args, "--tensor-parallel-size") {
				t.Fatalf("Turing spec %s/%s has unsafe runtime contract: %+v", model.ModelID, spec.Name, spec)
			}
			if model.ModelID == "liquidai/lfm2.5-230m" {
				foundSmallChat = true
			}
		}
		if modelMatched {
			matchedModels++
		}
	}
	if matchedModels < 20 || !foundSmallChat {
		t.Fatalf("RTX 2080 Ti matched %d models, small chat model present=%v", matchedModels, foundSmallChat)
	}
}

func TestEmbeddedCatalogSeparatesMistralRuntimeAndRepositoryWeights(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		storage int64
		runtime int64
	}{
		"mistralai/ministral-3-8b-reasoning-2512": {storage: 35_706_509_936, runtime: 17_836_115_976},
		"mistralai/ministral-3-14b-instruct-2512": {storage: 31_487_447_241, runtime: 15_726_706_472},
	}
	for _, model := range catalog.Models {
		expected, ok := want[model.ModelID]
		if !ok {
			continue
		}
		found := false
		for _, spec := range model.Specs {
			if spec.WeightBytes == expected.storage && spec.RuntimeWeightBytes == expected.runtime {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Mistral layout contract for %s = %+v", model.ModelID, model.Specs)
		}
		delete(want, model.ModelID)
	}
	if len(want) != 0 {
		t.Fatalf("missing Mistral layout contracts: %v", want)
	}
}

// Every Hygon spec inherits K100 PASS to the whole BW family: hy-smi on some
// drivers reports only the series "BW", others a numbered SKU (ADR 0009).
func TestEmbeddedCatalogMatchesReportedHygonNames(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	hygonModels := 0
	for _, model := range catalog.Models {
		var specs []chipmatch.Spec
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "hygon" {
				continue
			}
			specs = append(specs, chipmatch.Spec{
				Name: spec.Name,
				GPUs: chipmatch.Predicate{
					Vendor: spec.GPUs.Vendor,
					CC:     spec.GPUs.CC,
					Name:   spec.GPUs.Name,
				},
				Quantization: spec.Weights.Quantization,
			})
		}
		if len(specs) == 0 {
			continue
		}
		hygonModels++
		for _, name := range []string{"K100", "K100AI", "K100_AI", "BW", "BW100", "BW150", "BW1000", "BW1100", "BW3000"} {
			selection := chipmatch.Select(chipmatch.GPU{Vendor: "hygon", Name: name}, specs, nil)
			if !selection.Matched || selection.SpecIndex < 0 {
				t.Errorf("%s: Hygon %s matched no spec: %+v", model.ModelID, name, selection)
			}
		}
		for _, name := range []string{"K500", "BWX"} {
			if chipmatch.Select(chipmatch.GPU{Vendor: "hygon", Name: name}, specs, nil).Matched {
				t.Errorf("%s: unqualified Hygon model %s matched", model.ModelID, name)
			}
		}
	}
	if hygonModels == 0 {
		t.Fatal("embedded catalog has no Hygon specs")
	}
}

func TestEmbeddedCatalogPackageSummaryAndDeterministicArchive(t *testing.T) {
	// LoadPackage itself validates manifest.yaml against the walked content
	// ("manifest says N, package contains M"), so a drifted summary fails
	// right here with that message; no hardcoded count needed.
	if _, err := embeddedPackage(); err != nil {
		t.Fatal(err)
	}
	root, err := fs.Sub(embeddedCatalog, "catalog")
	if err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := inferencecatalog.PackPackage(&first, root); err != nil {
		t.Fatal(err)
	}
	if err := inferencecatalog.PackPackage(&second, root); err != nil {
		t.Fatal(err)
	}
	firstSHA := sha256.Sum256(first.Bytes())
	secondSHA := sha256.Sum256(second.Bytes())
	if firstSHA != secondSHA {
		t.Fatalf("embedded package archive is not deterministic: %x != %x", firstSHA, secondSHA)
	}
	if int64(first.Len()) > catalogasset.MaxPackageBytes {
		t.Fatalf("embedded package archive is %d bytes, limit is %d", first.Len(), catalogasset.MaxPackageBytes)
	}
	t.Logf("embedded package archive: %d bytes, sha256=%x", first.Len(), firstSHA)
}

func TestMirrorWeightOrganizationsDoNotCreateDuplicateOriginalModels(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	type expectedMirror struct {
		originalID, forbiddenID, provider, repoID string
	}
	expected := []expectedMirror{
		{"baidu/ernie-4.5-21b-a3b-pt", "paddlepaddle/ernie-4.5-21b-a3b-pt", "modelscope", "PaddlePaddle/ERNIE-4.5-21B-A3B-PT"},
		{"zai-org/glm-4.5v", "zhipuai/glm-4.5v", "modelscope", "ZhipuAI/GLM-4.5V"},
		{"zai-org/glm-4.6v-flash", "zhipuai/glm-4.6v-flash", "modelscope", "ZhipuAI/GLM-4.6V-Flash"},
		{"meta-llama/llama-3.2-1b-instruct", "llm-research/llama-3.2-1b-instruct", "modelscope", "LLM-Research/Llama-3.2-1B-Instruct"},
	}
	byID := make(map[string]inferencecatalog.InferenceModel, len(catalog.Models))
	for _, model := range catalog.Models {
		byID[model.ModelID] = model
	}
	for _, item := range expected {
		if _, exists := byID[item.forbiddenID]; exists {
			t.Errorf("mirror weight organization resurfaced as original model %s", item.forbiddenID)
		}
		model, exists := byID[item.originalID]
		if !exists {
			t.Errorf("missing canonical original model %s", item.originalID)
			continue
		}
		found := false
		for _, spec := range model.Specs {
			for _, source := range spec.Weights.Sources {
				if source.Provider == item.provider && source.RepoID == item.repoID {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("original model %s lost mirror weight %s/%s", item.originalID, item.provider, item.repoID)
		}
	}
}

func containsArgumentPair(args []string, flag, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == value {
			return true
		}
	}
	return false
}

func reconcileEmbeddedCatalog(st *store.Store) error {
	catalog, digest, err := CatalogWithDigest()
	if err != nil {
		return err
	}
	return inferencecatalog.ReconcileUpstreamCatalog(st, catalog, digest)
}

// Every built-in spec carries both display names: the template page shows
// them, and a built-in spec without one would fall back to its machine id.
// Validation already keeps them distinct within a model and engine.
func TestEmbeddedCatalogNamesEverySpecInBothLanguages(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.DisplayNameZHCN == "" || spec.DisplayNameEN == "" {
				t.Errorf("%s spec %s lacks a display name (zh %q, en %q)", model.ModelID, spec.Name, spec.DisplayNameZHCN, spec.DisplayNameEN)
			}
			if spec.RetiredDisplayProfile != "" || spec.GPUs.RetiredDisplayTarget != "" {
				t.Errorf("%s spec %s still sets the retired display_profile / display_target", model.ModelID, spec.Name)
			}
		}
	}
}

// The catalog every caller gets is decoded from the copy loaded once per
// process. It must be exactly what loading the embedded package yields, and
// no caller's edits may reach the next caller.
func TestEmbeddedCatalogCopyIsExact(t *testing.T) {
	loaded, err := embeddedPackage()
	if err != nil {
		t.Fatal(err)
	}
	first, digest, err := CatalogWithDigest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, loaded.Catalog) {
		t.Fatal("the cached catalog differs from the embedded package's: a catalog field does not survive JSON")
	}
	root, err := embeddedPackageFS()
	if err != nil {
		t.Fatal(err)
	}
	if want, err := inferencecatalog.PackageDigest(root); err != nil || digest != want {
		t.Fatalf("digest = %q, want %q (%v)", digest, want, err)
	}
	if len(first.Models) == 0 || len(first.Models[0].Specs) == 0 {
		t.Fatal("the embedded catalog has no model with a spec")
	}
	first.Models[0].ModelID = "edited"
	first.Models[0].Specs[0].Engine = "edited"
	first.Models = append(first.Models[:1], first.Models[2:]...)
	second, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, loaded.Catalog) {
		t.Fatal("an edit to one caller's catalog reached the next caller")
	}

	archive, err := Archive()
	if err != nil {
		t.Fatal(err)
	}
	archive[0] ^= 0xff
	published, publishedDigest, _, err := Published()
	if err != nil {
		t.Fatal(err)
	}
	if published[0] == archive[0] || publishedDigest != digest {
		t.Fatal("an edit to one caller's archive reached the next caller")
	}
	unpacked, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(published))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := inferencecatalog.PackageDigest(unpacked.FS); err != nil || got != digest {
		t.Fatalf("archive digest = %q, want %q (%v)", got, digest, err)
	}
}
