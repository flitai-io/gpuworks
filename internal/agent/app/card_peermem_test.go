package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeSys lays out the sysfs entries the peer-memory check reads.
func fakeSys(t *testing.T, nics []string, modules []string) string {
	t.Helper()
	root := t.TempDir()
	for _, nic := range nics {
		if err := os.MkdirAll(filepath.Join(root, "class", "infiniband", nic), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, module := range modules {
		if err := os.MkdirAll(filepath.Join(root, "module", module), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPeerMemoryState(t *testing.T) {
	for _, tc := range []struct {
		name, vendor string
		nics, mods   []string
		want         string
	}{
		{"ppu loaded", accelerator.VendorPPU, []string{"mlx5_0"}, []string{"alixpu_peermem"}, proto.PeerMemoryLoaded},
		{"ppu missing", accelerator.VendorPPU, []string{"mlx5_0"}, nil, proto.PeerMemoryMissing},
		{"nvidia module is not the ppu one", accelerator.VendorPPU, []string{"mlx5_0"}, []string{"nvidia_peermem"}, proto.PeerMemoryMissing},
		{"nvidia loaded", accelerator.VendorNVIDIA, []string{"mlx5_0"}, []string{"nvidia_peermem"}, proto.PeerMemoryLoaded},
		{"no RDMA NIC", accelerator.VendorPPU, nil, nil, ""},
		{"vendor without a module", accelerator.VendorAscend, []string{"mlx5_0"}, nil, ""},
	} {
		if got := peerMemoryState(fakeSys(t, tc.nics, tc.mods), tc.vendor); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// A host whose sysfs cannot be read says nothing.
	if got := peerMemoryState(filepath.Join(t.TempDir(), "absent"), accelerator.VendorPPU); got != "" {
		t.Errorf("unreadable sysfs: %q", got)
	}
}

func TestPeerMemoryChecksWarnWithTheFix(t *testing.T) {
	old := peerMemoryRoot
	t.Cleanup(func() { peerMemoryRoot = old })
	gpus := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:01:00.0", Vendor: accelerator.VendorPPU},
		{Index: 1, UUID: "GPU-b", PCIeBDF: "0000:02:00.0", Vendor: accelerator.VendorPPU, Ownership: "foreign"},
	}
	now := time.Unix(1_800_000_000, 0)
	n := &NodeRuntime{}

	peerMemoryRoot = fakeSys(t, []string{"mlx5_0"}, nil)
	round := checkRound{}
	n.peerMemoryChecks(gpus, now, round)
	got, ok := round[accelerator.CardIdent("GPU-a", "0000:01:00.0")][accelerator.CheckPeerMemory]
	if !ok || got.status != proto.CheckWarn || got.code != proto.DoctorPeerMemoryMissing {
		t.Fatalf("missing module: %+v", got)
	}
	fields := map[string]string{}
	for _, f := range got.evidence {
		fields[f.Name] = f.Value
	}
	if fields["module"] != "alixpu_peermem" || fields["fix"] != accelerator.PeerMemoryFix(accelerator.VendorPPU) {
		t.Fatalf("evidence = %v", got.evidence)
	}
	if _, ok := round[accelerator.CardIdent("GPU-b", "0000:02:00.0")]; ok && accelerator.CardUsable("foreign") == false {
		t.Fatal("a card the platform does not own got the check")
	}

	peerMemoryRoot = fakeSys(t, []string{"mlx5_0"}, []string{"alixpu_peermem"})
	round = checkRound{}
	n.peerMemoryChecks(gpus, now, round)
	if got := round[accelerator.CardIdent("GPU-a", "0000:01:00.0")][accelerator.CheckPeerMemory]; got.status != proto.CheckPass {
		t.Fatalf("loaded module: %+v", got)
	}

	// Without an RDMA NIC there is nothing to say: the check is not applicable.
	peerMemoryRoot = fakeSys(t, nil, nil)
	round = checkRound{}
	n.peerMemoryChecks(gpus, now, round)
	if len(round) != 0 {
		t.Fatalf("no RDMA NIC: %v", round)
	}
}

// Diagnose reports what the peer-memory check warns about, with the fix, and
// says nothing where the check has nothing to say.
func TestDoctorPeerMemory(t *testing.T) {
	old := peerMemoryRoot
	t.Cleanup(func() { peerMemoryRoot = old })

	peerMemoryRoot = fakeSys(t, []string{"mlx5_0"}, nil)
	var r proto.DoctorReport
	doctorPeerMemory(&r, "PPU")
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := r.Findings[0]
	fields := map[string]string{}
	for _, e := range f.Evidence {
		fields[e.Name] = e.Value
	}
	if f.Code != proto.DoctorPeerMemoryMissing || f.Status != proto.CheckWarn || fields["module"] != "alixpu_peermem" ||
		fields["fix"] != accelerator.PeerMemoryFix(accelerator.VendorPPU) || f.Recheck != "lsmod | grep alixpu_peermem" {
		t.Fatalf("finding = %+v", f)
	}
	r.Report = renderDoctorText(r)
	if !strings.Contains(r.Report, "peer-memory module is not loaded") || !strings.Contains(r.Report, "modprobe alixpu_peermem") {
		t.Fatalf("text = %s", r.Report)
	}

	for name, root := range map[string]string{
		"loaded":      fakeSys(t, []string{"mlx5_0"}, []string{"alixpu_peermem"}),
		"no RDMA NIC": fakeSys(t, nil, nil),
	} {
		peerMemoryRoot = root
		var quiet proto.DoctorReport
		doctorPeerMemory(&quiet, "ppu")
		if len(quiet.Findings) != 0 {
			t.Fatalf("%s: findings = %+v", name, quiet.Findings)
		}
	}
}
