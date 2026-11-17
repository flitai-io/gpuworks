package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// PDRankPorts are the endpoints one rank of a PD unit launches with. The
// controller reserves the ports on the node; a preview shows placeholders.
type PDRankPorts struct {
	// API is the serving port; zero on a follower, and in a preview, which
	// then shows PreviewAPIPort.
	API int
	// SideChannel is the first port of the rank's extra block (NIXL side
	// channels, a bootstrap server, a Mooncake kv_port band); zero when the
	// mode needs none.
	SideChannel int
	// Master is the port a multi-node unit's ranks meet on, on the leader;
	// zero on a single-node unit.
	Master int
	// LeaderAddress is where a follower meets its leader. A leader meets its
	// followers on its own address.
	LeaderAddress string
}

// PDRankLaunch is the PD part of one rank's launch request: what makes a
// launch of the deployment the given rank of the given unit of a role. The
// preview and the controller both take it from PDPlacement.RankLaunch and
// supply only the ports, so what the form shows is what the controller runs.
type PDRankLaunch struct {
	Role       string
	Topology   DistTopology
	APIPort    int
	GPUIndexes []int
	TP, PP     int
	MasterAddr string
	MasterPort int
	PD         *deploy.PDLaunch
}

// RankLaunch derives the PD part of the launch of rank (an index into
// unit.Ranks()) of a unit of role, placed with topology (PDRankTopology) and
// the given ports.
func (p *PDPlacement) RankLaunch(mode pdmode.Mode, role string, unit PDUnit, rank int, topology DistTopology, ports PDRankPorts) PDRankLaunch {
	spec := p.Parallelism(role, unit)
	out := PDRankLaunch{
		Role: role, Topology: topology, APIPort: ports.API,
		TP: spec.TP, PP: spec.PP,
		PD: &deploy.PDLaunch{
			Mode: mode.Name, Role: role, KVLoadFailure: p.EffectiveKVLoadFailure(),
			SideChannelHost: topology.NodeAddress, SideChannelPort: ports.SideChannel, RoleTP: p.RoleTPs(mode),
		},
	}
	if ranks := unit.Ranks(); rank >= 0 && rank < len(ranks) {
		out.GPUIndexes = ranks[rank].GPUIndexes
	}
	if ports.Master > 0 {
		out.MasterPort, out.MasterAddr = ports.Master, ports.LeaderAddress
		if rank == 0 {
			out.MasterAddr = topology.NodeAddress
		}
	}
	return out
}

// Apply writes the rank's part into a launch request built from the
// deployment: the rank's topology, port, GPUs and parallelism, the role's
// launch configuration, the collective meeting point and the PD launch.
// A rank's cards come only from the "gpus" of its unit or follower in the
// placement document - the platform's unit picks and node replacements name
// nodes, never cards - so cards there are the operator's choice and consent
// to sharing them as the top-level "gpus" does (Placement.OperatorPinned).
func (l PDRankLaunch) Apply(req *LaunchRequest) {
	req.Topology, req.APIPort = l.Topology, l.APIPort
	req.Placement = Placement{GPUIndexes: l.GPUIndexes, TP: l.TP, PP: l.PP, operatorChose: len(l.GPUIndexes) > 0}
	req.Quick = deploy.PDRoleLaunchConfig(req.Engine, req.Quick, l.Role)
	req.MasterAddr, req.MasterPort = l.MasterAddr, l.MasterPort
	req.PD = l.PD
}
