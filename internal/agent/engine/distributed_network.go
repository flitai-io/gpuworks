package engine

import (
	"fmt"
	"net"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

const vllmHostIPEnv = "VLLM_HOST_IP"

// The socket-interface variables a distributed launch needs pointed at the
// interface this node is reached on, per accelerator vendor. The collective
// library is the vendor's: NCCL on NVIDIA, HCCL on Ascend, and vLLM-Ascend
// additionally reads TP_SOCKET_IFNAME for its tensor-parallel socket.
//
// Filling them is what keeps an Ascend template from having to name an
// interface it cannot know — the name belongs to the node, not to the recipe
// — but a vendor is only ever given its own variables: naming all of them
// everywhere would start resolving an interface for NVIDIA launches that
// never needed one, and inject Ascend variables into CUDA containers.
var distributedSocketInterfaceEnv = map[string][]string{
	accelerator.VendorNVIDIA: {"GLOO_SOCKET_IFNAME", "NCCL_SOCKET_IFNAME"},
	// RCCL reads the NCCL_* names.
	accelerator.VendorAMD:    {"GLOO_SOCKET_IFNAME", "NCCL_SOCKET_IFNAME"},
	accelerator.VendorAscend: {"GLOO_SOCKET_IFNAME", "HCCL_SOCKET_IFNAME", "TP_SOCKET_IFNAME"},
}

// socketInterfaceEnvFor is the variables to fill on this node. A vendor with
// no entry keeps the pair vLLM has always needed and nothing else: an unknown
// accelerator is not a reason to start naming another vendor's collective.
func socketInterfaceEnvFor(vendor string) []string {
	if keys, ok := distributedSocketInterfaceEnv[accelerator.NormalizeVendor(vendor)]; ok {
		return keys
	}
	return distributedSocketInterfaceEnv[accelerator.VendorNVIDIA]
}

// EngineOption customizes an Engine dependency while preserving production
// defaults. It is primarily useful for hermetic agents whose advertised node
// addresses are simulated rather than assigned to the test host.
//
//nolint:revive // Preserve the exported Engine constructor option type for callers.
type EngineOption func(*Engine)

// WithNetworkInterfaceResolver supplies the mapping from an advertised node IP
// to its host-network interface. Production agents use net.Interfaces; test
// environments may inject the interface represented by their fake network.
func WithNetworkInterfaceResolver(resolve func(string) (string, error)) EngineOption {
	return func(engine *Engine) {
		engine.networkInterfaceName = resolve
	}
}

// applyDistributedNetworkInterface replaces vLLM's empty multi-node socket
// interface markers with the host interface that owns VLLM_HOST_IP. Which
// markers those are is the node's accelerator vendor's business
// (socketInterfaceEnvFor). The distributed containers use host networking, so
// the host interface name is also valid inside Docker and udocker. Explicit
// user overrides remain authoritative.
func applyDistributedNetworkInterface(
	ws *runtime.WorkloadSpec,
	spec proto.InstanceDesiredState,
	vendor string,
	resolve func(string) (string, error),
) error {
	if spec.NNodes <= 1 || !strings.EqualFold(strings.TrimSpace(spec.Engine), engines.VLLM) {
		return nil
	}
	if !spec.HostNetwork {
		return fmt.Errorf("vLLM nnodes=%d requires host networking", spec.NNodes)
	}
	if ws == nil {
		return fmt.Errorf("workload spec is nil")
	}
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}

	keys := socketInterfaceEnvFor(vendor)
	needsResolution := false
	for _, key := range keys {
		if strings.TrimSpace(ws.Env[key]) == "" {
			needsResolution = true
			break
		}
	}
	if !needsResolution {
		return nil
	}

	hostIP := strings.TrimSpace(ws.Env[vllmHostIPEnv])
	if hostIP == "" {
		return fmt.Errorf("%s is empty", vllmHostIPEnv)
	}
	if resolve == nil {
		return fmt.Errorf("network interface resolver is nil")
	}
	interfaceName, err := resolve(hostIP)
	if err != nil {
		return fmt.Errorf("resolve interface for %s=%q: %w", vllmHostIPEnv, hostIP, err)
	}
	interfaceName = strings.TrimSpace(interfaceName)
	if interfaceName == "" {
		return fmt.Errorf("resolve interface for %s=%q: empty interface name", vllmHostIPEnv, hostIP)
	}

	for _, key := range keys {
		if strings.TrimSpace(ws.Env[key]) == "" {
			ws.Env[key] = interfaceName
		}
	}
	return nil
}

// networkInterfaceNameForIP returns the active, non-loopback interface that
// owns rawIP. Matching the advertised node address is more reliable than
// guessing a provider-specific name such as eth0, ens5, or enp1s0.
func networkInterfaceNameForIP(rawIP string) (string, error) {
	want := net.ParseIP(strings.TrimSpace(rawIP))
	if want == nil {
		return "", fmt.Errorf("invalid IP address %q", rawIP)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list host interfaces: %w", err)
	}
	for _, candidate := range interfaces {
		if candidate.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := candidate.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var local net.IP
			switch value := address.(type) {
			case *net.IPNet:
				local = value.IP
			case *net.IPAddr:
				local = value.IP
			default:
				text := address.String()
				if ip, _, parseErr := net.ParseCIDR(text); parseErr == nil {
					local = ip
				} else {
					local = net.ParseIP(text)
				}
			}
			if local == nil || !local.Equal(want) {
				continue
			}
			if candidate.Flags&net.FlagLoopback != 0 {
				return "", fmt.Errorf("IP %s belongs to loopback interface %s", want, candidate.Name)
			}
			return candidate.Name, nil
		}
	}
	return "", fmt.Errorf("no active host interface owns IP %s", want)
}

// proberVendor is this node's accelerator vendor, or "" when there is no
// prober to ask. It decides which collective library's variables a
// distributed launch gets.
func (e *Engine) proberVendor() string {
	if e.prober == nil {
		return ""
	}
	return e.prober.Vendor()
}
