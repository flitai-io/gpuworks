// Package ucxnetlink finds out whether the host kernel answers an IPv6 route
// dump the way UCX 1.20-1.22 (bundled with NIXL) cannot read.
//
// UCX decides whether a peer is reachable by dumping the IPv6 routes over a
// NETLINK_ROUTE socket (RTM_GETROUTE with NLM_F_DUMP) and reading datagrams
// until NLMSG_DONE. It looks for DONE only as the first message of a datagram
// (openucx/ucx 6d96bec1a0; fixed upstream in 32c7a90a45, #12182). Older
// kernels send DONE as a datagram of its own, so that works. Newer ones (Ubuntu
// 5.15.0-198 and -203, for one) append DONE to the datagram that carries the
// last routes, UCX never sees it, and its next recv() blocks for good while
// another thread spins. NIXL initialisation hangs, so vLLM and SGLang PD never
// start. The probe sends the same dump and reports a datagram that holds a
// route followed by DONE.
//
// The socket belongs to the network namespace of the agent. On Kubernetes the
// agent pod runs on the host network and natively elsewhere, so it sees the
// routes the engine containers (host network too) will dump.
package ucxnetlink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Netlink message types (linux/netlink.h, linux/rtnetlink.h).
const (
	nlmsgError   = 2
	nlmsgDone    = 3
	rtmNewRoute  = 24
	nlmsgHdrLen  = 16
	nlmsgAlignTo = 4
)

// ErrUnsupported is what a Dumper returns where there is no netlink.
var ErrUnsupported = errors.New("netlink route dumps are not available on this OS")

// Dumper sends an RTM_GETROUTE dump for AF_INET6 and returns the datagrams it
// answers with, in order, up to and including the one that holds NLMSG_DONE.
// It must give up with an error at the context's deadline and at the limits
// of collect; a Dumper that does neither is abandoned by its caller.
type Dumper func(ctx context.Context) ([][]byte, error)

// Result is what one probe saw.
type Result struct {
	// Affected: some datagram holds route messages followed by NLMSG_DONE, so
	// UCX, which reads only the first message of a datagram, misses the end.
	Affected bool
	// Routes counts the route messages in the dump.
	Routes int
	// Datagrams is how many datagrams the dump took.
	Datagrams int
}

// Analyze reads the datagrams of a finished dump. A dump that ended in an
// error message, was cut short, or never said it was done is an error: what
// the kernel answered says nothing about UCX then.
func Analyze(datagrams [][]byte) (Result, error) {
	res := Result{Datagrams: len(datagrams)}
	done := false
	for _, datagram := range datagrams {
		position := 0
		for off := 0; off < len(datagram); {
			if len(datagram)-off < nlmsgHdrLen {
				return res, fmt.Errorf("netlink datagram has %d trailing bytes, fewer than a header", len(datagram)-off)
			}
			length := int(binary.NativeEndian.Uint32(datagram[off:]))
			typ := binary.NativeEndian.Uint16(datagram[off+4:])
			if length < nlmsgHdrLen || off+length > len(datagram) {
				return res, fmt.Errorf("netlink message of %d bytes does not fit its datagram", length)
			}
			switch typ {
			case nlmsgError:
				return res, errors.New("the kernel answered the route dump with an error")
			case nlmsgDone:
				done = true
				if res.Routes > 0 && position > 0 && countRoutes(datagram[:off]) > 0 {
					res.Affected = true
				}
			case rtmNewRoute:
				res.Routes++
			}
			position++
			off += (length + nlmsgAlignTo - 1) &^ (nlmsgAlignTo - 1)
		}
		if done {
			break
		}
	}
	if !done {
		return res, errors.New("the route dump ended without NLMSG_DONE")
	}
	return res, nil
}

// countRoutes counts the route messages at the start of a datagram, up to
// before the message at its end offset.
func countRoutes(head []byte) int {
	n := 0
	for off := 0; off+nlmsgHdrLen <= len(head); {
		length := int(binary.NativeEndian.Uint32(head[off:]))
		if length < nlmsgHdrLen || off+length > len(head) {
			break
		}
		if binary.NativeEndian.Uint16(head[off+4:]) == rtmNewRoute {
			n++
		}
		off += (length + nlmsgAlignTo - 1) &^ (nlmsgAlignTo - 1)
	}
	return n
}

// Limits on what one dump may read. A dump of a host's IPv6 routes is a few
// datagrams; these only stop a socket that never says DONE (or a table of a
// size no GPU node has) from being read for ever.
const (
	maxDatagrams = 1024
	maxBytes     = 8 << 20
	// recvBuffer is larger than any datagram a route dump produces, so none is
	// truncated; a datagram that fills it is treated as truncated.
	recvBuffer = 64 * 1024
)

// limits are the caps collect applies; tests lower them.
type limits struct{ datagrams, bytes, buffer int }

var defaultLimits = limits{datagrams: maxDatagrams, bytes: maxBytes, buffer: recvBuffer}

// collect reads datagrams with recv until one holds NLMSG_DONE or NLMSG_ERROR,
// the context ends, or a limit is passed. It returns what it read with the
// error that stopped it.
func collect(ctx context.Context, recv func(buf []byte) (int, error), lim limits) ([][]byte, error) {
	var datagrams [][]byte
	buf := make([]byte, lim.buffer)
	total := 0
	for len(datagrams) < lim.datagrams {
		if err := ctx.Err(); err != nil {
			return datagrams, err
		}
		n, err := recv(buf)
		if err != nil {
			return datagrams, err
		}
		switch {
		case n <= 0:
			return datagrams, errors.New("the netlink socket returned an empty read")
		case n >= len(buf):
			return datagrams, fmt.Errorf("a netlink datagram fills the %d byte buffer and may be cut short", len(buf))
		}
		if total += n; total > lim.bytes {
			return datagrams, fmt.Errorf("the route dump is more than %d bytes", lim.bytes)
		}
		datagram := append([]byte(nil), buf[:n]...)
		datagrams = append(datagrams, datagram)
		if hasDone(datagram) {
			return datagrams, nil
		}
	}
	return datagrams, fmt.Errorf("the route dump is more than %d datagrams", lim.datagrams)
}

// hasDone reports whether any message of the datagram is NLMSG_DONE or
// NLMSG_ERROR, which both end a dump; a datagram that cannot be read to its
// end also stops the read, since nothing more can be taken from it.
func hasDone(datagram []byte) bool {
	for off := 0; off+nlmsgHdrLen <= len(datagram); {
		length := int(binary.NativeEndian.Uint32(datagram[off:]))
		if length < nlmsgHdrLen || off+length > len(datagram) {
			return true
		}
		if typ := binary.NativeEndian.Uint16(datagram[off+4:]); typ == nlmsgDone || typ == nlmsgError {
			return true
		}
		off += (length + nlmsgAlignTo - 1) &^ (nlmsgAlignTo - 1)
	}
	return false
}

// Kernel is the host's kernel release, or "" when it cannot be read.
func Kernel() string {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// hostGate lets one probe run in the whole process: the monitor's rounds and
// a diagnosis share it.
var hostGate atomic.Bool

// Monitor probes on first use and again when its answer is older than
// Interval (a sysctl may have changed since), never more often. The probe runs
// in a goroutine of its own that the caller waits for only until Timeout: one
// that does not come back is abandoned, counted as unknown, and no second
// probe starts while it is still out. Nothing here runs a program or opens a
// file or device other than the netlink socket.
type Monitor struct {
	Dump            Dumper
	Kernel          func() string
	Now             func() time.Time
	Log             *slog.Logger
	Timeout         time.Duration
	Interval        time.Duration
	RetryAfterError time.Duration

	gate *atomic.Bool

	mu         sync.Mutex
	checked    time.Time
	last       *proto.UCXNetlinkHang
	known      bool
	skipLogged bool
}

// NewMonitor is the monitor of this host: a probe every interval with a three
// second budget. The agent passes the interval of its card runtime checks.
func NewMonitor(log *slog.Logger, interval time.Duration) *Monitor {
	return &Monitor{
		Dump: HostDump, Kernel: Kernel, Now: time.Now, Log: log, gate: &hostGate,
		Timeout: 3 * time.Second, Interval: interval, RetryAfterError: time.Minute,
	}
}

// Status is the fact to report with the node's status: non-nil while the host
// is affected, nil while it is not or is not known.
func (m *Monitor) Status(ctx context.Context) *proto.UCXNetlinkHang {
	hang, _ := m.observe(ctx, false)
	return hang
}

// Fresh probes now, whatever the interval, and says whether the answer is
// known: a probe that failed, timed out or was skipped gives the last one.
func (m *Monitor) Fresh(ctx context.Context) (hang *proto.UCXNetlinkHang, known bool) {
	return m.observe(ctx, true)
}

func (m *Monitor) observe(ctx context.Context, force bool) (*proto.UCXNetlinkHang, bool) {
	if m.Dump == nil {
		return nil, false
	}
	now := m.Now()
	m.mu.Lock()
	if m.gate == nil {
		m.gate = new(atomic.Bool)
	}
	if !force && !m.checked.IsZero() && now.Sub(m.checked) < m.Interval {
		defer m.mu.Unlock()
		return m.last, m.known
	}
	m.mu.Unlock()
	if !m.gate.CompareAndSwap(false, true) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.skipLogged && m.Log != nil {
			m.skipLogged = true
			m.Log.Warn("the previous ucx netlink probe has not returned; skipping this one")
		}
		return m.last, m.known
	}
	pctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer m.gate.Store(false)
		datagrams, err := m.Dump(pctx)
		var res Result
		if err == nil {
			res, err = Analyze(datagrams)
		}
		m.finish(res, err)
		close(done)
	}()
	select {
	case <-done:
	case <-pctx.Done():
		select {
		case <-done:
		default:
			// Abandoned: whatever it finds later is kept, nobody waits for it.
			m.mu.Lock()
			m.checked = m.Now().Add(m.RetryAfterError - m.Interval)
			m.mu.Unlock()
			if m.Log != nil {
				m.Log.Warn("the ucx netlink probe did not return in time; abandoning it", "timeout", m.Timeout)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.known
}

// finish records what a probe found; a failed probe keeps the last answer and
// is tried again after RetryAfterError.
func (m *Monitor) finish(res Result, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	if err != nil {
		if !errors.Is(err, ErrUnsupported) && m.Log != nil {
			m.Log.Debug("ucx netlink probe failed", "err", err)
		}
		m.checked = now.Add(m.RetryAfterError - m.Interval)
		return
	}
	m.checked, m.known = now, true
	switch {
	case !res.Affected:
		m.last = nil
	case m.last == nil:
		kernel := ""
		if m.Kernel != nil {
			kernel = m.Kernel()
		}
		m.last = &proto.UCXNetlinkHang{Kernel: kernel}
		if m.Log != nil {
			m.Log.Warn("this kernel answers an IPv6 route dump in a way that hangs UCX (NIXL); PD over NIXL cannot start here", "kernel", kernel, "routes", res.Routes)
		}
	}
}
