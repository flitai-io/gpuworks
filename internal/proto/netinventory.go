package proto

// NetInterface is one host network interface as the agent sees it. Addrs are
// CIDR strings ("10.20.1.15/16") so a selector can match either the address or
// the subnet it belongs to.
type NetInterface struct {
	Name      string   `json:"name"`
	State     string   `json:"state,omitempty"` // sysfs operstate: up | down | unknown ...
	MTU       int      `json:"mtu,omitempty"`
	SpeedMbps int      `json:"speed_mbps,omitempty"` // 0 when the driver does not report it
	MAC       string   `json:"mac,omitempty"`
	Addrs     []string `json:"addrs,omitempty"`
}

// RDMAPort is one port of an RDMA device (mlx5_0 port 1). LinkLayer tells an
// InfiniBand port from a RoCE one ("InfiniBand" | "Ethernet"); NetDev is the
// host interface bound to the same PCI function, empty for IB without IPoIB.
type RDMAPort struct {
	Device    string  `json:"device"`
	Port      int     `json:"port"`
	State     string  `json:"state,omitempty"` // ACTIVE | DOWN | INIT ...
	LinkLayer string  `json:"link_layer,omitempty"`
	RateGbps  float64 `json:"rate_gbps,omitempty"`
	NetDev    string  `json:"netdev,omitempty"`
}

// AcceleratorPort is one network port an accelerator card owns. Ascend cards
// put accelerator fabric ports on the card itself; the host kernel never sees
// them, so they are neither interfaces nor RDMA ports and are reached only
// through the vendor tool. IP/State describe RoCE; HCCSIP/HCCSState describe
// the HCCS vNIC when present. Name is the rail name internal/accelerator gives
// the card.
type AcceleratorPort struct {
	Name        string  `json:"name"`
	Index       int     `json:"index"` // the card index the vendor tool uses
	IP          string  `json:"ip,omitempty"`
	Netmask     string  `json:"netmask,omitempty"`
	State       string  `json:"state,omitempty"` // UP | DOWN (RoCE)
	SpeedGbps   float64 `json:"speed_gbps,omitempty"`
	HCCSIP      string  `json:"hccs_ip,omitempty"`
	HCCSNetmask string  `json:"hccs_netmask,omitempty"`
	HCCSState   string  `json:"hccs_state,omitempty"` // UP | DOWN (HCCS vNIC)
}

// NetInventory is the node's network inventory reported with NodeStatus.
type NetInventory struct {
	Interfaces []NetInterface `json:"interfaces"`
	RDMAPorts  []RDMAPort     `json:"rdma_ports"`
	// AcceleratorPorts are the ports the node's accelerator cards own; empty
	// on vendors whose cards have none and from an older agent.
	AcceleratorPorts []AcceleratorPort `json:"accelerator_ports,omitempty"`
	// EnginePortListeners lists the host TCP ports inside the server's engine
	// port range (NetworkConfig.EnginePorts) that some socket is listening on,
	// so placement avoids ports owned outside GPUWorks. It is a best-effort
	// snapshot, up to one status interval old.
	EnginePortListeners []int `json:"engine_port_listeners,omitempty"`
	// EnginePortRange is the node's own engine port range, set when its
	// agent was started with one (--engine-ports); nil means the server's.
	// A Kubernetes node needs it: the server's default range is the
	// cluster's NodePort range, whose ports kube-proxy redirects even on
	// loopback, so an engine there would be unreachable.
	EnginePortRange *PortRange `json:"engine_port_range,omitempty"`
	// Tools names the network test tools the node can run (ib_write_bw: the
	// host's, else the one embedded in the agent).
	Tools []string `json:"tools,omitempty"`
	// Management is the address this node reaches the server from: the
	// management network's address on this node. Nil from an older agent.
	Management *ManagementAddress `json:"management,omitempty"`
}

// ManagementAddress is a node's address on the management network and the
// interface that holds it (empty when no reported interface does).
type ManagementAddress struct {
	Interface string `json:"interface,omitempty"`
	IP        string `json:"ip"`
}

// RDMABenchParams starts servers (rdma.bench.serve) or clients
// (rdma.bench.run, with Target) on Rails, rail i using port BasePort+i.
type RDMABenchParams struct {
	Rails    []string `json:"rails"`
	BasePort int      `json:"base_port"`
	Seconds  int      `json:"seconds"`
	Target   string   `json:"target,omitempty"`
}

// RDMABenchRail is one rail's result of rdma.bench.run, or a rail
// rdma.bench.serve could not serve. Code, when set, is one of the
// RDMABenchCode* values; Error is the English detail (the tool's own message
// for RDMABenchToolFailed).
type RDMABenchRail struct {
	Rail  string  `json:"rail"`
	Gbps  float64 `json:"gbps"`
	Code  string  `json:"code,omitempty"`
	Error string  `json:"error,omitempty"`
}

// RDMABenchServed answers rdma.bench.serve: the rails it could not serve.
// Older agents answer {"serving": true} and report failures as an RPC error.
type RDMABenchServed struct {
	Failed []RDMABenchRail `json:"failed,omitempty"`
}

// Why an RDMA bench rail failed.
const (
	// RDMABenchToolMissing: neither the host's nor the embedded ib_write_bw
	// can run on the node.
	RDMABenchToolMissing = "tool_missing"
	// RDMABenchNoRoCEv2GID: a RoCE port has no IPv4 RoCE v2 GID, which it
	// only gets when its interface holds an IPv4 address.
	RDMABenchNoRoCEv2GID = "no_roce_v2_gid"
	// RDMABenchToolFailed: ib_write_bw ran and failed; Error is its message.
	RDMABenchToolFailed = "tool_failed"
)

// AcceleratorPingParams asks a node to ping addresses from the ports of its
// accelerator cards (accelerator.ping). Each probe names the card it leaves
// from; probes on different cards run side by side, probes on one card in
// turn. TimeoutMS bounds one probe: an address that does not answer never
// ends the vendor tool by itself.
type AcceleratorPingParams struct {
	Probes    []AcceleratorPingProbe `json:"probes"`
	TimeoutMS int                    `json:"timeout_ms"`
	// Protocol selects the accelerator fabric command. Empty and "roce"
	// preserve the original hccn_tool -ping behavior; "hccs" uses
	// hccn_tool -hccs_ping.
	Protocol string `json:"protocol,omitempty"`
}

// AcceleratorPingProbe is one ping: from the port of Card (an
// AcceleratorPort name, "npu3") to Target, an IPv4 address.
type AcceleratorPingProbe struct {
	Card   string `json:"card"`
	Target string `json:"target"`
}

// AcceleratorPingResult answers one probe, in request order. Code, when set,
// is one of the AcceleratorPing* values and Error its English detail.
type AcceleratorPingResult struct {
	Card      string  `json:"card"`
	Target    string  `json:"target"`
	Reachable bool    `json:"reachable"`
	RTTMS     float64 `json:"rtt_ms,omitempty"`
	Code      string  `json:"code,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Why an accelerator ping did not get an answer.
const (
	// AcceleratorPingProtocolRoCE selects the accelerator card's RoCE port.
	AcceleratorPingProtocolRoCE = "roce"
	// AcceleratorPingProtocolHCCS selects the Ascend HCCS/HIXL fabric.
	AcceleratorPingProtocolHCCS = "hccs"

	// AcceleratorPingUnreachable: nothing answered within the time limit.
	AcceleratorPingUnreachable = "unreachable"
	// AcceleratorPingNoCard: the node has no such card, or its accelerators
	// have no ports of their own.
	AcceleratorPingNoCard = "no_card"
	// AcceleratorPingToolFailed: the vendor tool is missing or answered with
	// an error; Error is its message.
	AcceleratorPingToolFailed = "tool_failed"
)
