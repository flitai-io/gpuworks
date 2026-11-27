// Package testport hands tests loopback addresses to bind later.
//
// A test that needs an address before the code under test binds it (the
// address goes into a configuration, a certificate, a cluster's records)
// cannot hold a listener and pass it on. Listening on port 0 and closing gives
// such an address, but from the kernel's ephemeral range, which every outgoing
// connection and every other port-0 listener on the machine draws from: on a
// CI runner whose jobs share the host network the port is often taken again
// before the test binds it ("bind: address already in use").
//
// The addresses here come from a pool below the ephemeral range (see
// poolEnd), where the kernel assigns no port by itself.
//
// What that does and does not give:
//
//   - A port is checked by binding it and releasing it again, so this is
//     still check-then-use. Between the check and the test's own bind, only
//     a process that asks for that port by number can take it.
//   - Every port is handed out together with the one after it, which is
//     checked the same way and never handed out: a clustered App binds its
//     member session listener at its database port plus one.
//   - A port is handed out once per process. `go test` runs each package as
//     its own process, and CI runs several jobs on one host network, each in
//     its own container; none of them know of each other. To keep them apart,
//     a process starts at a random place in the pool and hands out the ports
//     that follow, so each uses one short run of ports. Two processes hand
//     out the same port only when their runs overlap and the second checks
//     the port before the first has bound it.
package testport

import (
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	poolStart = 10000
	// The test harness's engine port ranges (internal/testenv) begin here.
	poolEndMost = 30000
	// A pool shorter than this is not worth having; see poolEnd.
	poolLeast = 1000
)

var (
	mu    sync.Mutex
	next  = -1 // the next candidate, as an even offset into the pool
	given = map[int]struct{}{}
)

// Address returns a 127.0.0.1 address that was free when it was chosen and
// that this process hands out only once.
func Address(t testing.TB) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(Port(t, "127.0.0.1")))
}

// Port returns a port that was free on ip when it was chosen, as was the port
// after it, and that this process hands out only once.
func Port(t testing.TB, ip string) int {
	t.Helper()
	pairs := (poolEnd() - poolStart) / 2
	mu.Lock()
	defer mu.Unlock()
	if next < 0 {
		next = 2 * rand.IntN(pairs)
	}
	var lastErr error
	for range pairs {
		port := poolStart + next
		next = (next + 2) % (2 * pairs)
		if _, taken := given[port]; taken {
			continue
		}
		if lastErr = free(ip, port); lastErr != nil {
			continue
		}
		if lastErr = free(ip, port+1); lastErr != nil {
			continue
		}
		given[port] = struct{}{}
		return port
	}
	if lastErr == nil {
		t.Fatalf("testport: every pair of ports in %d-%d was already handed out by this process", poolStart, poolStart+2*pairs-1)
	}
	t.Fatalf("testport: no free pair of ports on %s in %d-%d; the last one tried: %v", ip, poolStart, poolStart+2*pairs-1, lastErr)
	return 0
}

// free reports whether port can be bound on ip right now.
func free(ip string, port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return listener.Close()
}

// poolEnd is the first port not in the pool: the start of the kernel's
// ephemeral range, and no later than poolEndMost. A machine whose ephemeral
// range cannot be read, or starts so low that fewer than poolLeast ports lie
// below it, gets the whole fixed pool instead; there the kernel can assign a
// port of the pool, as it could with port 0.
func poolEnd() int {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return poolEndMost
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return poolEndMost
	}
	low, err := strconv.Atoi(fields[0])
	if err != nil || low >= poolEndMost || low < poolStart+poolLeast {
		return poolEndMost
	}
	return low
}
