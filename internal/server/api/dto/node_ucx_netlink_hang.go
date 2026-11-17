package dto

// NodeUCXNetlinkHang says the node's kernel answers an IPv6 route dump in a
// way that hangs UCX 1.20-1.22, which NIXL bundles: a PD engine that moves its
// KV with NIXL never finishes starting on the node. It is present only while
// the agent says so, and the server derives Fixes from it; a client shows the
// text for each fix Code in its own language and the commands as they are.
type NodeUCXNetlinkHang struct {
	// Kernel is the node's kernel release.
	Kernel string `json:"kernel,omitempty"`
	// Fixes are the ways to stop it: IPv6 off on the host, now and for good.
	Fixes []NodeUCXNetlinkHangFix `json:"fixes"`
}

// NodeUCXNetlinkHangFix is one way to end the hang.
type NodeUCXNetlinkHangFix struct {
	// Code names the fix (disable_ipv6_now, disable_ipv6_persist).
	Code string `json:"code"`
	// Commands are run in order, as written, on the node.
	Commands []string `json:"commands"`
}
