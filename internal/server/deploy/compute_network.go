package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// ComputeNetwork is the node-to-node collective network one rank uses. A
// zero value means the node has none and today's automatic selection applies.
type ComputeNetwork struct {
	Type string // a netplan type
	// Interface carries the bootstrap (socket) traffic: the NCCL/Gloo/HCCL
	// rendezvous, and every collective on Ethernet.
	Interface string
	// HCAs are the rails: RDMA ports as "device:port", handed to NCCL (all
	// rails) and UCX, or accelerator cards, which no environment names (HCCL
	// uses the cards the container was given).
	HCAs []string
	// LinkLayer is "infiniband" or "roce" as detected on this node's ports.
	LinkLayer string
}

// HostRDMA reports whether collectives must run over the host's RDMA verbs
// devices.
func (n ComputeNetwork) HostRDMA() bool {
	return n.Type == netplan.TypeHostRDMA
}

// Accelerator reports whether collectives run on the accelerator cards' own
// ports, which only the vendor's collective library drives.
func (n ComputeNetwork) Accelerator() bool {
	return netplan.IsAccelerator(n.Type)
}

// AcceleratorHCCS reports that accelerator traffic uses the Ascend A3 HCCS
// fabric rather than the card's RoCE ports.
func (n ComputeNetwork) AcceleratorHCCS() bool {
	return n.Type == netplan.TypeAcceleratorHCCS
}

// rdmaDockerArgs expose the RDMA verbs devices to the container and lift the
// locked-memory limit that registered memory regions need.
var rdmaDockerArgs = [][2]string{
	{"--device", "/dev/infiniband"},
	{"--ulimit", "memlock=-1"},
	{"--cap-add", "IPC_LOCK"},
}

// RequiresCollectiveInterface applies the accelerator's interface rule to
// TP×PP×DP. Unstated PP and DP each default to one; the multi-node rule stays
// with the accelerator.
func RequiresCollectiveInterface(p Params) bool {
	pp, dp := p.PipelineParallel, p.DataParallel
	if pp < 1 {
		pp = 1
	}
	if dp < 1 {
		dp = 1
	}
	return accelerator.RequiresCollectiveInterface(p.Vendor, p.NNodes, p.TensorParallel*pp*dp)
}

// computeNetworkEnv is the system-layer environment for a multi-node rank or
// a target whose single-node multi-device collective needs a real interface.
// Host RDMA networks force the IB transport so a container that cannot see
// the verbs devices fails at startup instead of silently falling back to TCP
// sockets at a fraction of the bandwidth. An accelerator network names
// the host address and interface the vendor's collective library builds its
// communicator over (accelerator.CardNetworkOf says in which variables): left
// to pick among a host's interfaces HCCL picks wrong and no rank reaches the
// root (docs/adr/0024-network-objects.md); the data itself moves on the
// cards' ports. Template and user environment layers still override every key.
func computeNetworkEnv(p Params) map[string]string {
	n := p.ComputeNetwork
	if n.Type == "" {
		return nil
	}
	if p.NNodes <= 1 {
		if RequiresCollectiveInterface(p) && n.Interface != "" {
			return map[string]string{"NCCL_SOCKET_IFNAME": n.Interface, "GLOO_SOCKET_IFNAME": n.Interface}
		}
		return nil
	}
	env := map[string]string{}
	if n.Accelerator() {
		cards, _ := accelerator.CardNetworkOf(p.Vendor)
		if n.Interface != "" {
			env["GLOO_SOCKET_IFNAME"] = n.Interface
			for _, key := range cards.InterfaceEnv {
				env[key] = n.Interface
			}
		}
		if p.NodeAddress != "" && !n.AcceleratorHCCS() {
			for _, key := range cards.AddressEnv {
				env[key] = p.NodeAddress
			}
		}
		if n.AcceleratorHCCS() {
			env["ASCEND_GLOBAL_RESOURCE_CONFIG"] = `{"comm_resource_config.protocol_desc":["hccs:device"]}`
			env["ACL_OP_INIT_MODE"] = "1"
			env["ASCEND_ENABLE_USE_FABRIC_MEM"] = "1"
		}
		return env
	}
	if n.Interface != "" {
		env["NCCL_SOCKET_IFNAME"] = n.Interface
		env["GLOO_SOCKET_IFNAME"] = n.Interface
	}
	if !n.HostRDMA() {
		env["NCCL_IB_DISABLE"] = "1"
		return env
	}
	env["NCCL_IB_DISABLE"] = "0"
	env["NCCL_NET"] = "IB"
	if len(n.HCAs) > 0 {
		env["NCCL_IB_HCA"] = strings.Join(n.HCAs, ",")
		// UCX (NIXL KV transfer) gets one rail: "all" lets it pick a TCP
		// device when verbs are unusable, which is the fallback to avoid.
		env["UCX_NET_DEVICES"] = n.HCAs[0]
	}
	return env
}

// onComputeNetwork reports whether the instance's traffic rides its
// compute network: a multi-node rank's collectives, or a PD instance's KV
// transfer.
func onComputeNetwork(p Params) bool {
	return p.NNodes > 1 || p.PD != nil
}

// withComputeNetworkDockerArgs appends the RDMA container options a
// multi-node rank or a PD instance on a host RDMA network needs, skipping any
// already present. An accelerator network needs none: the cards' ports
// are not under /dev/infiniband, and the cards come with device injection.
func withComputeNetworkDockerArgs(args []string, p Params) []string {
	if !onComputeNetwork(p) || !p.ComputeNetwork.HostRDMA() {
		return args
	}
	out := append([]string(nil), args...)
	for _, opt := range rdmaDockerArgs {
		// The locked-memory limit is the deployer's to state; any memlock they
		// set stands, so the option is never repeated (see launchDockerArgs).
		if opt[0] == "--ulimit" && hasMemlockUlimit(out) {
			continue
		}
		if !hasDockerArgValue(out, opt[0], opt[1]) {
			out = append(out, opt[0], opt[1])
		}
	}
	return out
}

// computeNetworkMounts are the host files an instance whose traffic moves
// on the accelerator cards' ports needs, read-only (accelerator.CardNetworkOf
// names them): a multi-node rank or a PD instance on an accelerator RDMA
// network, and a PD instance whose mode hands the KV to the cards whatever
// network it sits on (on one host there may be none).
func computeNetworkMounts(p Params) []Mount {
	onCards := onComputeNetwork(p) && p.ComputeNetwork.Accelerator()
	if p.PD != nil && p.PD.mode().Transport == pdmode.TransportHCCL {
		onCards = true
	}
	if !onCards {
		return nil
	}
	cards, _ := accelerator.CardNetworkOf(p.Vendor)
	var out []Mount
	for _, file := range cards.HostFiles {
		out = append(out, Mount{Source: file, Target: file, ReadOnly: true, Purpose: proto.MountPurposeComputeNetwork})
	}
	return out
}

// hasDockerArgValue matches "--flag value" and "--flag=value" forms.
func hasDockerArgValue(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg == flag+"="+value || (arg == flag && i+1 < len(args) && args[i+1] == value) {
			return true
		}
	}
	return false
}
