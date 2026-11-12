// Package pdmode is the catalog of PD (prefill/decode) disaggregation modes:
// the one place that says which roles a mode has, how its KV connector names
// them, how the gateway hands a request from one role to the other, and which
// pairing rules the two roles must satisfy (ADR 0028). Placement validation,
// launch assembly, the controller and the gateway all read these declarations
// instead of writing the rules down again.
//
// It is a leaf package: it imports only other leaves (engine and vendor
// names, product error codes) and does no I/O.
package pdmode

import (
	"fmt"
	"sort"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Role names. A role is a string on the instance row, so a later mode may add
// an auxiliary role without a schema change; these are the two every mode so
// far has.
const (
	RolePrefill = "prefill"
	RoleDecode  = "decode"
)

// MaxUnitsPerRole bounds the instances of one role, matching the replica
// ceiling of an ordinary deployment.
const MaxUnitsPerRole = 32

// Fallback is what the gateway does when no prefill instance can take a
// request. Which ones a mode allows, and its default, is Mode.Fallbacks.
const (
	// FallbackDecode sends the request to a decode instance, which then
	// computes the prefill itself. It is the default where the decoder can.
	FallbackDecode = "decode"
	// FallbackFail answers with a retryable 503 instead.
	FallbackFail = "fail"
)

// KV load failure policies: what a decode engine does when pulling the KV
// from the prefill instance fails after the handoff. Which ones a mode's
// engine takes, and its default, is Mode.KVLoadFailures.
const (
	KVLoadFail      = "fail"
	KVLoadRecompute = "recompute"
)

// Wire is how the gateway hands a request from prefill to decode.
type Wire string

const (
	// WireSequential sends the request to the prefill instance first, with
	// one output token and a marker asking it to keep the KV for a remote
	// decoder, then forwards the kv_transfer_params it returns to the decode
	// instance, which pulls the KV itself.
	WireSequential Wire = "sequential"
	// WireConcurrent sends the same request to both instances at once, both
	// carrying a bootstrap address (the prefill instance's bootstrap server)
	// and a room number that pairs them; the decode instance's answer is the
	// client's (SGLang).
	WireConcurrent Wire = "concurrent"
)

// TPRule constrains the tensor parallel degrees of the two roles.
type TPRule string

const (
	// TPDecodeAtLeastPrefill requires decode TP >= prefill TP (NIXL).
	TPDecodeAtLeastPrefill TPRule = "decode_at_least_prefill"
	// TPPrefillAtLeastDecode requires prefill TP >= decode TP (vllm-ascend's
	// Mooncake connector gathers one decode rank's KV from several prefill
	// ranks, the reverse of NIXL).
	TPPrefillAtLeastDecode TPRule = "prefill_at_least_decode"
	// TPEqual requires both roles to use the same TP.
	TPEqual TPRule = "equal"
)

// SideChannel is how an instance tells its peers where it is.
type SideChannel string

const (
	// SideChannelNIXL sets vLLM's NIXL side channel host and port.
	SideChannelNIXL SideChannel = "nixl"
	// SideChannelHostIP makes the connector read the node address from the
	// mode's AddressEnv (vLLM's get_ip honours VLLM_HOST_IP).
	SideChannelHostIP SideChannel = "host_ip"
)

// Transport is what carries the KV between the roles.
type Transport string

const (
	// TransportUCX is UCX under NIXL: the host network, so the UCX device
	// rules apply (network device, RDMA transports, bridge switch).
	TransportUCX Transport = "ucx"
	// TransportHCCL is the NPUs' own RoCE ports: across hosts it needs an
	// accelerator RDMA network, and the host network only for the handshake.
	TransportHCCL Transport = "hccl"
	// TransportIPC is a card-to-card copy inside one host (Mooncake's NVLink
	// transport with legacy IPC handles): no network carries the KV, so both
	// roles must run on the same node (Mode.SameNode).
	TransportIPC Transport = "ipc"
	// TransportMooncakeRDMA is Mooncake's RDMA transport between the hosts'
	// RDMA NICs: the group shares a host RDMA network, and the NIC registers
	// the cards' memory, which needs the vendor's peer-memory kernel module
	// on every node (Mode.PeerMemory).
	TransportMooncakeRDMA Transport = "mooncake_rdma"
	// TransportMooncake is the Mooncake transfer engine on whatever host
	// network the group shares: RDMA on a host RDMA network, TCP on any other,
	// which the launch chooses (mooncake_protocol).
	TransportMooncake Transport = "mooncake"
)

// OnHostNetwork reports whether the transport drives host network devices
// (UCX, Mooncake), so the KV cannot ride an accelerator network.
func (t Transport) OnHostNetwork() bool {
	return t == TransportUCX || t == TransportMooncake || t == TransportMooncakeRDMA
}

// MooncakeBootstrap reports whether the transport is Mooncake's, whose vLLM
// connector serves the prefill's handshake from a bootstrap server.
func (t Transport) MooncakeBootstrap() bool {
	return t == TransportMooncake || t == TransportMooncakeRDMA
}

// Mode is one PD recipe.
type Mode struct {
	Name   string
	Engine string
	// Vendors are the accelerator vendors the mode is declared for.
	Vendors []string
	// SameVendor is a pairing rule between the two roles.
	SameVendor bool
	// Roles lists the roles in the order the gateway picks them: decode first
	// so a later conditional disaggregation can look at the decoder's cache.
	Roles []string
	// Connector is the engine's KV connector class and ConnectorRoles maps a
	// role to the connector's own role value.
	Connector      string
	ConnectorRoles map[string]string
	Wire           Wire
	TP             TPRule
	// TwoStagePaths are the request paths the gateway disaggregates; every
	// other path goes to a decode instance directly.
	TwoStagePaths []string
	// TransferMetricsRole is the role whose KV transfer counters are
	// meaningful: NIXL is pulled by the decoder, so the prefill side stays 0.
	TransferMetricsRole string
	// TransferBytesMetric is the Prometheus family on that role that counts
	// transferred bytes (a histogram's _sum); empty when the engine exposes
	// none, and a handoff is then confirmed by its answer alone.
	TransferBytesMetric string
	// VerifiedVersions is the engine version range the mode was verified on.
	// Launching outside it is allowed and only warned about.
	VerifiedVersions string
	// ScalableToZero lists the roles a running deployment may shrink to no
	// instances: without prefill instances every request is decoded alone,
	// which the decoder can do (plan D14). A deployment is still created
	// with every role.
	ScalableToZero []string
	// DecodeServesAlone reports whether a decode instance can answer a
	// request with no prefill instance involved (it then computes the
	// prefill itself). Without it there is no fallback: a request no prefill
	// instance can take is answered with a retryable 503.
	DecodeServesAlone bool
	// KVLoadFailures are the KV load failure policies the engine takes, the
	// default first; none when the engine has no such setting.
	KVLoadFailures []string
	// ExtraPortsPerTP and ExtraPorts size each role's block of ports beyond
	// the API port: per TP rank (NIXL side channels) or fixed (a bootstrap
	// server).
	ExtraPortsPerTP map[string]int
	ExtraPortsFixed map[string]int
	// Unverified marks a mode no release has been verified on real hardware;
	// evaluation warns about it.
	Unverified bool

	// SideChannel is how an instance tells peers where it is.
	SideChannel SideChannel
	// AddressEnv are variables set to the instance's node address and
	// InterfaceEnv to the network interface holding it (HCCL handshakes).
	AddressEnv   []string
	InterfaceEnv []string
	// Env is fixed environment every instance of the mode needs; a template
	// or the deployment may still set another value.
	Env map[string]string
	// Transport is what carries the KV.
	Transport Transport
	// ConnectorPortKey names the connector config field that takes the base
	// of the instance's extra port band (vllm-ascend's kv_port), and
	// ConnectorParallelism puts both roles' tp/dp into the connector config.
	ConnectorPortKey     string
	ConnectorParallelism bool
	// SameNode requires every instance of the deployment on one node: the
	// transport cannot reach another host.
	SameNode bool
	// EngineArgs are engine flags every instance of the mode needs; a flag a
	// template or the deployment already states keeps its value.
	EngineArgs []string
	// PeerMemory names the kernel module that lets the RDMA NIC register the
	// cards' memory (accelerator.PeerMemoryModule); a node without it loaded
	// cannot run the mode, and placement says so.
	PeerMemory bool
	// ConnectorExtra is fixed kv_connector_extra_config for the mode's
	// connector (vLLM), merged under what the launch composes.
	ConnectorExtra map[string]any
	// SynthesizedHandoff is a sequential mode whose prefill answer carries no
	// kv_transfer_params (vLLM's MooncakeConnector): the gateway gives the
	// prefill copy a transfer id and composes the decoder's params itself,
	// naming the prefill instance by its bootstrap server and the engine id
	// its workers registered there. The engine id is read from that server,
	// not pinned at launch: the decoder caches a prefill's worker addresses
	// by engine id, so a prefill restarted on the same port under the same id
	// would be pulled from at its dead workers' addresses (measured: every
	// handoff then failed after 540 s).
	SynthesizedHandoff bool
	// FirstRequestCompiles: the engine compiles kernels for each new request
	// shape, so the first request after a start takes minutes (measured on
	// PPU SGLang: 188–198 s, the rest under a second). The deploy form says
	// so; the gateway waits as for any slow answer.
	FirstRequestCompiles bool
	// HybridKVCacheManager: a model with recurrent layers (gated delta net,
	// Mamba; modelconfig.Config.HasRecurrentLayers) runs under the mode only
	// with vLLM's hybrid KV cache manager on, which vLLM turns off whenever a
	// KV connector is configured unless told otherwise. Without it the
	// layers' KV specs cannot be unified and the engine fails to start.
	HybridKVCacheManager bool
}

// ascendCards is what HCCL needs to be told on an Ascend host; the mode that
// hands KV to HCCL sets the same variables an ordinary multi-node rank gets.
var ascendCards, _ = accelerator.CardNetworkOf(accelerator.VendorAscend)

var modes = map[string]Mode{
	"vllm-nixl": {
		Name:   "vllm-nixl",
		Engine: engines.VLLM,
		// AMD: nixl-rocm over UCX TCP, validated across two MI300X hosts.
		Vendors:             []string{accelerator.VendorNVIDIA, accelerator.VendorAMD},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "NixlConnector",
		ConnectorRoles:      map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                WireSequential,
		TP:                  TPDecodeAtLeastPrefill,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole: RoleDecode,
		TransferBytesMetric: "vllm:nixl_bytes_transferred",
		VerifiedVersions:    "0.17.1-0.29.1",
		ScalableToZero:      []string{RolePrefill},
		DecodeServesAlone:   true,
		KVLoadFailures:      []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsPerTP:     map[string]int{RolePrefill: 1, RoleDecode: 1},
		SideChannel:         SideChannelNIXL,
		// NixlConnector transfers a Mamba-style layer's conv state (gated
		// delta net, KDA, short conv) only in the dim-first layout and refuses
		// to start otherwise; vLLM's default is state-first. The variable means
		// nothing to a model without such layers, which is why vLLM's own CPU
		// platform sets it whenever a NIXL connector is configured.
		Env:       map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "DS"},
		Transport: TransportUCX,
	},
	// vllm-ascend with its MooncakeConnectorV1: the same kv_transfer_params
	// wire as NIXL, KV over the NPUs' RoCE ports (HCCL), handshakes on the
	// node's control network. MooncakeConnectorV1 rather than
	// MooncakeHybridConnector: when a transfer fails V1 answers 500, while
	// the hybrid connector was measured to answer 200 with garbage.
	// Verified across two 910B4 hosts (vllm-ascend 0.26.0rc1, driver
	// 26.0.rc1): the prefill and decode NPUs must reach each other on the NPU
	// network; the lab's hosts are cabled NPU to NPU, card N to card N, so
	// only same-index pairs across the two hosts can. The earlier failure
	// (HcclCommPrepare) was a same-host pair, whose ports are not linked.
	"vllm-ascend-mooncake": {
		Name:                 "vllm-ascend-mooncake",
		Engine:               engines.VLLM,
		Vendors:              []string{accelerator.VendorAscend},
		SameVendor:           true,
		Roles:                []string{RoleDecode, RolePrefill},
		Connector:            "MooncakeConnectorV1",
		ConnectorRoles:       map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                 WireSequential,
		TP:                   TPPrefillAtLeastDecode,
		TwoStagePaths:        []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole:  RoleDecode,
		ScalableToZero:       []string{RolePrefill},
		DecodeServesAlone:    true,
		KVLoadFailures:       []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsPerTP:      map[string]int{RolePrefill: 1, RoleDecode: 1},
		SideChannel:          SideChannelHostIP,
		AddressEnv:           append([]string{"VLLM_HOST_IP"}, ascendCards.AddressEnv...),
		InterfaceEnv:         append(append([]string(nil), ascendCards.InterfaceEnv...), "GLOO_SOCKET_IFNAME", "TP_SOCKET_IFNAME"),
		Transport:            TransportHCCL,
		ConnectorPortKey:     "kv_port",
		ConnectorParallelism: true,
	},
	// SGLang with the NIXL transfer backend: UCX carries the KV, so the
	// network rules are vllm-nixl's. The decoder cannot compute a prefill in
	// disaggregation mode, so there is no fallback and no scaling prefill to
	// zero. Both roles keep one TP until heterogeneous TP is verified.
	"sglang-nixl": {
		Name:                "sglang-nixl",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorNVIDIA},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "nixl",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPEqual,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		Unverified:          true,
		Transport:           TransportUCX,
	},
	// SGLang on T-Head PPU with Mooncake's RDMA transport: the KV goes card
	// to NIC to card, across hosts on a host RDMA network or between two cards
	// of one host. The NIC registers the cards' memory through the PPU
	// driver's peer-memory module (alixpu_peermem), so every node needs it
	// loaded. None of the IPC mode's patches is involved. Verified on PPU
	// ZW810E (RoCE, mlx5) with TP 1 on both roles.
	"sglang-ppu-mooncake": {
		Name:                "sglang-ppu-mooncake",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "mooncake",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPEqual,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.5.13",
		// First requests of a shape compile PPU kernels for minutes; a
		// generating health check would time out and retire the instance.
		Env:                  map[string]string{"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0"},
		FirstRequestCompiles: true,
		PeerMemory:           true,
		Transport:            TransportMooncakeRDMA,
	},
	// vLLM's MooncakeConnector on T-Head PPU over RDMA, as sglang-ppu-mooncake.
	// The prefill engine answers its one-token copy without kv_transfer_params;
	// the decoder pulls by the prefill engine's id from its bootstrap server
	// (VLLM_MOONCAKE_BOOTSTRAP_PORT), so the gateway composes the handoff.
	// Verified on PPU ZW810E with vLLM 0.23.0 and TP 1 on both roles.
	"vllm-ppu-mooncake": {
		Name:                "vllm-ppu-mooncake",
		Engine:              engines.VLLM,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "MooncakeConnector",
		ConnectorRoles:      map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                WireSequential,
		TP:                  TPEqual,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.23.0",
		ConnectorExtra:      map[string]any{"mooncake_protocol": "rdma"},
		SynthesizedHandoff:  true,
		PeerMemory:          true,
		Transport:           TransportMooncakeRDMA,
	},
	// vLLM's MooncakeConnector on Hygon DCU: the Hygon vLLM 0.21 image runs it
	// on a Mooncake transfer engine built for DTK (TCP, RDMA and an intra-host
	// HIP transport), patched by vllm-hcu, so the KV moves over whatever host
	// network the group shares. A decoder given no handoff computes the
	// prefill itself. The vllm-hcu patch is about heterogeneous TP/PP; only
	// equal TP was tried. Verified on K100, two cards in one host at TP 1. It
	// needs the image's r2 or later: r1's connector reported a failed pull as
	// finished, so the decoder answered from KV it never received and the
	// load failure policy did not apply. Hybrid models with gated delta net
	// layers (Qwen3.5/3.6/3.8) need r3, whose connector moves their KV pages
	// whole at equal TP and whose scheduler survives their failed pulls, and
	// the hybrid KV cache manager (verified on K100 with Qwen3.5-0.8B and
	// hygon/Qwen3.8-27B-Channel-INT8-w8a8).
	"vllm-mooncake": {
		Name:                 "vllm-mooncake",
		Engine:               engines.VLLM,
		Vendors:              []string{accelerator.VendorHygon},
		SameVendor:           true,
		Roles:                []string{RoleDecode, RolePrefill},
		Connector:            "MooncakeConnector",
		ConnectorRoles:       map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                 WireSequential,
		TP:                   TPEqual,
		TwoStagePaths:        []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole:  RoleDecode,
		VerifiedVersions:     "0.21.0",
		ScalableToZero:       []string{RolePrefill},
		DecodeServesAlone:    true,
		KVLoadFailures:       []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsFixed:      map[string]int{RolePrefill: 1},
		SynthesizedHandoff:   true,
		Transport:            TransportMooncake,
		HybridKVCacheManager: true,
	},
	// SGLang on T-Head PPU with the mooncake backend: Mooncake's NVLink
	// transport over legacy IPC handles copies the KV card to card inside one
	// host, so the two roles share a node. The PPU SGLang runner carries the
	// patches the variables switch on (build/runner-images/hggc/ppu-pd):
	// registering the KV by allocation (an IPC handle names a whole
	// allocation), metadata on the card, and serialized transfers - an IPC
	// copy issued while a forward pass runs can deadlock the PPU driver, which
	// is also why the overlap scheduler is off. The health endpoint answers
	// without a generation: the first requests of a new shape compile PPU
	// kernels for minutes, a generating health check times out meanwhile and
	// the agent would retire a healthy instance, and every such check would
	// queue a fake transfer on the prefill side. Verified on PPU ZW810E with
	// TP 1 on both roles.
	"sglang-ppu-ipc": {
		Name:                "sglang-ppu-ipc",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		SameNode:            true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "mooncake",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPEqual,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.5.13",
		Env: map[string]string{
			"SGLANG_PPU_PD_IPC":                        "1",
			"SGLANG_PPU_PD_SERIAL_TRANSFER":            "1",
			"MC_FORCE_MNNVL":                           "1",
			"MC_USE_NVLINK_IPC":                        "1",
			"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0",
		},
		EngineArgs:           []string{"--disable-overlap-schedule"},
		FirstRequestCompiles: true,
		Transport:            TransportIPC,
	},
}

// Lookup returns the mode with this name.
func Lookup(name string) (Mode, bool) {
	mode, ok := modes[name]
	return mode, ok
}

// Names lists every mode, sorted.
func Names() []string {
	out := make([]string, 0, len(modes))
	for name := range modes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ForEngine lists the modes an engine supports, sorted.
func ForEngine(engine string) []string {
	var out []string
	for _, name := range Names() {
		if modes[name].Engine == engine {
			out = append(out, name)
		}
	}
	return out
}

// HasRole reports whether the mode declares this role.
func (m Mode) HasRole(role string) bool {
	return contains(m.Roles, role)
}

// MayBeEmpty reports whether a running deployment may have no instance of
// this role.
func (m Mode) MayBeEmpty(role string) bool {
	return contains(m.ScalableToZero, role)
}

// SupportsVendor reports whether the mode is declared for this vendor.
func (m Mode) SupportsVendor(vendor string) bool {
	return contains(m.Vendors, vendor)
}

// CheckTP validates the two roles' tensor parallel degrees against the mode's
// direction rule; the error is a product error naming the rule.
func (m Mode) CheckTP(prefill, decode int) error {
	params := map[string]any{"mode": m.Name, "prefill": prefill, "decode": decode}
	switch m.TP {
	case TPDecodeAtLeastPrefill:
		if decode < prefill {
			return messages.NewProductError(messages.ErrorPDTPDirection, fmt.Sprintf("%s needs decode TP >= prefill TP, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	case TPPrefillAtLeastDecode:
		if prefill < decode {
			return messages.NewProductError(messages.ErrorPDTPPrefillAtLeastDecode, fmt.Sprintf("%s needs prefill TP >= decode TP, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	case TPEqual:
		if decode != prefill {
			return messages.NewProductError(messages.ErrorPDTPEqual, fmt.Sprintf("%s needs the same TP on both roles, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	}
	return nil
}

// ExtraPorts is how many consecutive ports beyond the API port one instance
// of a role with this TP listens on: NIXL offsets a worker's side channel by
// its rank, so a TP group needs TP of them; a bootstrap server needs one.
func (m Mode) ExtraPorts(role string, tp int) int {
	if tp < 1 {
		tp = 1
	}
	return m.ExtraPortsPerTP[role]*tp + m.ExtraPortsFixed[role]
}

// TwoStage reports whether a request path is disaggregated.
func (m Mode) TwoStage(path string) bool {
	return contains(m.TwoStagePaths, path)
}

// ConnectorRole is the connector's role value for a PD role.
func (m Mode) ConnectorRole(role string) string {
	return m.ConnectorRoles[role]
}

// Fallbacks lists the gateway fallbacks the mode allows, its default first:
// a mode whose decoder serves alone may fall back to it, any mode may fail.
func (m Mode) Fallbacks() []string {
	if m.DecodeServesAlone {
		return []string{FallbackDecode, FallbackFail}
	}
	return []string{FallbackFail}
}

// EffectiveFallback is the fallback a placement's value means: empty is the
// mode's default.
func (m Mode) EffectiveFallback(value string) string {
	if value == "" {
		return m.Fallbacks()[0]
	}
	return value
}

// ValidFallback reports whether the mode allows a fallback value; empty
// means the default.
func (m Mode) ValidFallback(value string) bool {
	return value == "" || contains(m.Fallbacks(), value)
}

// EffectiveKVLoadFailure is the policy a placement's value means: empty is
// the mode's default, and a mode whose engine has no such setting has none.
func (m Mode) EffectiveKVLoadFailure(value string) string {
	if value == "" && len(m.KVLoadFailures) > 0 {
		return m.KVLoadFailures[0]
	}
	return value
}

// ValidKVLoadFailure reports whether the mode's engine takes a policy value;
// empty means the default.
func (m Mode) ValidKVLoadFailure(value string) bool {
	return value == "" || contains(m.KVLoadFailures, value)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
