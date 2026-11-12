package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This is the importable package's contract, not a claim of GPU inference.
// Exercise the same catalog loader, image store and launch assembler used by
// the Web/API, including local device visibility versus global tensor degree.
func TestKunlunQwenPackageImportsAndResolvesLaunches(t *testing.T) {
	root := filepath.Join("..", "..", "..", "build", "inference-templates", "kunlun-p800")
	pkg, err := inferencecatalog.LoadPackageDirectory(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.Models != 2 || pkg.Manifest.Specs != 2 {
		t.Fatal(pkg.Manifest)
	}
	run := newOfficialCatalogRun(t)
	st := run.store
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	var before []store.Image
	if err := st.DB().Where("source = ?", "official").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	originalModels := map[string]*store.InferenceModel{}
	for _, model := range pkg.Catalog.Models {
		original, err := st.InferenceModelByModelID(model.ModelID)
		if err != nil {
			t.Fatal(err)
		}
		originalModels[model.ModelID] = original
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, os.DirFS(filepath.Join(root, "catalog"))); err != nil {
		t.Fatal(err)
	}
	rec := run.do(http.MethodPost, "/api/v1/inference-templates/import", archive.Bytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("package import: %d %s", rec.Code, rec.Body.String())
	}
	for _, original := range before {
		current, err := st.ImageByRef(original.ImageRef)
		if err != nil || current.ID != original.ID || current.Source != "official" || current.Deprecated != original.Deprecated || current.Enabled != original.Enabled {
			t.Fatalf("template import changed upstream image %s", original.ImageRef)
		}
	}
	for id, original := range originalModels {
		current, err := st.InferenceModelByModelID(id)
		if err != nil {
			t.Fatal(err)
		}
		if current.Source != original.Source || current.InputModalities != original.InputModalities || current.ModelConfig != original.ModelConfig || len(current.Specs) != len(original.Specs)+1 {
			t.Fatalf("optional package replaced upstream model facts: %s", id)
		}
		for _, old := range original.Specs {
			found := false
			for _, spec := range current.Specs {
				if spec.ID == old.ID {
					found = true
					if spec.Image != old.Image || spec.Args != old.Args || spec.Env != old.Env || spec.GPUs != old.GPUs || spec.Source != old.Source {
						t.Fatalf("existing profile changed: %s/%s", id, old.Name)
					}
				}
			}
			if !found {
				t.Fatalf("existing profile removed: %s/%s", id, old.Name)
			}
		}
	}
	for _, model := range pkg.Catalog.Models {
		t.Run(model.ModelID, func(t *testing.T) {
			spec := model.Specs[0]
			if model.Source != "user" || spec.Weights.Quantization != "bf16" || len(spec.Weights.Sources) != 1 {
				t.Fatalf("model identity changed: %+v", spec.Weights)
			}
			wantRepo, wantRevision := "Qwen/Qwen3-0.6B", "c1899de289a04d12100db370d81485cdf75e47ca"
			if model.ModelID == "qwen/qwen3.8-27b" {
				wantRepo, wantRevision = "Qwen/Qwen3.8-27B", "1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0"
			}
			if source := spec.Weights.Sources[0]; source.RepoID != wantRepo || source.Revision != wantRevision {
				t.Fatal(source)
			}
			if spec.Image != "registry.zstack.ai/gpuworks/gpuworks-runner@sha256:33d94768ff9a895bdc8874cae09940aede1b0e88218e52ed52231b8f86a91beb" {
				t.Fatalf("image %q must be the reviewed PP/DP r4 private-registry manifest digest", spec.Image)
			}
			// Template-owned digest refs do not need a mutable image-catalog row:
			// a deployment trusts the image its own template names.
			target := deploy.ImageTarget{Engine: spec.Engine, Vendor: "kunlunxin", PlatformArch: "amd64", Override: spec.Image,
				TrustedOverride: true}
			resolvedImage := deploy.ResolveImageCatalog(target, nil)
			if resolvedImage.Blocking || resolvedImage.Image != spec.Image {
				t.Fatalf("template digest unavailable: %+v", resolvedImage)
			}
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
			for _, card := range []struct {
				vendor, name string
				match        bool
			}{{"kunlunxin", "P800 PCIe", true}, {"kunlunxin", "P600", false}, {"nvidia", "P800 PCIe", false}, {"ascend", "910C", false}} {
				if got := chipmatch.Evaluate(chipmatch.GPU{Vendor: card.vendor, Name: card.name}, predicate).Matched; got != card.match {
					t.Fatalf("wrong hardware match: %+v", card)
				}
			}
			for _, shape := range []struct{ cards, nodes, tp int }{{1, 1, 1}, {2, 1, 2}, {1, 2, 2}, {4, 2, 8}} {
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: shape.cards, MinMemoryGiB: 96})
				if err != nil || result.Reason != "" {
					t.Fatalf("placement %+v: %+v %v", shape, result, err)
				}
				variables := placement.RuntimeVariables(result, shape.cards, shape.tp, "/model")
				engine, err := deploy.SelectEngine(spec.Engine)
				if err != nil {
					t.Fatal(err)
				}
				launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
					Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, PreStartScript: spec.PreStartScript, APIContract: spec.API, PlacementVariables: variables,
					Params: deploy.Params{Vendor: "kunlunxin", ModelPath: "/model", APIPort: 31417, AssignedGPUs: shape.cards, TensorParallel: shape.tp, PipelineParallel: 1, NNodes: shape.nodes, TemplateArgs: spec.Args, TemplateEnv: spec.Env},
				})
				if err != nil {
					t.Fatalf("launch %+v: %v", shape, err)
				}
				if launch.RequestedFeatures == nil || !launch.RequestedFeatures.CommandKnown || launch.RequestedFeatures.MultiNode != (shape.nodes > 1) {
					t.Fatalf("direct entrypoint features = %+v", launch.RequestedFeatures)
				}
				for flag, want := range map[string]string{"--dtype": "float16", "--tensor-parallel-size": strconv.Itoa(shape.tp), "--max-model-len": "2048"} {
					if got, ok := deploy.LaunchFlag(launch.Argv, flag); !ok || got != want {
						t.Fatalf("%s=%q, want %q", flag, got, want)
					}
				}
				entry := strings.Join(launch.Entrypoint, " ")
				visible := make([]string, shape.cards)
				for i := range visible {
					visible[i] = strconv.Itoa(i)
				}
				if entry != "/opt/vllm025-venv/bin/vllm serve" {
					t.Fatalf("wrong native entrypoint: %q", entry)
				}
				if got := launch.Env["CUDA_VISIBLE_DEVICES"]; got != strings.Join(visible, ",") {
					t.Fatalf("CUDA_VISIBLE_DEVICES = %q, want %q", got, strings.Join(visible, ","))
				}
				if _, found := deploy.LaunchFlag(launch.Argv, "--enable-sleep-mode"); found {
					t.Fatal("sleep leaked into native template")
				}
			}
			for _, shape := range []placement.GPU{{Count: 3, MinMemoryGiB: 96}, {Count: 1, MinMemoryGiB: 32}} {
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, shape)
				if err != nil || result.Reason == "" {
					t.Fatalf("invalid shape accepted: %+v %v", shape, err)
				}
			}
		})
	}
}
