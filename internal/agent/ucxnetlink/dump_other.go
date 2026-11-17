//go:build !linux

package ucxnetlink

import "context"

// HostDump has nothing to dump off Linux: UCX's netlink reachability check,
// and the kernel behaviour that breaks it, are Linux's.
func HostDump(context.Context) ([][]byte, error) { return nil, ErrUnsupported }
