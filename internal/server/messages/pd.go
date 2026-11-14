package messages

// PD disaggregation product error and warning codes (ADR 0028). Placement
// validation in package planning raises them; the api layer passes them on.
const (
	ErrorPDModeUnknown        = "PD_MODE_UNKNOWN"
	ErrorPDModeEngineMismatch = "PD_MODE_ENGINE_MISMATCH"
	ErrorPDRoleMissing        = "PD_ROLE_MISSING"
	ErrorPDRoleUnknown        = "PD_ROLE_UNKNOWN"
	ErrorPDRoleUnitsEmpty     = "PD_ROLE_UNITS_EMPTY"
	ErrorPDRoleUnitsTooMany   = "PD_ROLE_UNITS_TOO_MANY"
	ErrorPDTPInvalid          = "PD_TP_INVALID"
	ErrorPDUnitGPUCount       = "PD_UNIT_GPU_COUNT"
	// ErrorPDTPDirection, ErrorPDTPPrefillAtLeastDecode and ErrorPDTPEqual
	// are the three TP rules a mode may declare (pdmode.TPRule).
	ErrorPDTPDirection            = "PD_TP_DIRECTION"
	ErrorPDTPPrefillAtLeastDecode = "PD_TP_PREFILL_AT_LEAST_DECODE"
	ErrorPDTPEqual                = "PD_TP_EQUAL"
	ErrorPDGPUOverlap             = "PD_GPU_OVERLAP"
	// ErrorPDRankNodeRepeated: one multi-node instance places two ranks on
	// one node.
	ErrorPDRankNodeRepeated = "PD_RANK_NODE_REPEATED"
	// ErrorPDRankTPRequired: a multi-node instance whose role states no TP.
	ErrorPDRankTPRequired = "PD_RANK_TP_REQUIRED"
	// ErrorPDRankSplit: the role's TP×PP does not split equally over a
	// multi-node instance's ranks.
	ErrorPDRankSplit            = "PD_RANK_SPLIT"
	ErrorPDFallbackInvalid      = "PD_FALLBACK_INVALID"
	ErrorPDKVLoadFailureInvalid = "PD_KV_LOAD_FAILURE_INVALID"
	ErrorPDNodeUnknown          = "PD_NODE_UNKNOWN"
	// ErrorPDNodeRequired: an instance, or a rank of one, names no node yet;
	// PD_NODE_UNKNOWN is for a node that does not exist.
	ErrorPDNodeRequired         = "PD_NODE_REQUIRED"
	ErrorPDNodeOffline          = "PD_NODE_OFFLINE"
	ErrorPDNodeGPUMissing       = "PD_NODE_GPU_MISSING"
	ErrorPDNodeGPUsInsufficient = "PD_NODE_GPUS_INSUFFICIENT"
	ErrorPDVendorUnsupported    = "PD_VENDOR_UNSUPPORTED"
	ErrorPDVendorMixed          = "PD_VENDOR_MIXED"
	// ErrorPDSameNodeRequired: the mode's KV transport stays inside one host
	// and the placement spreads the instances over several nodes.
	ErrorPDSameNodeRequired = "PD_SAME_NODE_REQUIRED"
	// ErrorPDPeerMemoryMissing: a node the placement names cannot register its
	// cards' memory with the RDMA NIC (the vendor's peer-memory module is not
	// loaded), and the mode moves the KV over RDMA. ErrorPDPeerMemoryNone is
	// the same for automatic placement, when that left no node to choose.
	ErrorPDPeerMemoryMissing    = "PD_PEER_MEMORY_MISSING"
	ErrorPDPeerMemoryNone       = "PD_PEER_MEMORY_NONE"
	ErrorPDConnectorArgConflict = "PD_CONNECTOR_ARG_CONFLICT"
	ErrorPDShapeConflict        = "PD_SHAPE_CONFLICT"
	ErrorPDSpecModeMismatch     = "PD_SPEC_MODE_MISMATCH"
	// ErrorPDNetworkAcceleratorRequired and ErrorPDNetworkHostRequired: the
	// network the group shares cannot carry the mode's KV transport.
	ErrorPDNetworkAcceleratorRequired = "PD_NETWORK_ACCELERATOR_REQUIRED"
	ErrorPDNetworkHostRequired        = "PD_NETWORK_HOST_REQUIRED"
	// ErrorPDNetworkRDMARequired: the mode moves KV over RDMA (Mooncake), and
	// the group, even a single host, is not on a host RDMA compute network;
	// only such a network gives the instances the RDMA devices.
	ErrorPDNetworkRDMARequired = "PD_NETWORK_RDMA_REQUIRED"
	// ErrorPDSleepRDMAUnsupported: a sleepable PD deployment whose KV moves
	// over UCX on a host RDMA network. NIXL registers the KV cache with the
	// NIC, and an engine that slept cannot allocate it again on waking.
	ErrorPDSleepRDMAUnsupported = "PD_SLEEP_RDMA_UNSUPPORTED"
	// ErrorPDUCXNetlinkHang: a node of the placement has a kernel whose IPv6
	// route dump hangs UCX 1.20-1.22 (bundled with NIXL), and the mode moves
	// the KV with NIXL over Ethernet (ADR 0028). Params: nodes (uuid, name and
	// kernel of each), names, fixes (code and literal commands each), fix (the
	// first fix's command).
	ErrorPDUCXNetlinkHang = "PD_UCX_NETLINK_HANG"
	// ErrorPDNotPD: a PD-only operation on an ordinary deployment.
	ErrorPDNotPD = "PD_NOT_PD"
	// ErrorPDImmutable: a runtime change to what an instance was launched
	// with (mode, a role's TP, the KV load failure policy); clone instead.
	ErrorPDImmutable = "PD_IMMUTABLE"

	// WarningPDUnverified names a combination the platform allows but has not
	// verified on real hardware (plan D16); {feature} says which.
	WarningPDUnverified = "PD_UNVERIFIED_COMBINATION"
	// WarningPDGPUModelsDiffer: the roles run on different GPU models.
	WarningPDGPUModelsDiffer = "PD_GPU_MODELS_DIFFER"
	// WarningPDSameNodeThroughput: every instance is on one node, which
	// trades throughput for decode latency.
	WarningPDSameNodeThroughput = "PD_SAME_NODE_THROUGHPUT"
	// WarningPDKVOffloadPrefillOnly: the deployment's KV offload runs on the
	// prefill role only; decode never carries a KV cache tier.
	WarningPDKVOffloadPrefillOnly = "PD_KV_OFFLOAD_PREFILL_ONLY"
	// WarningPDUCXNetlinkHangRDMA: the same node fact as ErrorPDUCXNetlinkHang
	// on a host RDMA network, where UCX is held to RDMA transports and may not
	// run the route check; it is not refused, the node may still hang.
	WarningPDUCXNetlinkHangRDMA = "PD_UCX_NETLINK_HANG_RDMA"
)
