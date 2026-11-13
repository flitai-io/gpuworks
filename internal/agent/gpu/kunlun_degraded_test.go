package gpu

import (
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestKunlunDegradesTelemetryWithoutWeakeningIdentityChecks(t *testing.T) {
	for _, field := range []struct {
		from, to, code, status, evidence string
		schedulable                      bool
	}{
		{"<uuid>GPU-fixture-1</uuid>", "<uuid></uuid>", accelerator.HealthInventoryInvalid, accelerator.HealthError, "card_validation", false},
		{"<total>98304 MiB</total>", "<total>N/A</total>", accelerator.HealthInventoryInvalid, accelerator.HealthError, "fb_memory_usage.total", false},
		{"<dram_uncorrectable>0</dram_uncorrectable>", "<dram_uncorrectable>N/A</dram_uncorrectable>", accelerator.HealthTelemetryUnavailable, accelerator.HealthWarning, "ecc_errors.volatile.dram_uncorrectable", true},
	} {
		t.Run(field.to, func(t *testing.T) {
			p, raw := kunlunFixture(t)
			changed := strings.Replace(*raw, field.from, field.to, 1)
			if changed == *raw {
				t.Fatalf("fixture has no %q", field.from)
			}
			*raw = changed
			cards, err := p.Probe(t.Context())
			if err != nil || len(cards) != 2 || !cards[1].Healthy || !p.Available() {
				t.Fatal(cards, err)
			}
			card := cards[0]
			if card.Healthy != field.schedulable || card.HealthCode != field.code || card.HealthStatus != field.status || !strings.Contains(card.HealthMessage, field.evidence) {
				t.Fatalf("health fields: %+v, want healthy=%v code=%q status=%q evidence=%q", card, field.schedulable, field.code, field.status, field.evidence)
			}
			injection := p.RuntimeInjectionForDriver([]int{0}, "docker")
			if field.schedulable {
				if injection.UnsupportedReason != "" || len(injection.DockerArgs) == 0 {
					t.Fatalf("ECC warning denied injection: %+v", injection)
				}
			} else if injection.UnsupportedReason != "invalid or duplicate Kunlun device selection" || len(injection.DockerArgs) != 0 {
				t.Fatalf("invalid identity authorized: %+v", injection)
			}
			if reason := p.RuntimeInjectionForDriver([]int{1}, "docker").UnsupportedReason; reason != "" {
				t.Fatal(reason)
			}
		})
	}
}

func TestKunlunMissingCLIRowUsesPCIRoster(t *testing.T) {
	p, raw := kunlunFixture(t)
	p.hostRoot = pciHost(t,
		pciFunction{bdf: "0000:07:00.0", vendor: "0x2057", device: "0x3686", class: "0x078000", driver: "kunlun"},
		pciFunction{bdf: "0000:09:00.0", vendor: "0x2057", device: "0x3686", class: "0x078000", driver: "kunlun"})
	start := strings.LastIndex(*raw, "<xpu ")
	end := strings.Index((*raw)[start:], "</xpu>") + start + len("</xpu>")
	*raw = (*raw)[:start] + (*raw)[end:]
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 2 || !cards[0].Healthy || cards[1].HealthCode != accelerator.HealthDriverInitFailed {
		t.Fatal(cards, err)
	}
}

func TestKunlunRuntimePlatformCheckIsHermetic(t *testing.T) {
	for _, host := range []accelerator.HostRuntime{{OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "amd64"}, {OS: "windows", Arch: "amd64"}} {
		p, _ := kunlunFixture(t)
		p.platform = func() accelerator.HostRuntime { return host }
		p.command = nil
		p.lookPath = func(string) (string, error) { t.Fatal("unsupported host invoked CLI"); return "", os.ErrNotExist }
		if got := p.RuntimeInjection([]int{0}); !strings.Contains(got.UnsupportedReason, "runtime_unsupported") {
			t.Fatal(got)
		}
	}
}

func TestKunlunCapturedIdleInventory(t *testing.T) {
	p, raw := kunlunFixture(t)
	data, err := os.ReadFile("testdata/kunlun-p800-idle-captured.xml")
	if err != nil {
		t.Fatal(err)
	}
	*raw = string(data)
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 8 {
		t.Fatal(cards, err)
	}
	procs, err := p.ComputeProcesses(t.Context())
	if err != nil || len(procs) != 0 {
		t.Fatal(procs, err)
	}
}

// Synthetic corruption: two differently formatted minors identify one device.
func TestKunlunDuplicateNumericMinorDoesNotAuthorizeEitherCard(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.Replace(*raw, "<minor_number>1</minor_number>", "<minor_number>00</minor_number>", 1)
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 2 || cards[0].Healthy || cards[1].Healthy {
		t.Fatal(cards, err)
	}
	if got := p.RuntimeInjectionForDriver([]int{0}, "docker"); got.UnsupportedReason == "" {
		t.Fatal("duplicate numeric device identity authorized")
	}
}

func TestKunlunZeroMemoryProcessStillReportsOccupation(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.Replace(*raw, "<processes/>", "<processes><process_info><pid>1234</pid><process_name>worker</process_name><used_memory>0 MiB</used_memory></process_info></processes>", 1)
	processes, err := p.ComputeProcesses(t.Context())
	if err != nil || len(processes) != 1 || processes[0].PID != 1234 || processes[0].PCIeBDF != "0000:07:00.0" || processes[0].MemoryBytes != 0 {
		t.Fatal(processes, err)
	}
}

// Synthetic missing-row output without PCI sysfs: retain known identities,
// surface the count mismatch, and never invent the missing card's identity.
func TestKunlunIncompleteInventoryWithoutSysfsIsVisible(t *testing.T) {
	p, raw := kunlunFixture(t)
	p.hostRoot = t.TempDir()
	start := strings.LastIndex(*raw, "<xpu ")
	end := strings.Index((*raw)[start:], "</xpu>") + start + len("</xpu>")
	*raw = (*raw)[:start] + (*raw)[end:]
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards=%+v err=%v", cards, err)
	}
	card := cards[0]
	if !card.Healthy || card.HealthCode != accelerator.HealthInventoryIncomplete || card.HealthStatus != accelerator.HealthWarning || !strings.Contains(card.HealthMessage, "attached_xpus=2") || !strings.Contains(card.HealthMessage, "inventory_rows=1") {
		t.Fatalf("incomplete inventory lost its diagnostic or valid card: %+v", card)
	}
	if inj := p.RuntimeInjectionForDriver([]int{0}, "docker"); inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	*raw = strings.Replace(*raw, "<dram_uncorrectable>0</dram_uncorrectable>", "<dram_uncorrectable>1</dram_uncorrectable>", 1)
	cards, err = p.Probe(t.Context())
	if err != nil || len(cards) != 1 || cards[0].Healthy || cards[0].HealthCode != accelerator.HealthMemoryUncorrectable {
		t.Fatalf("warning hid ECC error: %+v %v", cards, err)
	}
}
