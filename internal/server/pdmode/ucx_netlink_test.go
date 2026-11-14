package pdmode

import (
	"strings"
	"testing"
)

// Only a mode whose KV moves with NIXL over UCX is hit by the kernel behaviour
// that hangs UCX; Mooncake, HCCL and IPC modes never load UCX's route check.
func TestUCXNetlinkAffectsOnlyUCXModes(t *testing.T) {
	want := map[string]bool{"vllm-nixl": true, "sglang-nixl": true}
	seen := 0
	for _, engine := range []string{"vllm", "sglang"} {
		for _, name := range ForEngine(engine) {
			mode, ok := Lookup(name)
			if !ok {
				t.Fatalf("mode %q listed but not found", name)
			}
			seen++
			if got := mode.UCXNetlinkAffects(); got != want[name] {
				t.Errorf("mode %s: UCXNetlinkAffects = %t, want %t (transport %s)", name, got, want[name], mode.Transport)
			}
		}
	}
	if seen < 4 {
		t.Fatalf("only %d modes seen", seen)
	}
}

func TestUCXNetlinkFixesAreTheLiteralCommands(t *testing.T) {
	fixes := UCXNetlinkFixes()
	if len(fixes) != 2 || fixes[0].Code != UCXNetlinkFixDisableIPv6Now || fixes[1].Code != UCXNetlinkFixDisableIPv6Persist {
		t.Fatalf("fixes = %+v", fixes)
	}
	// sysctl -w takes several key=value pairs; IPv6 must go off for the whole
	// host, on single interfaces it does not stop the hang.
	if got := fixes[0].Commands; len(got) != 1 || got[0] != "sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1" {
		t.Fatalf("now = %q", got)
	}
	persist := fixes[1].Commands
	if len(persist) != 1 || !strings.Contains(persist[0], "/etc/sysctl.d/90-gpuworks-ucx.conf") || !strings.HasSuffix(persist[0], "sudo sysctl --system") ||
		!strings.Contains(persist[0], "net.ipv6.conf.all.disable_ipv6=1") || !strings.Contains(persist[0], "net.ipv6.conf.default.disable_ipv6=1") {
		t.Fatalf("persist = %q", persist)
	}
	// Mutating a returned list must not change the next one.
	fixes[0].Commands[0] = "changed"
	if UCXNetlinkFixes()[0].Commands[0] == "changed" {
		t.Fatal("the fixes are shared state")
	}
}
