package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The node's placement policy reaches the scheduler through the snapshot,
// and the operator's choice of cards through Input.Consent.
func TestEvaluateAppliesTheNodePolicyAndTheOperatorsConsent(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	node.GPUs[1].ForeignProcesses = node.GPUs[0].ForeignProcesses // both cards held
	in := Input{
		Model: &store.Model{WeightBytes: 15 << 30}, Node: node, Engine: "vllm",
		Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, VRAMLimitBytes: 50 << 30,
		PartitionBackend: accelerator.BackendNVIDIALogical,
	}
	if snap := NodeSnapshot(st, node, true, ""); snap.Policy != node.PlacementPolicy {
		t.Fatalf("snapshot policy = %+v, want the node's", snap.Policy)
	}
	out := in.Evaluate(st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonHeldOutsidePlatformNotAllowed ||
		out.Result.Message.Params["setting"] != string(nodepolicy.KeyShareWithForeign) {
		t.Fatalf("switched off, automatic = ok %v %+v", out.Result.OK, out.Result.Message)
	}
	chosen := in
	chosen.GPUIndexes, chosen.Consent = []int{0}, nodepolicy.Consent{CardsChosen: true}
	if out := chosen.Evaluate(st); !out.Result.OK || out.Result.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("switched off, chosen card = ok %v %+v", out.Result.OK, out.Result.Message)
	}
	node.PlacementPolicy.ShareWithForeign = true
	out = in.Evaluate(st)
	if !out.Result.OK || out.Result.Plan == nil {
		t.Fatalf("switched on = ok %v %+v", out.Result.OK, out.Result.Message)
	}
	// The card list is judged as picked by the operator either way.
	node.PlacementPolicy.ShareWithForeign = false
	out = in.Evaluate(st)
	if !candidateAvailable(t, out.GPUCandidates, 0) {
		t.Fatalf("held card not offered while switched off: %+v", out.GPUCandidates)
	}
	for _, candidate := range out.GPUCandidates {
		if candidate.Index == 0 && (candidate.Note == nil || candidate.Note.Code != scheduler.ReasonForeignSharedCard) {
			t.Fatalf("held card note = %+v", candidate.Note)
		}
	}
}

// Consent comes from the "gpus" key alone: a launch built from a stored
// placement carries it, an echoed plan ("gpu_indexes") and an unpinned
// placement do not.
func TestLaunchInputConsentsOnlyForTheOperatorsCards(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	model := &store.Model{WeightBytes: 15 << 30}
	for _, tc := range []struct {
		name      string
		placement string
		want      bool
	}{
		{"operator's cards", `{"gpus":[0]}`, true},
		{"echoed plan", `{"gpu_indexes":[0]}`, false},
		{"scheduler's choice", `{"tp":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placement, err := ParsePlacement(tc.placement)
			if err != nil {
				t.Fatal(err)
			}
			input, err := launchInput(st, LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: placement, Topology: DistTopology{NNodes: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if input.Consent.CardsChosen != tc.want {
				t.Fatalf("consent = %+v, want CardsChosen %v", input.Consent, tc.want)
			}
		})
	}
}

// A replica the pool places elsewhere lets the scheduler pick its cards:
// DeploymentLaunch drops the indexes, and the consent with them.
func TestDeploymentLaunchOffItsPinnedNodeCarriesNoConsent(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	dep := &store.Deployment{Engine: "vllm", Placement: `{"gpus":[0]}`, NNodes: 1}
	pinned := map[string][]string{"gpuworks.ai/node": {node.UUID}}
	for _, tc := range []struct {
		name string
		reqs map[string][]string
		want bool
	}{
		{"pinned to the node", pinned, true},
		{"placed by the pool", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, _, err := DeploymentLaunch(dep, tc.reqs, node, &store.Model{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.Placement.OperatorPinned() != tc.want {
				t.Fatalf("operator pinned = %v, want %v (%+v)", req.Placement.OperatorPinned(), tc.want, req.Placement)
			}
		})
	}
}

// Statistics count a held card as capacity only where the scheduler would
// place on it automatically: the node must allow it.
func TestPartitionMaySharedHeldCardFollowsTheNodePolicy(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	if PartitionMayShareHeldCard(node, &node.GPUs[0]) {
		t.Fatal("a held card counts as capacity while the node does not allow sharing")
	}
	node.PlacementPolicy.ShareWithForeign = true
	if !PartitionMayShareHeldCard(node, &node.GPUs[0]) {
		t.Fatal("a held card the node allows sharing does not count")
	}
	if bytes, known := ForeignHold(node, &node.GPUs[0]); !known || bytes <= reporterForeign.MemoryBytes {
		t.Fatalf("foreign hold = %d %v: memory is counted whatever the policy", bytes, known)
	}
	node.PlacementPolicy.ShareWithForeign = false
	if bytes, known := ForeignHold(node, &node.GPUs[0]); !known || bytes <= reporterForeign.MemoryBytes {
		t.Fatalf("foreign hold switched off = %d %v", bytes, known)
	}
}

// Cards an operator pins on a PD unit or a multi-node rank are the
// operator's choice as much as the top-level "gpus": the rank's launch
// carries the consent, so a hand-picked held card that fits by memory is
// admitted on a node whose switch is off. A unit the platform places names
// no cards, and gives none.
func TestPDRankLaunchConsentsForTheUnitsPinnedCards(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	model := &store.Model{WeightBytes: 15 << 30}
	mode, _ := pdmode.Lookup("vllm-nixl")
	pd := &PDPlacement{}
	for _, tc := range []struct {
		name string
		unit PDUnit
		want bool
	}{
		{"pinned unit", PDUnit{NodeUUID: node.UUID, GPUIndexes: []int{0}}, true},
		{"placed unit", PDUnit{NodeUUID: node.UUID}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := LaunchRequest{Engine: "vllm", Model: model, Node: node}
			pd.RankLaunch(mode, pdmode.RolePrefill, tc.unit, 0, DistTopology{NNodes: 1}, PDRankPorts{}).Apply(&req)
			input, err := launchInput(st, req)
			if err != nil {
				t.Fatal(err)
			}
			if input.Consent.CardsChosen != tc.want {
				t.Fatalf("consent = %+v, want CardsChosen %v", input.Consent, tc.want)
			}
		})
	}
}
