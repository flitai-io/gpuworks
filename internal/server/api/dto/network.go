package dto

import "time"

// NetworkWarning flags one resolved network on one node. Code is rendered by
// the client locale; Params are its interpolation values.
type NetworkWarning struct {
	Code   string            `json:"code"`
	Params map[string]string `json:"params,omitempty"`
}

// NetworkAttachment binds a network to one node or to a node label.
type NetworkAttachment struct {
	ID         uint      `json:"id"`
	Target     string    `json:"target"` // node | label
	NodeUUID   string    `json:"node_uuid,omitempty"`
	NodeName   string    `json:"node_name,omitempty"`
	LabelKey   string    `json:"label_key,omitempty"`
	LabelValue string    `json:"label_value,omitempty"`
	Interface  string    `json:"interface,omitempty"`
	IP         string    `json:"ip,omitempty"`
	HCAs       []string  `json:"hcas"`
	CreatedAt  time.Time `json:"created_at"`
}

// Network is a declared node-to-node network with its attachments.
type Network struct {
	ID          uint     `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Roles       []string `json:"roles"`
	Type        string   `json:"type"`
	// CIDR and Interface select the address on Ethernet networks; an RDMA
	// network has only a CIDR, which picks the bootstrap address.
	CIDR      string `json:"cidr"`
	Interface string `json:"interface"`
	// Builtin marks the management network: it cannot be edited, deleted
	// or attached, only its roles switched off per node.
	Builtin           bool                `json:"builtin"`
	Attachments       []NetworkAttachment `json:"attachments"`
	ResolvedNodeCount int                 `json:"resolved_node_count"`
	WarningNodeCount  int                 `json:"warning_node_count"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

// NetworkList is the network page payload. Roles and Types are the server
// dictionary the client renders choices from.
type NetworkList struct {
	Items []Network `json:"items"`
	Roles []string  `json:"roles"`
	Types []string  `json:"types"`
	// TypeRoles lists, per type, the roles a network of that type may carry.
	TypeRoles map[string][]string `json:"type_roles"`
}

// NetworkNode is one node a network resolved to, with the roles it serves
// there and the host endpoints picked for it.
type NetworkNode struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name"`
	// Labels are the node's written labels, which the node table filters by.
	Labels       map[string]string `json:"labels"`
	Roles        []string          `json:"roles"`
	AttachmentID uint              `json:"attachment_id"`
	Source       string            `json:"source"` // node | label
	LabelKey     string            `json:"label_key,omitempty"`
	LabelValue   string            `json:"label_value,omitempty"`
	// Interface and IP are the node's address on the network: the bootstrap
	// address on an RDMA network.
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	// HCAs are an RDMA network's rails on the node, empty otherwise.
	HCAs      []NetworkRail `json:"hcas"`
	LinkLayer string        `json:"link_layer,omitempty"` // infiniband | roce, RDMA networks only
	// HCASource and BootstrapSource say whether the rails and the bootstrap
	// address are inferred ("auto") or pinned on the node ("manual").
	HCASource       string `json:"hca_source,omitempty"`
	BootstrapSource string `json:"bootstrap_source,omitempty"`
	// Tools are the network test tools the node can run (ib_write_bw).
	Tools    []string         `json:"tools"`
	Warnings []NetworkWarning `json:"warnings"`
	// RolesOff are the roles this node's management network may not carry;
	// management network only.
	RolesOff []string `json:"roles_off,omitempty"`
}

// NetworkRail is one RDMA port a node uses on an RDMA network, or can add.
type NetworkRail struct {
	Name      string  `json:"name"` // device:port
	IP        string  `json:"ip"`   // its interface's IPv4 address; "" for InfiniBand without IPoIB
	NetDev    string  `json:"netdev"`
	LinkLayer string  `json:"link_layer"`
	State     string  `json:"state"`
	SpeedGbps float64 `json:"speed_gbps"`
	// Gap says why the port cannot carry RDMA traffic: inactive, no_ipv4
	// (RoCE port without an IPv4 address) or unknown (not reported); ""
	// when it can.
	Gap string `json:"gap,omitempty"`
}

// NetworkTargetMatches identifies matching rows of a network detail table.
type NetworkTargetMatches struct {
	IDs []string `json:"ids"`
}

// NetworkDetail is one network with the nodes it resolved to.
type NetworkDetail struct {
	Network
	Nodes []NetworkNode `json:"nodes"`
	Roles []string      `json:"role_options"`
	Types []string      `json:"type_options"`
	// TypeRoles lists, per type, the roles a network of that type may carry.
	TypeRoles map[string][]string `json:"type_roles"`
	// TestSupported is false for serving-only networks.
	TestSupported bool            `json:"test_supported"`
	LatestTest    *NetworkTestRun `json:"latest_test"`
}

// NetworkTestEstimate is the size of a test request, computed without
// starting it. Card is set for a card test.
type NetworkTestEstimate struct {
	Kind      string                   `json:"kind"`
	Pattern   string                   `json:"pattern"`
	NodeCount int                      `json:"node_count"`
	PairCount int                      `json:"pair_count"`
	Card      *NetworkCardTestEstimate `json:"card,omitempty"`
}

// NetworkCardTestEstimate is how long a card test can take: a round lasts
// one probe's time limit at most, so Seconds is the rounds times that limit.
// A request whose Seconds would exceed MaxSeconds is refused instead.
type NetworkCardTestEstimate struct {
	Rounds     int `json:"rounds"`
	Probes     int `json:"probes"`
	Seconds    int `json:"seconds"`
	MaxSeconds int `json:"max_seconds"`
}

// NetworkRequest creates or replaces a network.
type NetworkRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Roles       []string `json:"roles"`
	Type        string   `json:"type"`
	CIDR        string   `json:"cidr"`
	Interface   string   `json:"interface"` // Ethernet networks only
}

// NetworkAttachmentRequest attaches a network to exactly one of a node or a
// label. Overrides are only accepted on node attachments.
type NetworkAttachmentRequest struct {
	NodeUUID   string   `json:"node_uuid"`
	LabelKey   string   `json:"label_key"`
	LabelValue string   `json:"label_value"`
	Interface  string   `json:"interface"`
	IP         string   `json:"ip"`
	HCAs       []string `json:"hcas"`
}

// NetworkLabelRequest changes a label attachment's matching selector.
type NetworkLabelRequest struct {
	LabelKey   string `json:"label_key"`
	LabelValue string `json:"label_value"`
}

// NetworkOverrideRequest replaces a node attachment's overrides.
type NetworkOverrideRequest struct {
	Interface string   `json:"interface"`
	IP        string   `json:"ip"`
	HCAs      []string `json:"hcas"`
}

// NetInterface mirrors one reported host interface.
type NetInterface struct {
	Name      string   `json:"name"`
	State     string   `json:"state"`
	MTU       int      `json:"mtu"`
	SpeedMbps int      `json:"speed_mbps"`
	MAC       string   `json:"mac"`
	Addrs     []string `json:"addrs"`
	// RxBPS / TxBPS are the latest reported rates (bytes/s), nil when none
	// arrived in the last two minutes.
	RxBPS *float64 `json:"rx_bytes_per_sec,omitempty"`
	TxBPS *float64 `json:"tx_bytes_per_sec,omitempty"`
}

// RDMAPort mirrors one reported RDMA port.
type RDMAPort struct {
	Device    string  `json:"device"`
	Port      int     `json:"port"`
	State     string  `json:"state"`
	LinkLayer string  `json:"link_layer"`
	RateGbps  float64 `json:"rate_gbps"`
	NetDev    string  `json:"netdev"`
	// IP and Gap are as on NetworkRail: the port's interface IPv4 address
	// and why it cannot carry RDMA traffic ("" when it can).
	IP  string `json:"ip"`
	Gap string `json:"gap,omitempty"`
	// RxBPS / TxBPS are the latest reported rates (bytes/s), nil when none
	// arrived in the last two minutes (the port is down, or an older agent).
	RxBPS *float64 `json:"rx_bytes_per_sec,omitempty"`
	TxBPS *float64 `json:"tx_bytes_per_sec,omitempty"`
}

// NodeLoadedNetwork is the network a node uses for one role.
type NodeLoadedNetwork struct {
	Role            string           `json:"role"`
	NetworkID       uint             `json:"network_id"`
	NetworkName     string           `json:"network_name"`
	NetworkType     string           `json:"network_type"`
	AttachmentID    uint             `json:"attachment_id"`
	Source          string           `json:"source"`
	LabelKey        string           `json:"label_key,omitempty"`
	LabelValue      string           `json:"label_value,omitempty"`
	Interface       string           `json:"interface"`
	IP              string           `json:"ip"`
	HCAs            []NetworkRail    `json:"hcas"`
	LinkLayer       string           `json:"link_layer,omitempty"`
	HCASource       string           `json:"hca_source,omitempty"`
	BootstrapSource string           `json:"bootstrap_source,omitempty"`
	Warnings        []NetworkWarning `json:"warnings"`
}

// AcceleratorPort mirrors one reported port of an accelerator card. IP and
// State describe its RoCE port; HCCSIP and HCCSState describe the A3/910C
// LingQu vNIC when the agent reports one.
type AcceleratorPort struct {
	Name        string  `json:"name"`
	Index       int     `json:"index"`
	IP          string  `json:"ip"`
	Netmask     string  `json:"netmask"`
	State       string  `json:"state"` // UP | DOWN | "" when unknown
	SpeedGbps   float64 `json:"speed_gbps"`
	HCCSIP      string  `json:"hccs_ip,omitempty"`
	HCCSNetmask string  `json:"hccs_netmask,omitempty"`
	HCCSState   string  `json:"hccs_state,omitempty"`
	// Gap is as on NetworkRail: why the card's port cannot carry traffic
	// ("" when it can).
	Gap string `json:"gap,omitempty"`
	// HCCSGap is the HCCS vNIC's gap when the agent reports an HCCS port.
	HCCSGap string `json:"hccs_gap,omitempty"`
}

// ServingForward is the gateway's direct path to a node over its serving
// network. Mode is "" until the gateway has sent a request there.
type ServingForward struct {
	Address string     `json:"address"`
	Mode    string     `json:"mode,omitempty"` // direct | tunnel
	Reason  string     `json:"reason,omitempty"`
	At      *time.Time `json:"at,omitempty"`
}

// NodeNetworks is a node's loaded networks and its last reported inventory.
// InventoryReported is false until an agent that reports inventory connects.
type NodeNetworks struct {
	Networks          []NodeLoadedNetwork `json:"networks"`
	ServingForward    *ServingForward     `json:"serving_forward"`
	Management        *NodeManagement     `json:"management"`
	InventoryReported bool                `json:"inventory_reported"`
	Interfaces        []NetInterface      `json:"interfaces"`
	RDMAPorts         []RDMAPort          `json:"rdma_ports"`
	AcceleratorPorts  []AcceleratorPort   `json:"accelerator_ports"`
}

// NodeManagement is a node's management network: the address its agent
// reaches the server from (empty until reported), and the roles switched off
// there.
type NodeManagement struct {
	Interface string   `json:"interface"`
	IP        string   `json:"ip"`
	RolesOff  []string `json:"roles_off"`
	// RemoteAddr is the address the server saw the connection come from;
	// BehindNAT is set when it differs from IP. Display only.
	RemoteAddr string `json:"remote_addr,omitempty"`
	BehindNAT  bool   `json:"behind_nat"`
}

// ManagementRolesRequest replaces the roles a node's management network may
// not carry.
type ManagementRolesRequest struct {
	RolesOff []string `json:"roles_off"`
}

// ManagementRolesBatchRequest lets the management network of every listed
// node carry the Allow roles and stops it carrying the Deny roles; roles in
// neither list keep each node's current setting.
type ManagementRolesBatchRequest struct {
	NodeUUIDs []string `json:"node_uuids"`
	Allow     []string `json:"allow"`
	Deny      []string `json:"deny"`
}

// ManagementRolesBatchResult is how many nodes actually changed.
type ManagementRolesBatchResult struct {
	Updated int `json:"updated"`
}

// NetworkTestPair is one directed probe of a network test.
type NetworkTestPair struct {
	SourceUUID string `json:"source_uuid"`
	SourceName string `json:"source_name"`
	TargetUUID string `json:"target_uuid"`
	TargetName string `json:"target_name"`
	Target     string `json:"target,omitempty"`
	// Status is passed, partial (some RDMA rails failed) or failed; Passed
	// is Status == passed.
	Status    string  `json:"status"`
	Passed    bool    `json:"passed"`
	Reachable bool    `json:"reachable"`
	RTTP50MS  float64 `json:"rtt_ms_p50"`
	RTTP99MS  float64 `json:"rtt_ms_p99"`
	Gbps      float64 `json:"gbps"`
	// Code names why the pair failed and Side which node it is about
	// (source | target); Error is the English text, or the detail of a
	// *_failed code. Runs stored before codes have only Error.
	Code  string `json:"code,omitempty"`
	Side  string `json:"side,omitempty"`
	Error string `json:"error,omitempty"`
	// AvgRailGbps is the mean of successful RDMA bandwidth readings.
	AvgRailGbps float64 `json:"avg_rail_gbps,omitempty"`
	// Rails are per-rail results of an rdma_tool test; Gbps is their sum.
	Rails []NetworkTestRail `json:"rails,omitempty"`
	// CardShape is how the two nodes' cards reach each other in a card run:
	// full | diagonal | mixed | none | untested.
	CardShape string `json:"card_shape,omitempty"`
}

// NetworkTestRail is one RDMA rail of an rdma_tool pair: the source rail,
// the target rail it drove, and the outcome. A rail with no peer on the
// other node has only one side.
type NetworkTestRail struct {
	Rail       string  `json:"rail"`
	SourceIP   string  `json:"source_ip,omitempty"`
	TargetRail string  `json:"target_rail,omitempty"`
	TargetIP   string  `json:"target_ip,omitempty"`
	Gbps       float64 `json:"gbps"`
	// Reachable and RTTMS are a card test's outcome: whether the target
	// card answered the source card's ping, and how fast.
	Reachable bool    `json:"reachable,omitempty"`
	RTTMS     float64 `json:"rtt_ms,omitempty"`
	Code      string  `json:"code,omitempty"`
	Side      string  `json:"side,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// NetworkTestRun is one internal connectivity test of a network.
type NetworkTestRun struct {
	ID          uint   `json:"id"`
	Status      string `json:"status"`  // running | done | interrupted
	Kind        string `json:"kind"`    // tcp | rdma_tool | card_ping | card_matrix
	Pattern     string `json:"pattern"` // mesh | ring
	Scope       string `json:"scope"`   // all | nodes | label
	LabelKey    string `json:"label_key,omitempty"`
	LabelValue  string `json:"label_value,omitempty"`
	Bandwidth   bool   `json:"bandwidth"`
	NodeCount   int    `json:"node_count"`
	PairCount   int    `json:"pair_count"`
	PassedCount int    `json:"passed_count"`
	// PartialCount pairs had some rails fail; RailCount and
	// RailPassedCount count the rails of an rdma_tool run, and of a card run
	// the card pairs that were pinged and that answered.
	PartialCount    int `json:"partial_count"`
	RailCount       int `json:"rail_count"`
	RailPassedCount int `json:"rail_passed_count"`
	// CardShape is what a finished card run found as a whole: the shape most
	// node pairs have (full | diagonal | mixed | none | untested). It is read
	// from the results of that run, which measured one moment.
	CardShape  string            `json:"card_shape,omitempty"`
	NodeUUIDs  []string          `json:"node_uuids"` // the nodes under test, fixed when the run starts
	Pairs      []NetworkTestPair `json:"pairs"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
}

// NetworkTestRequest starts a test. Scope "nodes" takes NodeUUIDs, "label"
// takes LabelKey/LabelValue, "all" tests every node the network resolved to.
type NetworkTestRequest struct {
	Kind       string   `json:"kind"`    // tcp (default) | rdma_tool | card_ping | card_matrix
	Pattern    string   `json:"pattern"` // mesh (default) | ring
	Scope      string   `json:"scope"`
	NodeUUIDs  []string `json:"node_uuids"`
	LabelKey   string   `json:"label_key"`
	LabelValue string   `json:"label_value"`
	Bandwidth  *bool    `json:"bandwidth"`
	Seconds    int      `json:"seconds"`
}

// NetworkAttachBatchRequest attaches a network to several nodes.
type NetworkAttachBatchRequest struct {
	NodeUUIDs []string `json:"node_uuids"`
}

// NetworkAttachBatchItem is one node of a batch attach: attached, already
// (it was attached before) or failed, with the product error code and its
// English text when it failed.
type NetworkAttachBatchItem struct {
	NodeUUID string         `json:"node_uuid"`
	Status   string         `json:"status"`
	Code     string         `json:"code,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// NetworkAttachBatchResult is every node's outcome and the network as it now
// resolves.
type NetworkAttachBatchResult struct {
	Results []NetworkAttachBatchItem `json:"results"`
	Network NetworkDetail            `json:"network"`
}

// NetworkHCARequest edits the rails nodes use on an RDMA network: add one
// port to one node, remove one port from every node using it (or from
// NodeUUID only), or reset one node to inferred rails.
type NetworkHCARequest struct {
	Op       string `json:"op"` // add | remove | reset
	HCA      string `json:"hca,omitempty"`
	NodeUUID string `json:"node_uuid,omitempty"`
}
