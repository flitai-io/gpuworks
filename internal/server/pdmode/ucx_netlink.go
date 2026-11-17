package pdmode

import "gpuworks.ai/gpuworks/internal/proto"

// Codes of the fixes for a node whose kernel hangs UCX (NIXL); the console has
// the text for each.
const (
	UCXNetlinkFixDisableIPv6Now     = "disable_ipv6_now"
	UCXNetlinkFixDisableIPv6Persist = "disable_ipv6_persist"
)

// UCXNetlinkFix is one way to stop a node hanging UCX: Commands are run in
// order, as written, on the node.
type UCXNetlinkFix struct {
	Code     string   `json:"code"`
	Commands []string `json:"commands"`
}

// UCXNetlinkFixes are the fixes for a node whose IPv6 route dump hangs UCX
// 1.20-1.22 (ADR 0028): turn IPv6 off on the host (on single interfaces it
// does not help), now and then for good. The same list is on the node and in
// the deploy refusal.
func UCXNetlinkFixes() []UCXNetlinkFix {
	return []UCXNetlinkFix{
		{Code: UCXNetlinkFixDisableIPv6Now, Commands: []string{proto.UCXNetlinkFixNow}},
		{Code: UCXNetlinkFixDisableIPv6Persist, Commands: []string{proto.UCXNetlinkFixPersist}},
	}
}

// UCXNetlinkAffects reports whether a node's kernel hanging UCX matters for a
// mode: only a KV transport carried by UCX is hit.
func (m Mode) UCXNetlinkAffects() bool { return m.Transport == TransportUCX }
