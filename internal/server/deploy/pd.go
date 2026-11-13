// pd.go holds the launch facts a PD disaggregated instance adds to an
// ordinary launch (ADR 0028): which role it plays, where its KV side channel
// listens, and which network device the KV transfer uses. The rules behind
// them (connector class, the connector's role names) come from package pdmode.

package deploy

import (
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// PDLaunch is one PD instance's role-specific launch input.
type PDLaunch struct {
	Mode string
	Role string
	// KVLoadFailure is the engine policy when a pull fails (pdmode.KVLoad*).
	KVLoadFailure string
	// SideChannelHost is the address the KV side channel listens on: the
	// node's address on the network the PD group shares. The engine default
	// is localhost, which no other node can reach.
	SideChannelHost string
	// SideChannelPort is the first port of the instance's extra port block
	// (NIXL side channels, a bootstrap server, a Mooncake kv_port band).
	SideChannelPort int
	// RoleTP is every role's tensor parallel degree, for connectors whose
	// config names both sides.
	RoleTP map[string]int
	// RecurrentLayers: the model has recurrent layers (gated delta net,
	// Mamba), so a mode with pdmode.Mode.HybridKVCacheManager needs vLLM's
	// hybrid KV cache manager on.
	RecurrentLayers bool
}

// mode returns the declared mode; an unknown name yields the zero Mode, which
// injects nothing (placement validation rejects it long before a launch).
func (p *PDLaunch) mode() pdmode.Mode {
	if p == nil {
		return pdmode.Mode{}
	}
	mode, _ := pdmode.Lookup(p.Mode)
	return mode
}

// PDRoleLaunchConfig is the launch configuration a role runs with. KV offload
// belongs to the prefill role only: on the decoder the PD connector always
// supplies the prefix first, so a cache tier there only writes (measured: 14
// GB written, 0 bytes read back) and was reported to crash on first hit. The
// deployment keeps one configuration; this is where the decode role drops the
// tiers, for the argv and the disk mount alike.
func PDRoleLaunchConfig(q LaunchConfig, role string) LaunchConfig {
	if role != pdmode.RoleDecode {
		return q
	}
	q.KVOffloadingSize = 0
	q.KVDiskOffload = false
	q.SharedKVDir = ""
	q.KVDiskLimitGiB = 0
	q.KVOffloadReadThreads, q.KVOffloadWriteThreads, q.KVOffloadBlockSize = 0, 0, 0
	return q
}

// kvTransferNetworkEnv selects the network device the KV transfer (UCX under
// NIXL) uses on a PD instance. RDMA networks get one HCA and are held to RDMA
// and CUDA transports, so a container that cannot use verbs fails at startup
// instead of crawling over TCP. Ethernet networks name the interface: with
// host networking UCX sees every host device and would otherwise pick bridges
// such as docker0. UCX filters bridge devices by default, and a named bridge
// then leaves NIXL without a backend (NIXL_ERR_BACKEND, measured on a host
// whose only address is on a bridge), so naming a device also lifts that
// filter.
func kvTransferNetworkEnv(p Params) map[string]string {
	if p.PD == nil || p.PD.mode().Transport != pdmode.TransportUCX {
		return nil
	}
	n := p.ComputeNetwork
	env := map[string]string{}
	switch {
	case n.HostRDMA() && len(n.HCAs) > 0:
		env["UCX_NET_DEVICES"] = n.HCAs[0]
		// The transport list names CUDA's copy transports; on ROCm UCX picks
		// its own (rocm_copy, rocm_ipc) once the device is named.
		if accelerator.NormalizeVendor(p.Vendor) != accelerator.VendorAMD {
			env["UCX_TLS"] = "rc,cuda_copy,cuda_ipc,gdr_copy"
		}
	case n.Interface != "":
		env["UCX_NET_DEVICES"] = n.Interface
		env["UCX_TCP_BRIDGE_ENABLE"] = "y"
	}
	return env
}

// pdNoProxy keeps a runtime-injected proxy off a PD instance's own HTTP
// handshakes (SGLang's bootstrap server, vLLM Mooncake's): the engine
// registers with them at 0.0.0.0 and peers query them at the instance's PD
// address. A container runtime that injects a proxy (Docker's client proxies,
// for one) would carry those calls to the proxy, which answers 502 for a host
// it cannot reach: measured on a PPU host whose no_proxy did not list the node
// address. Stating the variables keeps the runtime from injecting its own; the
// engine reads its weights offline, so nothing else it does needs a proxy.
func pdNoProxy(p Params, env map[string]string) {
	noProxy := "localhost,127.0.0.1,::1,0.0.0.0"
	if host := p.PD.SideChannelHost; host != "" {
		noProxy += "," + host
	}
	env["NO_PROXY"], env["no_proxy"] = noProxy, noProxy
}

// hcaDevices are the device names of a network's HCAs ("mlx5_0:1" names
// device mlx5_0, port 1), each once, in order.
func hcaDevices(hcas []string) []string {
	var out []string
	for _, hca := range hcas {
		dev, _, _ := strings.Cut(hca, ":")
		if dev != "" && !slices.Contains(out, dev) {
			out = append(out, dev)
		}
	}
	return out
}

// MooncakeProtocol is the protocol a TransportMooncake instance hands the
// Mooncake transfer engine: RDMA on a host RDMA compute network, TCP on any
// other (the management network, an Ethernet network, or none on one host).
func MooncakeProtocol(n ComputeNetwork) string {
	if n.HostRDMA() {
		return "rdma"
	}
	return "tcp"
}

// vllmHybridKVCacheManager turns vLLM's hybrid KV cache manager on for a PD
// instance of a model with recurrent layers under a mode that needs it: vLLM
// turns the manager off whenever a KV connector is configured, and such a
// model's KV cache specs cannot be unified without it. A deployment that states
// either polarity keeps its choice.
func vllmHybridKVCacheManager(b *builder, p Params) {
	if p.PD == nil || !p.PD.RecurrentLayers || !p.PD.mode().HybridKVCacheManager {
		return
	}
	if b.blocked["--disable-hybrid-kv-cache-manager"] {
		return
	}
	b.injectFlag("--no-disable-hybrid-kv-cache-manager")
}

// vllmPDEnv is the vLLM side channel environment of a PD instance, as the
// mode declares it.
func vllmPDEnv(p Params, env map[string]string) {
	if p.PD == nil {
		return
	}
	if p.PD.mode().Transport == pdmode.TransportHCCL && p.ComputeNetwork.AcceleratorHCCS() {
		env["ASCEND_GLOBAL_RESOURCE_CONFIG"] = `{"comm_resource_config.protocol_desc":["hccs:device"]}`
		env["ACL_OP_INIT_MODE"] = "1"
		env["ASCEND_ENABLE_USE_FABRIC_MEM"] = "1"
	}
	mode := p.PD.mode()
	for key, value := range mode.Env {
		env[key] = value
	}
	// vLLM's MooncakeConnector serves its KV handshake from a bootstrap
	// server on the prefill instance, over HTTP.
	if mode.Transport.MooncakeBootstrap() {
		if p.PD.Role == pdmode.RolePrefill && p.PD.SideChannelPort > 0 {
			env["VLLM_MOONCAKE_BOOTSTRAP_PORT"] = strconv.Itoa(p.PD.SideChannelPort)
		}
		if p.PD.SideChannelHost != "" {
			env["VLLM_HOST_IP"] = p.PD.SideChannelHost
		}
		pdNoProxy(p, env)
	}
	if mode.SideChannel == pdmode.SideChannelNIXL {
		if p.PD.SideChannelHost != "" {
			env["VLLM_NIXL_SIDE_CHANNEL_HOST"] = p.PD.SideChannelHost
		}
		if p.PD.SideChannelPort > 0 {
			env["VLLM_NIXL_SIDE_CHANNEL_PORT"] = strconv.Itoa(p.PD.SideChannelPort)
		}
	}
	if p.PD.SideChannelHost != "" {
		for _, key := range mode.AddressEnv {
			env[key] = p.PD.SideChannelHost
		}
	}
	if iface := p.ComputeNetwork.Interface; iface != "" {
		for _, key := range mode.InterfaceEnv {
			env[key] = iface
		}
	}
	for key, value := range kvTransferNetworkEnv(p) {
		env[key] = value
	}
}

// PDModeProfile is one PD mode as the deploy form sees it: what it offers
// and the rules it checks, all read from package pdmode.
type PDModeProfile struct {
	Name string `json:"name"`
	// Vendors are the accelerator vendors the mode runs on.
	Vendors []string `json:"vendors"`
	// Roles in the mode's order; ScalableToZero are those a running
	// deployment may shrink to no instances.
	Roles          []string `json:"roles"`
	ScalableToZero []string `json:"scalable_to_zero"`
	// TPRule constrains the two roles' TP (pdmode.TPRule values).
	TPRule          string `json:"tp_rule"`
	MaxUnitsPerRole int    `json:"max_units_per_role"`
	// Fallbacks and KVLoadFailures are the allowed policy values, the
	// default first; KVLoadFailures is empty when the engine has no such
	// setting.
	Fallbacks      []string `json:"fallbacks"`
	KVLoadFailures []string `json:"kv_load_failures"`
	Unverified     bool     `json:"unverified,omitempty"`
	// FirstRequestCompiles: the first request after a start compiles
	// kernels and takes minutes (pdmode.Mode.FirstRequestCompiles).
	FirstRequestCompiles bool `json:"first_request_compiles,omitempty"`
}

func pdModeProfiles(engine string) []PDModeProfile {
	var out []PDModeProfile
	for _, name := range pdmode.ForEngine(engine) {
		mode, _ := pdmode.Lookup(name)
		out = append(out, PDModeProfile{
			Name: mode.Name, Vendors: nonNil(mode.Vendors), Roles: nonNil(mode.Roles), ScalableToZero: nonNil(mode.ScalableToZero),
			TPRule: string(mode.TP), MaxUnitsPerRole: pdmode.MaxUnitsPerRole,
			Fallbacks: nonNil(mode.Fallbacks()), KVLoadFailures: nonNil(mode.KVLoadFailures), Unverified: mode.Unverified,
			FirstRequestCompiles: mode.FirstRequestCompiles,
		})
	}
	return out
}

// nonNil keeps an empty list a JSON array.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return append([]string(nil), list...)
}
