package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/agent/ucxnetlink"
	"gpuworks.ai/gpuworks/internal/proto"
)

// ucxNetlinkInterval is how often the host's IPv6 route dump is probed: as
// often as the cards' runtime checks, since a sysctl or a kernel update can
// change the answer at any time but rarely does.
const ucxNetlinkInterval = cardsmoke.DefaultInterval

// UCXNetlinkHang is whether this host's kernel answers an IPv6 route dump in
// a way that hangs UCX (and so NIXL, the PD KV transport), reported with the
// node's status; nil while it does not, while that is not known, and off
// Linux. The probe is bounded and never blocks this report (package
// ucxnetlink).
func (n *NodeRuntime) UCXNetlinkHang(ctx context.Context) *proto.UCXNetlinkHang {
	n.ucxNetlinkOnce.Do(func() { n.ucxNetlink = ucxnetlink.NewMonitor(n.log, ucxNetlinkInterval) })
	return n.ucxNetlink.Status(ctx)
}

// doctorUCXNetlink probes now; a test replaces it.
var doctorUCXNetlink = func(ctx context.Context) (*proto.UCXNetlinkHang, bool) {
	return ucxnetlink.NewMonitor(nil, time.Minute).Fresh(ctx)
}

// doctorUCXNetlinkHang reports the same condition the node status carries, so
// a diagnosis never reads OK where PD over NIXL cannot start.
func doctorUCXNetlinkHang(ctx context.Context, r *proto.DoctorReport) {
	hang, _ := doctorUCXNetlink(ctx)
	if hang == nil {
		return
	}
	evidence := []proto.DoctorField{
		{Name: "fix", Value: proto.UCXNetlinkFixNow},
		{Name: "fix_persist", Value: proto.UCXNetlinkFixPersist},
	}
	if hang.Kernel != "" {
		evidence = append([]proto.DoctorField{{Name: "kernel", Value: hang.Kernel}}, evidence...)
	}
	r.Findings = append(r.Findings, proto.DoctorFinding{
		Code: proto.DoctorUCXNetlinkHang, Level: proto.DoctorLevelWarning,
		Probe:    "agent: RTM_GETROUTE dump (AF_INET6) over a NETLINK_ROUTE socket",
		Evidence: evidence,
		Recheck:  "sysctl net.ipv6.conf.all.disable_ipv6 net.ipv6.conf.default.disable_ipv6",
	})
}
