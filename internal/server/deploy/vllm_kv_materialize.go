package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
)

// canUnsetTemplateFlag recognizes only the transfer objects this vLLM adapter
// emits without PD: the disk tier, and a KV offload backend's own object
// (RecognizeKVBackend). --kv-transfer-config also carries PD and third-party
// connectors; those stay opaque and produce an explicit unset conflict.
func (e vLLMEngine) CanUnsetTemplateFlag(flag, raw string) bool {
	if flag != "--kv-transfer-config" {
		return false
	}
	if _, ok := e.RecognizeKVBackend(flag, raw); ok {
		return true
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(unquoteArgument(raw)), &config); err != nil {
		return false
	}
	if !onlyVLLMKVKeys(config, "kv_connector", "kv_role", "kv_connector_extra_config") || config["kv_connector"] != "OffloadingConnector" || config["kv_role"] != "kv_both" {
		return false
	}
	extra, ok := config["kv_connector_extra_config"].(map[string]any)
	if !ok || !onlyVLLMKVKeys(extra, "spec_name", "cpu_bytes_to_use", "secondary_tiers", "block_size") || extra["spec_name"] != "TieringOffloadingSpec" || !positiveJSONNumber(extra["cpu_bytes_to_use"]) {
		return false
	}
	tiers, ok := extra["secondary_tiers"].([]any)
	if !ok || len(tiers) != 1 {
		return false
	}
	tier, ok := tiers[0].(map[string]any)
	if !ok || !onlyVLLMKVKeys(tier, "type", "root_dir", "n_read_threads", "n_write_threads") || tier["type"] != "fs" {
		return false
	}
	root, rootOK := tier["root_dir"].(string)
	return rootOK && strings.TrimSpace(root) != "" && positiveJSONNumber(tier["n_read_threads"]) && positiveJSONNumber(tier["n_write_threads"])
}

func positiveJSONNumber(value any) bool {
	number, ok := value.(float64)
	return ok && number > 0
}

func onlyVLLMKVKeys(values map[string]any, allowed ...string) bool {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}
	for key := range values {
		if _, ok := known[key]; !ok {
			return false
		}
	}
	return true
}

// RecognizeKVBackend reads a --kv-transfer-config value back into the
// selectable backend it configures: through the backend's own reader, which
// also answers its dictionary values, or, for a backend without one, when
// the object is exactly what it renders selected with nothing else stated.
// Only the non-PD object is recognized: a MultiConnector wrapping a PD
// connector stays opaque, and ExportLaunchArgs builds without PD, so a
// template only ever carries the non-PD shape.
func (vLLMEngine) RecognizeKVBackend(flag, raw string) (KVBackendRecognition, bool) {
	if flag != "--kv-transfer-config" {
		return KVBackendRecognition{}, false
	}
	var stated any
	if err := json.Unmarshal([]byte(unquoteArgument(raw)), &stated); err != nil {
		return KVBackendRecognition{}, false
	}
	object, isObject := stated.(map[string]any)
	for _, row := range selectableKVBackends(EngineVLLM) {
		backend, ok := kvBackends.vllmBackend(row.ID)
		if !row.External || !ok {
			continue
		}
		if backend.recognize != nil {
			if !isObject {
				continue
			}
			if values, ok := backend.recognize(object); ok {
				return KVBackendRecognition{ID: row.ID, Values: values}, true
			}
			continue
		}
		rendered, ok := vllmKVTransferConfig(Params{Quick: LaunchConfig{KVOffloadBackend: row.ID}})
		if !ok {
			continue
		}
		var own any
		if json.Unmarshal([]byte(rendered), &own) == nil && reflect.DeepEqual(stated, own) {
			return KVBackendRecognition{ID: row.ID}, true
		}
	}
	return KVBackendRecognition{}, false
}
