package dto

// KVOffloadBackendRef names the KV offload backend (ADR 0069) an instance
// keeps (Instance.KVOffloadBackend): its id and its product name, shown as
// is. It is the deployment's selection as the instance's PD role scopes it,
// absent when the instance keeps the engine's own tiers or none; only a
// deployment's read with its instances carries it.
type KVOffloadBackendRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
