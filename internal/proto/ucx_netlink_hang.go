package proto

// UCXNetlinkHang is what a node's agent observes about the host kernel's
// IPv6 route dump (NodeStatus.UCXNetlinkHang). UCX 1.20-1.22, which NIXL
// bundles, dumps the IPv6 routes over netlink to decide whether a peer is
// reachable and only looks for NLMSG_DONE at the start of each datagram.
// Newer kernels put the last routes and NLMSG_DONE in one datagram, so UCX
// reads on forever: the engine that initialises NIXL (vLLM and SGLang PD)
// hangs, one thread spinning at 100% CPU. It is present only while the node
// is affected.
type UCXNetlinkHang struct {
	// Kernel is the host's kernel release, to tell which kernels do it.
	Kernel string `json:"kernel,omitempty"`
}

// The commands that end an UCXNetlinkHang: UCX only stops hanging when the
// host's IPv6 is off altogether (the routes it dumps are then none), so
// switching it off on single interfaces does not help. The first applies
// until the next reboot; the second keeps it. They are shared by the node
// page, the deploy refusal and the agent's own diagnosis.
const (
	UCXNetlinkFixNow     = "sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1"
	UCXNetlinkFixPersist = `printf 'net.ipv6.conf.all.disable_ipv6=1\nnet.ipv6.conf.default.disable_ipv6=1\n' | sudo tee /etc/sysctl.d/90-gpuworks-ucx.conf && sudo sysctl --system`
)
