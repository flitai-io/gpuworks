package planning

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// componentRankSelection is a shared primary (model 1) with an auxiliary
// component split per rank (models 20 and 21) and a draft every rank shares
// (model 30).
func componentRankSelection() deploy.CatalogSelection {
	return deploy.CatalogSelection{Components: []deploy.ComponentBinding{
		{Name: "main", Role: "primary", StoreModelID: 1, MountPath: "/model"},
		{Name: "engram", Role: "auxiliary", Subfolder: "rank${GPUWORKS_NODE_RANK}", StoreModelID: 20, WeightBytes: 5, MountPath: "/engram",
			RankWeights: []deploy.RankWeight{{Rank: 0, Subfolder: "rank0", StoreModelID: 20}, {Rank: 1, Subfolder: "rank1", StoreModelID: 21}}},
		{Name: "draft", Role: "speculative_draft", StoreModelID: 30, WeightBytes: 3, MountPath: "/draft"},
	}}
}

func componentRankDeployment(t *testing.T, selection deploy.CatalogSelection) *store.Deployment {
	t.Helper()
	snapshot, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := json.Marshal([]uint{3, 4})
	return &store.Deployment{ID: 1, Engine: "vllm", ModelID: 1, NNodes: 2, NodeIDs: string(ids), CatalogSelectionSnapshot: string(snapshot)}
}

// A rank reads its own artifact of a rank-scoped component and the
// components every rank shares; the deployment's set holds every rank's.
func TestRankModelIDsReadARankScopedComponentByRank(t *testing.T) {
	dep := componentRankDeployment(t, componentRankSelection())
	for rank, want := range [][]uint{{1, 20, 30}, {1, 21, 30}} {
		got, err := RankModelIDs(dep, rank)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("rank %d models = %v %v, want %v", rank, got, err, want)
		}
	}
	all, err := DeploymentModelIDs(dep)
	if err != nil || !slices.Equal(all, []uint{1, 20, 21, 30}) {
		t.Fatalf("deployment models = %v %v", all, err)
	}
	nodeModels, err := NodeModelIDs(dep)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeModels(&store.Node{ID: 4}); !slices.Equal(got, []uint{1, 21, 30}) {
		t.Fatalf("rank 1's node reads %v", got)
	}
	// A rank the component binds nothing for is an error, not rank 0's part.
	if _, err := RankModelIDs(dep, 2); err == nil {
		t.Fatal("rank 2 of a two-rank component was given a model")
	}
	broken := componentRankSelection()
	broken.Components[1].RankWeights[1].StoreModelID = 0
	if _, err := DeploymentModelIDs(componentRankDeployment(t, broken)); err == nil {
		t.Fatal("a rank without a model binding was accepted")
	}
}

// The estimate holds every rank's part of a rank-scoped component, as it
// holds every rank's share of rank-scoped weights; what a rank's node stores
// is its own part.
func TestComponentSumCountsARankScopedComponentPerRank(t *testing.T) {
	bytes, artifacts := componentSum(componentRankSelection().Components, 1)
	if bytes != 2*5+3 {
		t.Fatalf("component weight bytes = %d, want both engram parts and the draft", bytes)
	}
	want := []Artifact{{ModelID: 21, StorageBytes: 5}, {ModelID: 30, StorageBytes: 3}}
	if !slices.Equal(artifacts, want) {
		t.Fatalf("rank 1 artifacts = %+v, want %+v", artifacts, want)
	}
}

// Each rank's launch mounts its own artifact of a rank-scoped component at the
// component's mount path, and the paths it was resolved for decide which.
func TestDeploymentLaunchMountsTheRanksComponentArtifact(t *testing.T) {
	dep := componentRankDeployment(t, componentRankSelection())
	node := &store.Node{UUID: "node", GPUVendor: "nvidia", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}
	paths := &ModelPaths{Primary: "/m/pack", Components: map[uint]string{1: "/m/pack", 21: "/m/engram", 30: "/m/draft"}, Rank: 1}
	req, _, primary, err := DeploymentLaunch(dep, nil, node, &store.Model{ID: 1, Name: "pack"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if primary != "/m/pack" || len(req.ComponentMounts) != 2 || req.ComponentMounts[0].Source != "/m/engram" || req.ComponentMounts[0].Target != "/engram" || req.ComponentMounts[1].Source != "/m/draft" {
		t.Fatalf("rank 1 mounts primary %q components %+v", primary, req.ComponentMounts)
	}
	paths.Rank = 0
	if _, _, _, err := DeploymentLaunch(dep, nil, node, &store.Model{ID: 1, Name: "pack"}, paths); err == nil {
		t.Fatal("rank 0 launched without rank 0's component artifact")
	}

	// A rank-scoped primary component mounts the rank's own artifact.
	selection := componentRankSelection()
	selection.Components = selection.Components[:1]
	selection.Components[0].Subfolder = "rank${GPUWORKS_NODE_RANK}"
	selection.RankWeights = []deploy.RankWeight{{Rank: 0, Subfolder: "rank0", StoreModelID: 1}, {Rank: 1, Subfolder: "rank1", StoreModelID: 2}}
	dep = componentRankDeployment(t, selection)
	_, _, primary, err = DeploymentLaunch(dep, nil, node, &store.Model{ID: 1, Name: "pack"}, &ModelPaths{Primary: "/m/rank1", Components: map[uint]string{2: "/m/rank1"}, Rank: 1})
	if err != nil || primary != "/m/rank1" {
		t.Fatalf("rank 1 primary = %q %v", primary, err)
	}
}
