package deployment

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rankScopedComponentTemplate is the GB10 TensorFold shape: a primary pack the
// same on every rank and an auxiliary component whose subfolder names the
// rank. primarySubfolder makes the primary (and the spec's own weights)
// rank-scoped too.
func rankScopedComponentTemplate(t *testing.T, name, primarySubfolder string) (*store.Store, *Service, uint) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	primary := fmt.Sprintf(`{"provider":"huggingface","repo_id":"Acme/Pack","revision":"p1","subfolder":%q}`, primarySubfolder)
	fixture.Specs[0].WeightSources = "[" + primary + "]"
	fixture.Specs[0].WeightBytes = 7 << 30
	fixture.Specs[0].Components = `[{"name":"main","role":"primary","weights":{"quantization":"fp8","sources":[` + primary + `]},"weight_bytes":7516192768,"mount_path":"/model"},` +
		`{"name":"engram","role":"auxiliary","weights":{"quantization":"bf16","sources":[{"provider":"modelscope","repo_id":"Acme/Engram-TP2","revision":"e1","subfolder":"rank${GPUWORKS_NODE_RANK}"}]},"weight_bytes":3221225472,"mount_path":"/engram"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return st, New(Deps{Store: st}), template.ID
}

func componentByName(t *testing.T, components []deploy.ComponentBinding, name string) deploy.ComponentBinding {
	t.Helper()
	for _, component := range components {
		if component.Name == name {
			return component
		}
	}
	t.Fatalf("no component %s in %+v", name, components)
	return deploy.ComponentBinding{}
}

// A component whose subfolder names the rank resolves one artifact per rank
// at create, as the spec's own rank-scoped weights do: same provider,
// repository and revision, each rank its own subfolder, rank 0's as the
// component's StoreModelID. The primary stays one artifact for every rank,
// and the snapshot keeps the template's subfolder with the bindings.
func TestRankScopedComponentResolvesOneArtifactPerRank(t *testing.T) {
	st, h, templateID := rankScopedComponentTemplate(t, "rank_scoped_component_resolves", "")
	selection, model := rankScopedSelection(t, h, templateID, 2)
	if model.SourceID != "Acme/Pack" || model.ArtifactSubfolder != "" || len(selection.RankWeights) != 0 {
		t.Fatalf("deployment model %+v rank weights %+v, want one shared primary", model, selection.RankWeights)
	}
	engram := componentByName(t, selection.Components, "engram")
	if engram.Subfolder != "rank${GPUWORKS_NODE_RANK}" || len(engram.RankWeights) != 2 || engram.StoreModelID != engram.RankWeights[0].StoreModelID {
		t.Fatalf("engram binding = %+v", engram)
	}
	for rank, weight := range engram.RankWeights {
		artifact, err := st.GetModel(weight.StoreModelID)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("rank%d", rank)
		if weight.Rank != rank || weight.Subfolder != want || artifact.ArtifactSubfolder != want || artifact.SourceID != "Acme/Engram-TP2" ||
			artifact.Source != "modelscope" || artifact.Revision != "e1" || artifact.WeightBytes != 3<<30 {
			t.Fatalf("rank %d binding %+v artifact %+v", rank, weight, artifact)
		}
	}
	if engram.GroupWeightBytes() != 6<<30 {
		t.Fatalf("group weight bytes = %d, want two ranks' parts", engram.GroupWeightBytes())
	}

	// Resolving again (an evaluation before the create) reuses the rows.
	again, _ := rankScopedSelection(t, h, templateID, 2)
	if got := componentByName(t, again.Components, "engram"); !slices.Equal(got.RankWeights, engram.RankWeights) {
		t.Fatalf("second resolution bound %+v, want %+v", got.RankWeights, engram.RankWeights)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 3 {
		t.Fatalf("models = %d %v, want the primary and one engram row per rank", len(models), err)
	}

	snapshot, err := catalogSelectionDocument(selection, "img")
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ModelID: model.ID, CatalogSelectionSnapshot: snapshot, NodeIDs: "[7,8]"}
	for rank := range 2 {
		ids, err := planning.RankModelIDs(dep, rank)
		if err != nil {
			t.Fatal(err)
		}
		if want := []uint{model.ID, engram.RankWeights[rank].StoreModelID}; !slices.Equal(ids, want) {
			t.Fatalf("rank %d reads %v, want %v", rank, ids, want)
		}
	}
	all, err := planning.DeploymentModelIDs(dep)
	if err != nil || !slices.Equal(all, []uint{model.ID, engram.RankWeights[0].StoreModelID, engram.RankWeights[1].StoreModelID}) {
		t.Fatalf("deployment models = %v %v", all, err)
	}
	nodeModels, err := planning.NodeModelIDs(dep)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeModels(&store.Node{ID: 8}); !slices.Equal(got, []uint{model.ID, engram.RankWeights[1].StoreModelID}) {
		t.Fatalf("rank 1's node reads %v", got)
	}
	if got := nodeModels(&store.Node{ID: 9}); !slices.Equal(got, all) {
		t.Fatalf("a node at no rank reads %v, want every rank's %v", got, all)
	}
}

// What a create admits, plans and downloads of a rank-scoped component: each
// rank's artifact on its own rank's node only, told apart by its subfolder
// in the forecast.
func TestRankScopedComponentTargetsPairRanksWithTheirNodes(t *testing.T) {
	_, h, templateID := rankScopedComponentTemplate(t, "rank_scoped_component_targets", "")
	selection, _ := rankScopedSelection(t, h, templateID, 2)
	components, err := h.templateComponents(selection.Components)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 {
		t.Fatalf("components = %+v, want one entry per rank", components)
	}
	nodes := []*store.Node{{ID: 7, UUID: "lead"}, {ID: 8, UUID: "follow"}}
	for rank, component := range components {
		if component.rank != rank || component.model.ArtifactSubfolder != fmt.Sprintf("rank%d", rank) {
			t.Fatalf("entry %d = rank %d model %+v", rank, component.rank, component.model)
		}
		if got := component.nodesOf(nodes, nodes); len(got) != 1 || got[0] != nodes[rank] {
			t.Fatalf("rank %d read on %+v", rank, got)
		}
		if got := component.nodesOf(nodes[:1], nil); len(got) != 1 || got[0] != nodes[0] {
			t.Fatalf("a single-node deployment reads rank %d on %+v", rank, got)
		}
	}
	forecast := acquisitionComponents([]componentPlan{{templateComponent: components[1], plan: ComponentPlanNodeDownload, nodes: 1}})
	if forecast[0].Name != "engram" || forecast[0].Model != "Acme/Engram-TP2/rank1" || forecast[0].SizeBytes != 3<<30 {
		t.Fatalf("forecast = %+v", forecast[0])
	}
	shared := templateComponent{binding: deploy.ComponentBinding{Name: "draft", StoreModelID: 1}, model: &store.Model{ID: 1, Name: "draft"}}
	if got := shared.nodesOf(nodes, nodes); len(got) != 2 {
		t.Fatalf("a component every rank shares is read on %+v", got)
	}
}

// A single-node deployment of the shape binds rank 0's part only.
func TestRankScopedComponentOnOneNodeBindsRankZero(t *testing.T) {
	_, h, templateID := rankScopedComponentTemplate(t, "rank_scoped_component_one_node", "")
	selection, _ := rankScopedSelection(t, h, templateID, 1)
	engram := componentByName(t, selection.Components, "engram")
	if len(engram.RankWeights) != 1 || engram.RankWeights[0].Subfolder != "rank0" || engram.StoreModelID != engram.RankWeights[0].StoreModelID {
		t.Fatalf("engram = %+v", engram)
	}
}

// A primary component whose subfolder names the rank is the spec's own
// rank-scoped weights: the deployment's ranks bind it, and the component
// keeps the template's subfolder instead of rank 0's.
func TestRankScopedPrimaryComponentIsTheDeploymentsRankWeights(t *testing.T) {
	_, h, templateID := rankScopedComponentTemplate(t, "rank_scoped_primary_component", "rank${GPUWORKS_NODE_RANK}")
	selection, model := rankScopedSelection(t, h, templateID, 2)
	primary := componentByName(t, selection.Components, "main")
	if model.ArtifactSubfolder != "rank0" || len(selection.RankWeights) != 2 || primary.StoreModelID != model.ID ||
		primary.Subfolder != "rank${GPUWORKS_NODE_RANK}" || primary.RankScoped() {
		t.Fatalf("model %+v ranks %+v primary %+v", model, selection.RankWeights, primary)
	}
}

// A PD deployment numbers ranks per role instance, so a rank-scoped
// component is refused as PD like rank-scoped spec weights are.
func TestRankScopedComponentRefusesPD(t *testing.T) {
	_, h, templateID := rankScopedComponentTemplate(t, "rank_scoped_component_pd", "")
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: 2}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRankWeightsPD(selection, true); !containsProductCode(err, messages.ErrorRankWeightsPDUnsupported) {
		t.Fatalf("PD before resolution = %v", err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, templateID); err != nil {
		t.Fatal(err)
	}
	if !containsProductCode(checkRankWeightsPD(selection, true), messages.ErrorRankWeightsPDUnsupported) || checkRankWeightsPD(selection, false) != nil {
		t.Fatal("PD refusal of a rank-scoped component after resolution")
	}
}
