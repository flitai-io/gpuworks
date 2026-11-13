package planning

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// GroupNetworks is what a trial of a group of nodes needs from the network
// service: the launch's own checks and the network the group shares.
type GroupNetworks interface {
	Networks
	ComputeGroup(nodes []store.Node) (netplan.Group, error)
}

// UnitTrial plans proposed units the way their launch would: the group's
// shared network, the deployment's stored configuration (the role's, for PD)
// and the template's hardware contract. Nothing is reserved; a trial only
// says whether the launch would be refused, and on which node.
type UnitTrial struct {
	Store        *store.Store
	Networks     GroupNetworks
	Deployment   *store.Deployment
	Requirements map[string][]string
	Model        *store.Model
	// PD and Mode are set for a PD deployment: the placement the units
	// belong to (its roles' parallelism) and its mode.
	PD   *PDPlacement
	Mode pdmode.Mode
}

// Unit plans the ranks of unit that differ from original (a rank kept on its
// node may still hold its own cards; a zero original plans every rank) and
// returns the node that refused, "" when every rank plans.
func (t UnitTrial) Unit(ctx context.Context, role string, original, unit PDUnit, nodes map[string]*store.Node) (string, *scheduler.Reason, error) {
	ranks := unit.Ranks()
	rows := make([]store.Node, 0, len(ranks))
	for _, rank := range ranks {
		node := nodes[rank.NodeUUID]
		if node == nil {
			return rank.NodeUUID, &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": "node not found"}}, nil
		}
		rows = append(rows, *node)
	}
	group, err := t.Networks.ComputeGroup(rows)
	if err != nil {
		return ranks[0].NodeUUID, &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}, nil //nolint:nilerr // Network rejection is returned as a ScheduleReason.
	}
	kept := original.Ranks()
	for i, rank := range ranks {
		node := nodes[rank.NodeUUID]
		if i < len(kept) && kept[i].NodeUUID == rank.NodeUUID {
			continue
		}
		launch, layer, _, err := DeploymentLaunch(t.Deployment, t.Requirements, node, t.Model, nil)
		if err != nil {
			return "", nil, err
		}
		launch.Preview = true
		launch.Topology = PreviewRankTopology(len(ranks), i, group.Ranks[node.ID])
		if t.PD != nil {
			t.PD.RankLaunch(t.Mode, role, unit, i, launch.Topology, PDRankPorts{}).Apply(&launch)
			launch.Preview = true
		}
		_, refusal, err := PlanTemplateLaunch(ctx, t.Store, t.Networks, node, launch, layer)
		if err != nil {
			return "", nil, err
		}
		if refusal != nil {
			return node.UUID, refusal, nil
		}
	}
	return "", nil, nil
}

// PDNetwork checks the network a PD deployment's members would exchange KV
// over: one compute network across all of them (a single node needs none)
// that carries the mode's transport.
func (t UnitTrial) PDNetwork(members []store.Node) error {
	group, err := t.Networks.ComputeGroup(members)
	group, err = PDGroup(members, group, err)
	if err != nil {
		return err
	}
	return CheckPDNetwork(t.Mode, members, group, t.Deployment.Sleepable)
}
