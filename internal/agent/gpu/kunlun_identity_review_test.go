package gpu

import (
	"os"
	"strings"
	"testing"
)

// Synthetic VMD addresses exercise the same-domain collision that motivated
// accelerator.NormalizeBDF. Injection must consult the full sysfs address.
func TestKunlunLargePCIDomainIsNotTruncated(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.Replace(*raw, "00000000:07:00.0", "00010000:09:00.0", -1)
	oldRead := p.readFile
	p.readFile = func(path string) ([]byte, error) {
		switch path {
		case "/sys/bus/pci/devices/10000:09:00.0/vendor":
			return []byte("0x2057"), nil
		case "/sys/bus/pci/devices/10000:09:00.0/kunlun/xpu0/dev":
			return []byte("195:0"), nil
		case "/sys/bus/pci/devices/10000:09:00.0/numa_node":
			return []byte("-1"), nil
		}
		return oldRead(path)
	}
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 2 || cards[0].PCIeBDF != "10000:09:00.0" || cards[1].PCIeBDF != "0000:09:00.0" || !cards[0].Healthy || !cards[1].Healthy {
		t.Fatalf("%+v %v", cards, err)
	}
	// Name the container runtime: the driver is not what this test is about,
	// and Kunlun refuses rather than assume one.
	if inj := p.RuntimeInjectionForDriver([]int{0, 1}, "docker"); inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	p.readFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "10000:09:00.0") {
			return nil, os.ErrNotExist
		}
		return oldRead(path)
	}
	if inj := p.RuntimeInjectionForDriver([]int{0}, "docker"); inj.UnsupportedReason != "Kunlun PCI vendor cannot be verified" || len(inj.DockerArgs) != 0 {
		t.Fatalf("domain-zero fallback: %+v", inj)
	}
}

func TestKunlunMissingUUIDRetainsIndexMetricsButNotLaunchPermission(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.Replace(*raw, "<uuid>GPU-fixture-1</uuid>", "<uuid></uuid>", 1)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 2 || metrics[0].Index != 0 || metrics[0].UUID != "" || metrics[0].Values["gpuworks_gpu_memory_total_bytes"] != 96<<30 {
		t.Fatalf("metrics lost: %+v %v", metrics, err)
	}
	// The named cause matters: refusing for want of a runtime driver would
	// pass this assertion without ever reaching the identity check.
	if inj := p.RuntimeInjectionForDriver([]int{0}, "docker"); inj.UnsupportedReason != "invalid or duplicate Kunlun device selection" || len(inj.DockerArgs) != 0 {
		t.Fatalf("unidentified card was authorized: %+v", inj)
	}
}

func TestKunlunAvailableRequiresExplicitBrand(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.ReplaceAll(*raw, "KUNLUNXIN", "INTEL")
	if p.Available() {
		t.Fatal("a non-Kunlun brand was claimed by the Kunlun prober")
	}
}

func TestKunlunInjectionRequiresProbeBaselineAndManagesWholeInventory(t *testing.T) {
	p, _ := kunlunFixture(t)
	inj := p.RuntimeInjectionForDriver([]int{1}, "docker")
	if inj.UnsupportedReason != "Kunlun injection requires a successful probe baseline" || inj.Failure == nil || len(inj.DockerArgs) != 0 || inj.ManagedSelection != nil {
		t.Fatalf("injection without probe baseline: %+v", inj)
	}

	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	inj = p.RuntimeInjectionForDriver([]int{1}, "docker")
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if inj.ManagedSelection == nil || strings.Join(inj.ManagedSelection.DevicePaths, ",") != "/dev/xpu0,/dev/xpu1,/dev/xpuctrl" {
		t.Fatalf("managed device paths = %+v", inj.ManagedSelection)
	}
	if len(inj.ManagedSelection.Env) != 1 || inj.ManagedSelection.Env[0] != "XPU_VISIBLE_DEVICES" {
		t.Fatalf("managed environment = %+v", inj.ManagedSelection.Env)
	}
	if inj.Env["XPU_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("Docker must use compact container-logical XPU indexes: %+v", inj.Env)
	}
}
