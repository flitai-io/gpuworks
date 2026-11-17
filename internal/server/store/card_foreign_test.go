package store

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func foreignJSON(t *testing.T, procs ...proto.CardProcess) string {
	t.Helper()
	b, err := json.Marshal(procs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The card of flitai-io/gpuworks#20: a GPUStack container's python3 holds
// 63982 MiB of a 143771 MiB H20-3e.
func TestForeignBytesSumsWhatEveryHolderIsKnownToHold(t *testing.T) {
	gpustack := proto.CardProcess{PID: 41235, Name: "python3", MemoryBytes: 67089334272}
	other := proto.CardProcess{PID: 5150, Name: "train.py", MemoryBytes: 2 << 30}
	for _, tc := range []struct {
		name  string
		procs string
		bytes int64
		known bool
	}{
		{"nobody", "", 0, true},
		{"the reported container", foreignJSON(t, gpustack), 67089334272, true},
		{"two processes", foreignJSON(t, gpustack, other), 67089334272 + 2<<30, true},
		{"a process the tool gives no figure for", foreignJSON(t, gpustack, proto.CardProcess{PID: 7, Name: "x"}), 0, false},
		// A Kubernetes holder is an allocation the kubelet made, not a
		// process; what it may grow to is not what it holds now.
		{"a Kubernetes holder", foreignJSON(t, proto.CardProcess{Name: "Pod ml/train (8 GiB)", MemoryBytes: 8 << 30}), 0, false},
		{"a list that cannot be read", "not json", 0, false},
		{"an empty list", "[]", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bytes, known := GPU{ForeignProcesses: tc.procs}.ForeignBytes()
			if bytes != tc.bytes || known != tc.known {
				t.Fatalf("ForeignBytes = %d, %v; want %d, %v", bytes, known, tc.bytes, tc.known)
			}
		})
	}
}

// Only a healthy card of this node's own, not stopped by an operator, whose
// holders' memory is known may be shared by memory - and that never makes
// it take whole-card work.
func TestSharesMemoryWithForeignIsTheHeldCardWhoseHoldersAreKnown(t *testing.T) {
	held := foreignJSON(t, proto.CardProcess{PID: 41235, Name: "python3", MemoryBytes: 67089334272})
	base := GPU{MemoryTotal: 143771 << 20, HealthStatus: accelerator.HealthOK, ForeignProcesses: held}
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*GPU)
		want   bool
	}{
		{"held, known, healthy", func(*GPU) {}, true},
		{"not held", func(g *GPU) { g.ForeignProcesses = "" }, false},
		{"memory unknown", func(g *GPU) { g.ForeignProcesses = foreignJSON(t, proto.CardProcess{PID: 1, Name: "x"}) }, false},
		{"faulty", func(g *GPU) { g.HealthStatus = accelerator.HealthError }, false},
		{"passed through", func(g *GPU) { g.Ownership = accelerator.CardPassedThrough }, false},
		{"stopped by an operator", func(g *GPU) { g.SchedulingStopped = true }, false},
		{"allowed by an operator", func(g *GPU) { g.SchedulingAllowedAt = &now }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := base
			tc.change(&card)
			bytes, ok := card.SharesMemoryWithForeign()
			if ok != tc.want {
				t.Fatalf("SharesMemoryWithForeign = %d, %v; want %v", bytes, ok, tc.want)
			}
			if ok && bytes != 67089334272 {
				t.Fatalf("foreign bytes = %d", bytes)
			}
		})
	}
	if base.CanTakeWork() {
		t.Fatal("a held card must still take no whole-card work")
	}
}
