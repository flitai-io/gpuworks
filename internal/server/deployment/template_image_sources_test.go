package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// jsonEqual compares two JSON documents by value, so a test can state the wire
// shape literally without depending on key order.
func jsonEqual(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected document is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("wire JSON = %s\nwant      %s", encoded, want)
	}
}

// The pull forecast of an image that is also an image-catalog row: the
// template's sources, then the row's registry sources, then the canonical ref,
// without repeats; an archive URL is not a registry reference to pull.
func TestForecastPullRefsOrderTemplateThenCatalogThenCanonical(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("catalog-order-node", store.HashToken("token"), "catalog-order-node")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := json.Marshal([]string{"registry.example/gpustack/runner:v1", "https://files.example/runner.tar", "mirror.example/gpustack/runner:v1"})
	if _, err := st.UpsertImage(&store.Image{ImageRef: "gpustack/runner:v1", Name: "runner", Engine: "vllm", Enabled: true, RegistrySources: string(registry)}); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	templateSources := []string{"team.example/gpustack/runner:v1", " mirror.example/gpustack/runner:v1 ", "team.example/gpustack/runner:v1", ""}

	facts := h.evaluateNodeAcquisition(context.Background(), []string{node.UUID}, "gpustack/runner:v1", templateSources, nil, store.SourceRules{}, nil)
	if len(facts) != 1 || facts[0].ImagePlan != "pull" {
		t.Fatalf("forecast = %+v", facts)
	}
	want := []string{"team.example/gpustack/runner:v1", "mirror.example/gpustack/runner:v1", "registry.example/gpustack/runner:v1", "gpustack/runner:v1"}
	if !reflect.DeepEqual(facts[0].ImagePullRefs, want) {
		t.Fatalf("pull refs = %v, want %v", facts[0].ImagePullRefs, want)
	}
}

// The refs the evaluation forecasts and the chain a deployment falls back to
// are one ordering: for an image the catalog does not know, both are the
// template's sources, then the image, with a reference repeated in another
// spelling (Docker Hub default, host case) tried once, where it first
// appeared.
func TestForecastPullRefsAndFallbackChainShareOneOrdering(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("ordering-node", store.HashToken("token"), "ordering-node")
	if err != nil {
		t.Fatal(err)
	}
	const image = "org/runner:v1"
	sources := []string{"Team.Example/org/runner:v1", "team.example/org/runner:v1", "docker.io/org/runner:v1", "ghcr.io/org/runner:V1"}
	h := New(Deps{Store: st})
	facts := h.evaluateNodeAcquisition(context.Background(), []string{node.UUID}, image, sources, nil, store.SourceRules{}, nil)
	plan, err := planning.ImagePullPlanFor(st, nil, planning.LaunchSourceFromDeployment(&store.Deployment{ID: 1, Image: image}), node,
		deploy.CatalogSelection{Image: image, ImageSources: sources})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Team.Example/org/runner:v1", "docker.io/org/runner:v1", "ghcr.io/org/runner:V1"}
	if len(facts) != 1 || !reflect.DeepEqual(facts[0].ImagePullRefs, want) || !reflect.DeepEqual(plan.Refs(), want) {
		t.Fatalf("forecast = %+v, fallback chain = %v, want both %v", facts, plan.Refs(), want)
	}
}

// The ownership rule: only the template's own image gets its sources.
func TestTemplateImageSourcesBelongToTheTemplateImageOnly(t *testing.T) {
	spec := store.InferenceSpecRow{Image: imageSourcesUpstream}
	selection := templateSelection{Spec: &spec, ImageSources: []string{imageSourcesSWR}}
	if got := templateImageSources(selection, imageSourcesUpstream); !reflect.DeepEqual(got, []string{imageSourcesSWR}) {
		t.Fatalf("sources of the template image = %v", got)
	}
	if got := templateImageSources(selection, strings.ToUpper(imageSourcesUpstream[:4])+imageSourcesUpstream[4:]); len(got) != 1 {
		t.Fatalf("the image is compared case-insensitively like the trusted override: %v", got)
	}
	for _, image := range []string{"", "custom/vllm:cuda", "ghcr.io/org/runner@sha256:bbbb"} {
		if got := templateImageSources(selection, image); got != nil {
			t.Errorf("sources of %q = %v, want none", image, got)
		}
	}
	if got := templateImageSources(templateSelection{ImageSources: []string{"x"}}, "x"); got != nil {
		t.Fatalf("no template, no sources: %v", got)
	}
}

const (
	imageSourcesDigest   = "sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	imageSourcesUpstream = "ghcr.io/org/runner@" + imageSourcesDigest
	imageSourcesSWR      = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@" + imageSourcesDigest
	imageSourcesNJU      = "ghcr.nju.edu.cn/org/runner:v0.6.0"
)

// imageSourcesFixture is a template whose digest-pinned image names two further
// sources: a byte-identical copy and a mirror that is only a tag reference.
func imageSourcesFixture() store.InferenceModel {
	return store.InferenceModel{
		ModelID: "acme/sources", DisplayTitle: "Sources", Source: "upstream", Architecture: "qwen3", ContextLength: 8192,
		Specs: []store.InferenceSpecRow{{
			Name: "fp8-balanced", Scenario: "balanced", Engine: "vllm", Image: imageSourcesUpstream,
			ImageSources: fmt.Sprintf(`[%q,%q]`, imageSourcesSWR, imageSourcesNJU),
			Quantization: "fp8", WeightBytes: 4 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Sources-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=8.9"}`, Args: `["--max-model-len","4096"]`,
		}},
	}
}

type imageSourcesRig struct {
	st       *store.Store
	h        *Service
	template *store.InferenceModel
	model    *store.Model
	nodeUUID string
}

func newImageSourcesRig(t *testing.T, name string) imageSourcesRig {
	t.Helper()
	st, err := store.Open("file:api_image_sources_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := imageSourcesFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Acme/Sources-FP8", Revision: "main", Name: "Acme/Sources-FP8", WeightBytes: 4 << 30, QuantLabel: "fp8",
		Meta: `{"model_type":"llama","max_position_embeddings":8192,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "image-sources-node")
	return imageSourcesRig{st: st, h: New(Deps{Store: st}), template: template, model: model, nodeUUID: "image-sources-node"}
}

// The selection the template defaults produce carries the spec's sources and
// leaves an unrelated request alone: the template identity stays the image.
func TestApplyTemplateDefaultsReadsTheSpecsImageSources(t *testing.T) {
	rig := newImageSourcesRig(t, "defaults")
	req := dto.DeployRequest{TemplateID: rig.template.ID}
	selection, err := rig.h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Image != imageSourcesUpstream || !reflect.DeepEqual(selection.ImageSources, []string{imageSourcesSWR, imageSourcesNJU}) || selection.Deviated {
		t.Fatalf("image=%q sources=%v deviated=%v", req.Image, selection.ImageSources, selection.Deviated)
	}
	snapshot := snapshotTemplateSelection(selection, req.Image)
	if !reflect.DeepEqual(snapshot.ImageSources, selection.ImageSources) {
		t.Fatalf("snapshot sources = %v", snapshot.ImageSources)
	}
	if overridden := snapshotTemplateSelection(selection, "custom/vllm:cuda"); overridden.ImageSources != nil {
		t.Fatalf("an overridden image snapshotted the template's sources: %v", overridden.ImageSources)
	}
}

// A stored image_sources column that does not decode is reported with the spec's
// name rather than silently dropping the sources.
func TestApplyTemplateDefaultsRefusesUndecodableImageSources(t *testing.T) {
	rig := newImageSourcesRig(t, "defaults_corrupt")
	if err := rig.st.DB().Model(&store.InferenceSpecRow{}).Where("name = ?", "fp8-balanced").Update("image_sources", "not json").Error; err != nil {
		t.Fatal(err)
	}
	req := dto.DeployRequest{TemplateID: rig.template.ID}
	_, err := rig.h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err == nil || !strings.Contains(err.Error(), "inference spec fp8-balanced image_sources") {
		t.Fatalf("error = %v", err)
	}
}
