package seed

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

func TestSyntheticUserCatalogEntrypointAndGlobalTP(t *testing.T) {
	root := t.TempDir()
	modelDir := filepath.Join(root, "models", "example", "tiny")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: 1\npublished_at: 2026-10-14T00:00:00Z\nmodels: 1\nspecs: 1\n"
	model := `model_id: example/tiny
source: user
input_modalities: [text]
specs:
- name: synthetic-xre-fixture
  engine: sglang
  image: registry.example/synthetic:tested
  weights:
    quantization: bf16
    sources:
    - huggingface:
        repo_id: example/tiny
  gpus:
    vendor: kunlunxin
    name: P800 PCIe
  entrypoint: [env, -u, XPU_VISIBLE_DEVICES, /opt/sglang/bin/sglang, serve]
  placement:
    language: starlark
    resolver: |
      def resolve(gpu):
          if gpu.count < 1: return reject("requires a GPU")
          return {"GPU_COUNT": gpu.count}
`
	for path, body := range map[string]string{filepath.Join(root, "manifest.yaml"): manifest, filepath.Join(modelDir, "model.yaml"): model} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg, err := inferencecatalog.LoadPackageDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.Models != 1 || pkg.Manifest.Specs != 1 || len(pkg.Catalog.Models) != 1 || len(pkg.Catalog.Models[0].Specs) != 1 {
		t.Fatalf("package shape: %+v", pkg.Manifest)
	}
	spec := pkg.Catalog.Models[0].Specs[0]
	if pkg.Catalog.Models[0].Source != "user" || spec.Engine != "sglang" || spec.GPUs.Vendor != "kunlunxin" || spec.Image != "registry.example/synthetic:tested" {
		t.Fatalf("user spec: %+v", spec)
	}
	wantEntry := []string{"env", "-u", "XPU_VISIBLE_DEVICES", "/opt/sglang/bin/sglang", "serve"}
	if !reflect.DeepEqual(spec.Entrypoint, wantEntry) {
		t.Fatalf("entrypoint = %q", spec.Entrypoint)
	}
	for _, tc := range []struct{ cards, tp int }{{1, 1}, {2, 2}, {2, 4}} {
		resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: tc.cards})
		if err != nil || resolved.Reason != "" {
			t.Fatalf("%+v: resolver=%+v, err=%v", tc, resolved, err)
		}
		vars := placement.RuntimeVariables(resolved, tc.cards, tc.tp, "/model")
		if vars["TP_SIZE"] != int64(tc.tp) || vars["GPU_COUNT"] != int64(tc.cards) {
			t.Fatalf("%+v: variables=%+v", tc, vars)
		}
	}
}

func TestImportableP800PackageMatchesImageLock(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "build", "inference-templates", "kunlun-p800", "catalog")
	// The reviewed release is owned by the package lock. A second literal in
	// this test goes stale when the single-node image gains its RDMA layer.
	lockBytes, err := os.ReadFile(filepath.Join(root, "..", "image-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		ImageRef       string `json:"image_ref"`
		ManifestDigest string `json:"manifest_digest"`
		Interpreter    string `json:"interpreter"`
	}
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatal(err)
	}
	algorithm, value, _ := strings.Cut(lock.ManifestDigest, ":")
	digestBytes, err := hex.DecodeString(value)
	if err != nil || algorithm != "sha256" || len(digestBytes) != 32 {
		t.Fatalf("invalid locked manifest digest %q", lock.ManifestDigest)
	}
	repository, _, _ := strings.Cut(lock.ImageRef, "@")
	if tag := strings.LastIndex(repository, ":"); tag > strings.LastIndex(repository, "/") {
		repository = repository[:tag]
	}
	if repository == "" || !strings.HasSuffix(lock.Interpreter, "/python") {
		t.Fatalf("incomplete package lock: %+v", lock)
	}
	digest := repository + "@" + lock.ManifestDigest
	entrypoint := []string{strings.TrimSuffix(lock.Interpreter, "python") + "vllm", "serve"}
	var packed bytes.Buffer
	if err := inferencecatalog.PackPackage(&packed, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	archive, err := inferencecatalog.ReadPackageArchive(&packed)
	if err != nil {
		t.Fatal(err)
	}
	pkg := archive.Package
	if len(pkg.Catalog.Models) != 2 {
		t.Fatalf("models = %d", len(pkg.Catalog.Models))
	}
	for _, model := range pkg.Catalog.Models {
		for _, spec := range model.Specs {
			matched := chipmatch.Evaluate(chipmatch.ReportedGPU(accelerator.NormalizeGPUName("Kunlunxin P800 PCIe"), "kunlunxin", "KL3", ""), chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name})
			if !matched.Matched {
				t.Fatalf("%s does not match normalized P800 name: %+v", model.ModelID, matched)
			}
			if spec.Image != digest {
				t.Fatalf("%s image = %s, want locked image %s", model.ModelID, spec.Image, digest)
			}
			if !reflect.DeepEqual(spec.Entrypoint, entrypoint) {
				t.Fatalf("%s entrypoint = %v, want direct vllm entrypoint %v", model.ModelID, spec.Entrypoint, entrypoint)
			}
			if strings.TrimSpace(spec.Placement.Resolver) == "" {
				t.Fatalf("%s has no resolver", model.ModelID)
			}
			resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: 1, MinMemoryGiB: 96})
			if err != nil || resolved.Reason != "" {
				t.Fatalf("%s: resolver=%+v err=%v", model.ModelID, resolved, err)
			}
		}
	}
}

// Fresh installations must offer a real P800 profile without importing a user
// package. Keep repository identity and precision distinct from display names.
func TestBuiltinKunlunQwenProfiles(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"qwen/qwen3-0.6b": "Qwen/Qwen3-0.6B", "qwen/qwen3.8-27b": "Qwen/Qwen3.8-27B"}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != accelerator.VendorKunlunxin {
				continue
			}
			repo, ok := want[model.ModelID]
			if !ok || seen[model.ModelID] {
				t.Fatalf("unexpected or duplicate built-in Kunlun model %s", model.ModelID)
			}
			seen[model.ModelID] = true
			if spec.Image != "gpuworks/gpuworks-runner:xre5.0.21.46-vllm0.25.1-r1" || spec.Weights.Quantization != "bf16" || len(spec.Weights.Sources) == 0 || spec.Weights.Sources[0].RepoID != repo {
				t.Fatalf("%s: inconsistent image/weight identity", model.ModelID)
			}
			if !reflect.DeepEqual(spec.Entrypoint, []string{"/opt/vllm025-venv/bin/vllm", "serve"}) || !strings.Contains(strings.Join(spec.Args, " "), "--dtype float16") {
				t.Fatalf("%s: wrong interpreter or compute precision", model.ModelID)
			}
			// The agent reports the normalized name, while Add Chip uses the
			// branded hardware-catalog label. Both identify the same PCIe card.
			for _, card := range []struct {
				name string
				want bool
			}{{"P800 PCIe", true}, {"Kunlunxin P800 PCIe", true}, {"P800 OAM", false}, {"Kunlunxin P800 OAM", false}, {"P800 PCIe-compatible", false}} {
				match := chipmatch.Evaluate(chipmatch.GPU{Vendor: accelerator.VendorKunlunxin, Name: card.name}, chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name})
				if match.Matched != card.want {
					t.Fatalf("%s match %q = %v, want %v", model.ModelID, card.name, match.Matched, card.want)
				}
			}
			resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: 1, MinMemoryGiB: 96})
			if err != nil || resolved.Reason != "" {
				t.Fatalf("%s: %v %+v", model.ModelID, err, resolved)
			}
			for _, count := range []int{0, 2, 4, 8} {
				rejected, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: count, MinMemoryGiB: 96})
				if err != nil || rejected.Reason == "" {
					t.Fatalf("%s accepted unverified %d-GPU topology: %+v %v", model.ModelID, count, rejected, err)
				}
			}
			providers := map[string]bool{}
			for _, source := range spec.Weights.Sources {
				if source.RepoID != repo || source.Revision == "" {
					t.Fatal("weight source must retain pinned original identity")
				}
				providers[source.Provider] = true
			}
			if !providers["huggingface"] || !providers["modelscope"] {
				t.Fatalf("%s missing provider: %v", model.ModelID, providers)
			}

		}
	}
	if len(seen) != len(want) {
		t.Fatalf("built-in Kunlun profiles = %v", seen)
	}
}
