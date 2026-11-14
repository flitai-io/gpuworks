//go:build linux

package ucxnetlink

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
)

// defaultBudget bounds a dump whose context carries no deadline.
const defaultBudget = 3 * time.Second

// HostDump dumps the IPv6 routes of the agent's network namespace the way UCX
// does and returns the datagrams as they arrived. The socket is non-blocking
// and read through Go's poller with the context's deadline (at most a few
// seconds), so a read the kernel never answers returns at the deadline; ending
// the context ends the read at once; the descriptor is closed on every path;
// and at most maxDatagrams / maxBytes are read.
func HostDump(ctx context.Context) ([][]byte, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind netlink socket: %w", err)
	}
	sock := os.NewFile(uintptr(fd), "netlink-route") // owns fd from here
	defer sock.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultBudget)
	}
	if err := sock.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set socket deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = sock.SetDeadline(time.Now()) })
	defer stop()
	// A write on a netlink socket with no address goes to the kernel.
	if _, err := sock.Write(routeDumpRequest()); err != nil {
		return nil, fmt.Errorf("send route dump request: %w", err)
	}
	return collect(ctx, sock.Read, defaultLimits)
}

// routeDumpRequest is RTM_GETROUTE with NLM_F_DUMP for AF_INET6: an nlmsghdr
// and a zeroed rtmsg whose family is the only field set.
func routeDumpRequest() []byte {
	const rtmsgLen = 12
	msg := make([]byte, nlmsgHdrLen+rtmsgLen)
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:], syscall.RTM_GETROUTE)
	binary.NativeEndian.PutUint16(msg[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(msg[8:], 1) // sequence
	msg[nlmsgHdrLen] = syscall.AF_INET6
	return msg
}
