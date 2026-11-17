// kv_render_vllm.go renders the parts of vLLM's --kv-transfer-config: the PD
// connector of a PD instance's mode and role, and each KV offload backend's
// connector object (ADR 0069). Composing them into the one flag is
// kv_compose.go's job; nothing here decides how parts combine.

package deploy

import (
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// vllmOffloadRenderer renders one KV offload backend's contribution for a
// launch, nil when the launch asks for no offload. It sees the whole launch
// (the PD role on a PD instance, the vendor, the compute network, the
// parallelism), so a backend decides its own role, endpoint, wrapper keys and
// environment; the composer only assembles the parts.
type vllmOffloadRenderer func(p Params) *kvContribution

// vllmOffloadReader reads a stated --kv-transfer-config object back into
// the backend's own dictionary values, false when the object is not the
// backend's.
type vllmOffloadReader func(object map[string]any) (map[string]any, bool)

// vllmOffloadBackend is a backend's vLLM half: how it renders, and how a
// template's object reads back into its selection. A backend with no reader
// is recognized only by exactly the object it renders when selected with
// nothing else stated; one whose object carries its own values needs one.
type vllmOffloadBackend struct {
	render    vllmOffloadRenderer
	recognize vllmOffloadReader
}

// vllmOffloadBackends are the backends the product ships, keyed by kvbackend
// id. A backend row without one here cannot be selected
// (TestKVBackendSetRendersEverySelectableRow).
func vllmOffloadBackends() map[string]vllmOffloadBackend {
	return map[string]vllmOffloadBackend{kvbackend.Native: {render: nativeVLLMOffload}}
}

// nativeKVKeys are the launch configuration keys of an engine's own KV
// offload: what an external backend replaces (it must replace all of them,
// because the adapter renders no native tier while a backend is selected),
// what validateKVBackend judges, and what PDRoleLaunchConfig drops on a role
// that keeps no offload (TestKVBackendSetRendersEverySelectableRow and
// TestPDRoleLaunchConfigDropsEveryOffloadKey hold the three to this list).
// kv_offload_block_size has no dictionary row but is a native tier setting
// all the same. Only vLLM has a KV offload adapter; a row of another engine
// is refused by the same test until that engine's adapter renders it.
func nativeKVKeys(engine string) []string {
	if engine != EngineVLLM {
		return nil
	}
	return []string{
		paramKVOffloadingSize, "kv_disk_offload", "shared_kv_dir", "kv_disk_limit_gib",
		"kv_offload_read_threads", "kv_offload_write_threads", "kv_offload_block_size",
	}
}

// nativeVLLMOffload is vLLM's own offload: the disk tier as a tiering
// OffloadingConnector, or the CPU tier, which without a PD connector is
// carried by vLLM's own flags and only joins a composition as an object.
func nativeVLLMOffload(p Params) *kvContribution {
	q := p.Quick
	switch {
	case q.KVDiskOffload:
		return &kvContribution{object: diskKVTransferObject(q)}
	case q.KVOffloadingSize > 0:
		return &kvContribution{object: map[string]any{
			"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
			"kv_connector_extra_config": map[string]any{"cpu_bytes_to_use": KVCPUTierBytes(q)},
		}, standaloneAsFlags: true}
	}
	return nil
}

// vllmPDContribution is a PD instance's connector in its role's connector
// role, and the mode's KV load failure policy, which the engine reads only at
// the top level of the composed config.
func vllmPDContribution(p Params) (*kvContribution, string) {
	mode := p.PD.mode()
	policy := mode.EffectiveKVLoadFailure(p.PD.KVLoadFailure)
	role := mode.ConnectorRole(p.PD.Role)
	pd := map[string]any{"kv_connector": mode.Connector, "kv_role": role}
	if mode.ConnectorPortKey != "" && p.PD.SideChannelPort > 0 {
		pd[mode.ConnectorPortKey] = p.sideChannelPortText()
	}
	if mode.ConnectorParallelism {
		sides := map[string]any{}
		for _, r := range mode.Roles {
			sides[r] = map[string]any{"tp_size": max(p.PD.RoleTP[r], 1), "dp_size": 1}
		}
		pd["kv_connector_extra_config"] = sides
	}
	if len(mode.ConnectorExtra) > 0 || mode.Transport.MooncakeBootstrap() {
		extra, _ := pd["kv_connector_extra_config"].(map[string]any)
		if extra == nil {
			extra = map[string]any{}
		}
		for key, value := range mode.ConnectorExtra {
			extra[key] = value
		}
		// TransportMooncake follows the network the group shares; the RDMA
		// transport states its protocol in ConnectorExtra.
		if mode.Transport == pdmode.TransportMooncake {
			extra["mooncake_protocol"] = MooncakeProtocol(p.ComputeNetwork)
		}
		if hcas := hcaDevices(p.ComputeNetwork.HCAs); len(hcas) > 0 && extra["mooncake_protocol"] == "rdma" {
			extra["device_name"] = strings.Join(hcas, ",")
		}
		pd["kv_connector_extra_config"] = extra
	}
	// No engine_id: vLLM gives each engine process a fresh one, and the
	// gateway reads it from the prefill's bootstrap server
	// (pdmode.Mode.SynthesizedHandoff).
	return &kvContribution{object: pd, role: role}, policy
}

func diskKVTransferConfig(q LaunchConfig) string {
	config, _ := composeVLLMKVTransfer(nil, &kvContribution{object: diskKVTransferObject(q)}, "")
	return config
}

func diskKVTransferObject(q LaunchConfig) map[string]any {
	// The thread counts are folded into one composite flag, so an unset one has
	// no "emit nothing" spelling of its own: the dictionary's default is what
	// the connector is given.
	readThreads, writeThreads := q.KVOffloadReadThreads, q.KVOffloadWriteThreads
	if readThreads <= 0 {
		readThreads = defaultInt(EngineVLLM, "kv_offload_read_threads")
	}
	if writeThreads <= 0 {
		writeThreads = defaultInt(EngineVLLM, "kv_offload_write_threads")
	}
	extra := map[string]any{
		"spec_name":        "TieringOffloadingSpec",
		"cpu_bytes_to_use": KVCPUTierBytes(q),
		"secondary_tiers": []map[string]any{{
			"type": "fs", "root_dir": filepath.Clean(q.SharedKVDir),
			"n_read_threads": readThreads, "n_write_threads": writeThreads,
		}},
	}
	if q.KVOffloadBlockSize > 0 {
		extra["block_size"] = q.KVOffloadBlockSize
	}
	return map[string]any{
		"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
		"kv_connector_extra_config": extra,
	}
}
