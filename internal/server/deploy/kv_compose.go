// kv_compose.go is the single assembly point of vLLM's --kv-transfer-config
// (ADR 0069). A launch contributes at most one PD connector and at most one
// KV offload backend; the flag carries both, because a second flag would
// replace the first: measured on vLLM 0.25.1, --kv-offloading-size next to a
// NIXL config silently drops NIXL.

package deploy

import "encoding/json"

// kvContribution is one part of the composed config.
type kvContribution struct {
	object map[string]any
	// role is the PD connector's role, which becomes the outer role when the
	// PD part is wrapped together with an offload part.
	role string
	// standaloneAsFlags: an offload part that, with no PD part beside it, is
	// carried by the engine's own flags instead of a connector object (vLLM's
	// native CPU tier: --kv-offloading-size and --kv-offloading-backend).
	standaloneAsFlags bool
	// wrapper are top-level keys an offload part needs on the MultiConnector
	// that wraps it beside a PD part (a vendor's connector module path, say).
	// The composer's own keys win over them.
	wrapper map[string]any
	// env is environment the engine process needs for this part; the adapter
	// merges it into the launch (vLLMEngine.Build).
	env map[string]string
}

// composeVLLMKVTransfer answers the --kv-transfer-config value, or false when
// the launch states none:
//
//   - no part, or only an offload part carried by flags: no config;
//   - one offload part: that object;
//   - only a PD part: that object;
//   - both: a MultiConnector of [pd, offload] in the PD role.
//
// The KV load failure policy is a PD setting the engine reads only at the top
// level, so it is set on the outermost object.
func composeVLLMKVTransfer(pd, offload *kvContribution, policy string) (string, bool) {
	var config map[string]any
	switch {
	case pd == nil && (offload == nil || offload.standaloneAsFlags):
		return "", false
	case pd == nil:
		config = cloneJSONObject(offload.object)
	case offload == nil:
		config = cloneJSONObject(pd.object)
	default:
		config = cloneJSONObject(offload.wrapper)
		config["kv_connector"] = "MultiConnector"
		config["kv_role"] = pd.role
		config["kv_connector_extra_config"] = map[string]any{"connectors": []map[string]any{pd.object, offload.object}}
	}
	if pd != nil && policy != "" {
		config["kv_load_failure_policy"] = policy
	}
	raw, _ := json.Marshal(config)
	return string(raw), true
}

// cloneJSONObject copies the top level, which is all the composer writes.
func cloneJSONObject(object map[string]any) map[string]any {
	out := make(map[string]any, len(object)+1)
	for key, value := range object {
		out[key] = value
	}
	return out
}

// vllmKVTransferConfig composes this launch's parts: the PD connector of a PD
// instance, and the selected KV offload backend (PDRoleLaunchConfig has
// already dropped it on a role that does not keep it).
func vllmKVTransferConfig(p Params) (string, bool) {
	config, ok, _ := vllmKVTransfer(p)
	return config, ok
}

// vllmKVTransfer is vllmKVTransferConfig plus the environment the offload
// part asks for, rendered once.
func vllmKVTransfer(p Params) (string, bool, map[string]string) {
	var pd *kvContribution
	policy := ""
	if p.PD != nil {
		pd, policy = vllmPDContribution(p)
	}
	offload := vllmOffloadContribution(p)
	config, ok := composeVLLMKVTransfer(pd, offload, policy)
	if offload == nil {
		return config, ok, nil
	}
	return config, ok, offload.env
}

// vllmOffloadContribution renders the selected backend's part, nil when the
// selection names no row with a renderer (validateKVBackend refuses an
// unknown id before any launch).
func vllmOffloadContribution(p Params) *kvContribution {
	row, ok := selectedKVBackend(EngineVLLM, p.Quick)
	if !ok {
		return nil
	}
	backend, ok := kvBackends.vllmBackend(row.ID)
	if !ok {
		return nil
	}
	return backend.render(p)
}
