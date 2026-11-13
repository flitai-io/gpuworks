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
