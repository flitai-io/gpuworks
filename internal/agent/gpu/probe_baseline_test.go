package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The identity a PPU grant is checked against is the one Probe listed: a
// card that has taken another's index since then is refused.
func TestPPUProbePublishesTheIdentitiesAGrantIsCheckedAgainst(t *testing.T) {
	const listed = "GPU-011ef111-c280-0918-0000-000020462378"
	const other = "GPU-019ec20c-0692-0214-0000-0000c0e5b553"
	bin := t.TempDir()
	row := "8, PPU ZW810, zw810, " + listed + ", 9, 00000000:3B:00.0, 49140, 0, 0, 40, 60, 1.2.3, 0, 0, 0, No, 0, No, No, 0, 0, 0"
	script := "#!/bin/sh\ncase \"$1\" in\n--query-ppu=index,name,*) echo '" + row + "' ;;\n*) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "ppu-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	p := &PPUProber{hostRoot: t.TempDir()}
	gpus, err := p.Probe(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].Index != 8 || gpus[0].UUID != listed {
		t.Fatalf("Probe() = %+v, %v", gpus, err)
	}
	resolve := func(index int, _ string) (string, error) { return fmt.Sprintf("/dev/alixpu_ppu%d", index+1), nil }
	target := runtime.TargetNamed(runtime.DriverDocker)
	if inj := p.runtimeInjection([]int{8}, "8, "+listed, resolve, target); inj.UnsupportedReason != "" {
		t.Fatalf("the card Probe listed is refused: %s", inj.UnsupportedReason)
	}
	inj := p.runtimeInjection([]int{8}, "8, "+other, resolve, target)
	if !strings.Contains(inj.UnsupportedReason, "changed from UUID "+listed) {
		t.Fatalf("another card at the probed index is granted: %+v", inj)
	}
}

// The same for Hygon, whose identity is the card's PCI address.
func TestHygonProbePublishesTheIdentitiesAGrantIsCheckedAgainst(t *testing.T) {
	inventory := readHygonFixture(t)
	p := testHygonProber(t, inventory)
	p.lookPath = func(string) (string, error) { return "/opt/dtk/bin/hy-smi", nil }
	p.hostRoot = t.TempDir()
	gpus, err := p.Probe(context.Background())
	if err != nil || len(gpus) != 2 || gpus[1].PCIeBDF != "0000:67:00.0" {
		t.Fatalf("Probe() = %+v, %v", gpus, err)
	}
	if inj := p.RuntimeInjection([]int{1}); inj.UnsupportedReason != "" {
		t.Fatalf("the card Probe listed is refused: %s", inj.UnsupportedReason)
	}
	swapped := strings.NewReplacer("19:00.0", "67:00.0", "67:00.0", "19:00.0").Replace(inventory)
	p.command = func(context.Context, ...string) (string, error) { return swapped, nil }
	inj := p.RuntimeInjection([]int{1})
	if !strings.Contains(inj.UnsupportedReason, "changed from PCI 0000:67:00.0 to 0000:19:00.0") {
		t.Fatalf("another card at the probed index is granted: %+v", inj)
	}
}
