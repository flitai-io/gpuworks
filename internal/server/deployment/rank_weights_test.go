package deployment

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func rankScopedTemplate(t *testing.T, name string) (*store.Store, *Service, uint) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Deploy-TP3","revision":"r1","subfolder":"rank${NODE_RANK}"}]`
	fixture.Specs[0].WeightBytes = 5 << 30
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return st, New(Deps{Store: st}), template.ID
}

func rankScopedSelection(t *testing.T, h *Service, templateID uint, nnodes int) (templateSelection, *store.Model) {
	t.Helper()
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: nnodes}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, templateID)
	if err != nil {
		t.Fatal(err)
	}
	return selection, model
}

// A rank-scoped spec resolves one artifact per rank at create (ADR 0076):
// rank 0's is the deployment's own model, every rank shares the repository
// and revision, each has its own subfolder, and the snapshot keeps the
// template's subfolder with the bindings.
func TestRankScopedTemplateResolvesOneArtifactPerRank(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_resolves")
	selection, model := rankScopedSelection(t, h, templateID, 3)
	if model.ArtifactSubfolder != "rank0" || model.SourceID != "Acme/Deploy-TP3" || model.Revision != "r1" || model.WeightBytes != 5<<30 {
		t.Fatalf("deployment model = %+v, want rank 0's artifact", model)
	}
	if len(selection.RankWeights) != 3 {
		t.Fatalf("rank weights = %+v", selection.RankWeights)
	}
	for rank, weight := range selection.RankWeights {
		artifact, err := st.GetModel(weight.StoreModelID)
		if err != nil {
			t.Fatal(err)
		}
		want := deploy.ExpandRankSubfolder("rank${NODE_RANK}", rank)
		if weight.Rank != rank || weight.Subfolder != want || artifact.ArtifactSubfolder != want || artifact.Source != "modelscope" || artifact.Revision != "r1" || artifact.SourceID != "Acme/Deploy-TP3" {
			t.Fatalf("rank %d binding %+v artifact %+v", rank, weight, artifact)
		}
	}
	if selection.RankWeights[0].StoreModelID != model.ID {
		t.Fatalf("rank 0 is not the deployment's own model: %+v", selection.RankWeights[0])
	}
	snapshot := snapshotTemplateSelection(selection, "vllm/vllm-openai:latest")
	if snapshot.WeightSubfolder != "rank${NODE_RANK}" || len(snapshot.RankWeights) != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	// Resolving again (an evaluation before the create) reuses the rows.
	again, model2 := rankScopedSelection(t, h, templateID, 3)
	if model2.ID != model.ID || again.RankWeights[2].StoreModelID != selection.RankWeights[2].StoreModelID {
		t.Fatalf("second resolution registered new rows: %+v vs %+v", again.RankWeights, selection.RankWeights)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 3 {
		t.Fatalf("models = %d %v, want one per rank", len(models), err)
	}
}

// A single-node deployment of a rank-scoped spec reads rank 0's part only,
// and its snapshot binds that one rank.
func TestRankScopedTemplateOnOneNodeBindsRankZero(t *testing.T) {
	_, h, templateID := rankScopedTemplate(t, "rank_scoped_template_one_node")
	selection, model := rankScopedSelection(t, h, templateID, 1)
	if model.ArtifactSubfolder != "rank0" || len(selection.RankWeights) != 1 || selection.RankWeights[0].StoreModelID != model.ID {
		t.Fatalf("model %+v weights %+v", model, selection.RankWeights)
	}
}

// A named model must be rank 0's artifact: it is what rank 0 mounts. Another
// rank's subfolder of the same repository is refused with its own code.
func TestRankScopedTemplateRefusesAnotherRanksModel(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_named_model")
	rank1, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r1", ArtifactSubfolder: "rank1", Name: "Acme/Deploy-TP3"})
	if err != nil {
		t.Fatal(err)
	}
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: 2}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.resolveDeploymentModelFromTemplate(context.Background(), rank1.ID, &selection, templateID)
	if !containsProductCode(err, messages.ErrorRankWeightsModelNotRankZero) {
		t.Fatalf("rank 1's model as the deployment's = %v", err)
	}
	whole, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r1", Name: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), whole.ID, &selection, templateID); err == nil {
		t.Fatal("the whole repository was accepted for a rank-scoped spec")
	}
	if checkRankWeightsPD(selection, true) == nil || checkRankWeightsPD(selection, false) != nil {
		t.Fatal("PD refusal of rank-scoped weights")
	}
}

// What a create admits and downloads: each rank's artifact on its own rank's
// node only.
func TestRankScopedWeightTargetsPairRanksWithTheirNodes(t *testing.T) {
	_, h, templateID := rankScopedTemplate(t, "rank_scoped_weight_targets")
	selection, model := rankScopedSelection(t, h, templateID, 2)
	nodes := []*store.Node{{ID: 7, UUID: "lead"}, {ID: 8, UUID: "follow"}}
	targets, err := h.weightTargets(selection, model, append([]*store.Node{nodes[0]}, nodes...), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
	for rank, target := range targets {
		if target.model.ID != selection.RankWeights[rank].StoreModelID || len(target.nodes) != 1 || target.nodes[0] != nodes[rank] {
			t.Fatalf("rank %d target = model %d nodes %+v", rank, target.model.ID, target.nodes)
		}
	}
	plain, err := h.weightTargets(templateSelection{}, model, nodes, nodes)
	if err != nil || len(plain) != 1 || len(plain[0].nodes) != 2 {
		t.Fatalf("unscoped targets = %+v %v", plain, err)
	}
	// The stored deployment reads per rank through planning.
	snapshot, err := catalogSelectionDocument(selection, "img")
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ModelID: model.ID, CatalogSelectionSnapshot: snapshot}
	if ids, err := planning.RankModelIDs(dep, 1); err != nil || ids[0] != selection.RankWeights[1].StoreModelID {
		t.Fatalf("rank 1 models = %v %v", ids, err)
	}
}

// The template's revision decides a rank-scoped deployment's artifacts: rows
// registered at another known revision are not substitutes, however ready
// they are, so a re-imported template at a new revision creates (and so
// downloads) its own artifacts for every rank. Naming the old revision's rank 0
// as the model is refused as a model of another revision.
func TestRankScopedTemplateUsesTheRevisionItStatesNow(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_revision")
	var stale []*store.Model
	for rank := range 3 {
		model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r0", ArtifactSubfolder: deploy.ExpandRankSubfolder("rank${NODE_RANK}", rank), Name: "Acme/Deploy-TP3"})
		if err != nil {
			t.Fatal(err)
		}
		stale = append(stale, model)
	}
	selection, model := rankScopedSelection(t, h, templateID, 3)
	if model.Revision != "r1" || model.ID == stale[0].ID || selection.Weight.Revision != "r1" {
		t.Fatalf("deployment model = %+v (snapshot revision %q), want a new rank 0 artifact at r1", model, selection.Weight.Revision)
	}
	for rank, weight := range selection.RankWeights {
		artifact, err := st.GetModel(weight.StoreModelID)
		if err != nil {
			t.Fatal(err)
		}
		if artifact.Revision != "r1" || artifact.ID == stale[rank].ID {
			t.Fatalf("rank %d binds %+v, want a new artifact at r1 instead of %+v", rank, artifact, stale[rank])
		}
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 6 {
		t.Fatalf("models = %d %v, want the 3 old rows and one new row per rank", len(models), err)
	}

	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: 3}
	named, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), stale[0].ID, &named, templateID); !containsProductCode(err, messages.ErrorTemplateModelMismatch) {
		t.Fatalf("rank 0 of the old revision named for the new one = %v, want %s", err, messages.ErrorTemplateModelMismatch)
	}
}

// A copy whose revision is unknown (scanned from a storage) is not another
// revision: a rank-scoped template still reuses it, as ADR 0018 does for any
// source, and every rank of the group follows rank 0's revision.
func TestRankScopedTemplateReusesAnUnknownRevisionCopy(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_template_unknown_revision")
	rank0, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", ArtifactSubfolder: "rank0", Name: "Acme/Deploy-TP3"})
	if err != nil {
		t.Fatal(err)
	}
	selection, model := rankScopedSelection(t, h, templateID, 2)
	if model.ID != rank0.ID || selection.RankWeights[1].Subfolder != "rank1" {
		t.Fatalf("model %+v ranks %+v, want the unknown-revision rank 0 row %d reused", model, selection.RankWeights, rank0.ID)
	}
}

// The deploy form offers a rank-scoped spec's registered models as one
// choice: rank 0's artifact at the revision now stated. The other ranks'
// artifacts, and any revision the template no longer states, are not offered.
func TestRankScopedWeightOffersOnlyRankZeroAtTheStatedRevision(t *testing.T) {
	source := weightSource{Provider: "modelscope", RepoID: "Acme/Deploy-TP3", Revision: "r1", Subfolder: "rank${GPUWORKS_NODE_RANK}"}
	model := func(revision, subfolder string) *store.Model {
		return &store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: revision, ArtifactSubfolder: subfolder}
	}
	for _, tc := range []struct {
		name  string
		model *store.Model
		want  bool
	}{
		{"rank 0 at the revision", model("r1", "rank0"), true},
		{"rank 0 of another revision", model("r0", "rank0"), false},
		{"rank 0 of an unknown revision", model("", "rank0"), true},
		{"rank 1 at the revision", model("r1", "rank1"), false},
		{"whole repository", model("r1", ""), false},
	} {
		if got := TemplateWeightOffersModel(source, tc.model); got != tc.want {
			t.Errorf("%s: offered = %t, want %t", tc.name, got, tc.want)
		}
	}
	// Naming any rank of the revision stays a match, so a request naming
	// rank 1 is refused with the rank-zero code, not as another model.
	if !TemplateWeightMatchesModel(source, model("r1", "rank1")) || TemplateWeightMatchesModel(source, model("r0", "rank1")) {
		t.Fatal("rank-scoped match must follow the revision and accept every rank")
	}
}

// The revision rule of a rank-scoped template is the strict one; a whole
// repository keeps ADR 0018's: an explicit provider choice downloads the
// revision the template states, while the automatic choice reuses what the
// user already registered of that repository.
func TestWholeRepositoryTemplateRevisionKeepsItsPreferenceRule(t *testing.T) {
	st, err := store.Open("file:whole_repository_revision?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Deploy-TP3","revision":"r1"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-TP3", Revision: "r0", Name: "Acme/Deploy-TP3"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	for _, explicit := range []bool{false, true} {
		req := dto.DeployRequest{TemplateID: template.ID, TemplateSpecName: "fp8-balanced"}
		if explicit {
			req.ModelSource = &dto.ModelSource{Provider: "modelscope"}
		}
		selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
		if err == nil && explicit {
			err = h.selectExplicitWeight(&selection, req.ModelSource)
		}
		if err != nil {
			t.Fatal(err)
		}
		model, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, template.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reused := model.ID == registered.ID; reused == explicit || (explicit && model.Revision != "r1") {
			t.Fatalf("explicit provider=%t resolved %+v (registered %d at r0)", explicit, model, registered.ID)
		}
	}
}

// The create's recommendation previews a rank-scoped spec before its
// preflight; the preview binds every rank but registers no artifact, so a
// create the preflight then refuses leaves the model list as it was.
func TestRankScopedPreviewRegistersNoArtifact(t *testing.T) {
	st, h, templateID := rankScopedTemplate(t, "rank_scoped_preview_registers_none")
	req := dto.DeployRequest{TemplateID: templateID, TemplateSpecName: "fp8-balanced", NNodes: 3}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	selection.previewWeights = true
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, templateID); err != nil {
		t.Fatal(err)
	}
	if len(selection.RankWeights) != 3 {
		t.Fatalf("rank weights = %+v, want one per rank", selection.RankWeights)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 0 {
		t.Fatalf("preview registered %d models (%v), want none", len(models), err)
	}
}
