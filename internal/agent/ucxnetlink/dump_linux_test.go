//go:build linux

package ucxnetlink

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHostDumpOnThisKernel sends the real dump to this host's kernel and
// reports what it answers. It needs only a netlink socket, which every Linux
// has; it fails when the dump is not well formed, and never when the host is
// or is not affected, which is the finding. Run it with
// `go test -v -run TestHostDumpOnThisKernel ./internal/agent/ucxnetlink`.
func TestHostDumpOnThisKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	datagrams, err := HostDump(ctx)
	if err != nil {
		t.Skipf("netlink is not usable in this environment: %v", err)
	}
	res, err := Analyze(datagrams)
	if err != nil {
		t.Fatalf("the kernel's answer is not a finished route dump: %v", err)
	}
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	sizes := make([]int, len(datagrams))
	for i, d := range datagrams {
		sizes[i] = len(d)
	}
	t.Logf("kernel %s: %d IPv6 route messages in %d datagrams (sizes %v), affected by the UCX netlink hang: %t",
		strings.TrimSpace(string(release)), res.Routes, res.Datagrams, sizes, res.Affected)
}

func TestHostDumpHonoursAnAlreadyEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := HostDump(ctx); err == nil && time.Since(start) > time.Second {
		t.Fatal("HostDump ignored an ended context")
	}
}
