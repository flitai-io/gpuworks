package gpu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestAMDChoiceAndVendorCensus(t *testing.T) {
	if got := NewFor("amd").Vendor(); got != "amd" {
		t.Fatalf("NewFor(amd).Vendor() = %q", got)
	}
	root := t.TempDir()
	writeBusCard(t, root, "0000:43:00.0", "0x1002", "0x1586", "0x030000", "amdgpu")
	writeAMDCensusCompute(t, root)
	got := (&VendorCensus{HostRoot: root}).Vendors()
	if len(got) != 1 || got[0].Vendor != "amd" || got[0].Cards != 1 {
		t.Fatalf("AMD PCI census = %#v", got)
	}
}

func TestMixedVendorAutoKeepsExistingProberPriority(t *testing.T) {
	probers := defaultProbers()
	gotOrder := make([]string, len(probers))
	for i, prober := range probers {
		gotOrder[i] = prober.Vendor()
	}
	wantOrder := []string{"hygon", "ascend", "ppu", "kunlunxin", "nvidia", "apple", "amd"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("default prober count = %v, want %v", gotOrder, wantOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Fatalf("default prober order = %v, want %v", gotOrder, wantOrder)
		}
	}

	// Model a mixed AMD/NVIDIA host without consulting the test machine's
	// devices. Candidates inherit their relative order from the real registry.
	var mixed []Prober
	for _, prober := range probers {
		if prober.Vendor() == "nvidia" || prober.Vendor() == "amd" {
			mixed = append(mixed, &choiceTestProber{vendor: prober.Vendor(), available: true})
		}
	}
	if got := firstAvailable(mixed).Vendor(); got != "nvidia" {
		t.Fatalf("mixed-vendor automatic choice = %q, want existing NVIDIA priority", got)
	}
	if got := chosenProber(probers, "amd").Vendor(); got != "amd" {
		t.Fatalf("explicit AMD choice = %q, want amd", got)
	}
}

func TestAMDAutoChoicePreservesFailedPhysicalNVIDIA(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		amdReady, nvidiaPresent bool
		want                    string
	}{
		{"NVIDIA failed and AMD compute ready", true, true, "nvidia"},
		{"NVIDIA failed and Ryzen display only", false, true, ""},
		{"AMD compute ready alone", true, false, "amd"},
		{"AMD KFD absent alone", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probers := []Prober{&choiceTestProber{vendor: "nvidia"}, &choiceTestProber{vendor: "amd", available: tc.amdReady}}
			got := firstAvailableWithBus(probers, func(v string) bool { return v == "nvidia" && tc.nvidiaPresent })
			vendor := ""
			if got != nil {
				vendor = got.Vendor()
			}
			if vendor != tc.want {
				t.Fatalf("auto vendor = %q, want %q", vendor, tc.want)
			}
			if chosenProber(probers, "amd").Vendor() != "amd" {
				t.Fatal("explicit AMD selection changed")
			}
		})
	}
}

type choiceTestProber struct {
	vendor    string
	available bool
}

func (p *choiceTestProber) Vendor() string                                 { return p.vendor }
func (p *choiceTestProber) Available() bool                                { return p.available }
func (p *choiceTestProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (p *choiceTestProber) DriverVersion(context.Context) string           { return "" }
func (p *choiceTestProber) Topology(context.Context) string                { return "" }
func (p *choiceTestProber) Metrics(context.Context) ([]Metric, error) {
	return nil, nil
}
func (p *choiceTestProber) RuntimeInjection([]int) RuntimeInjection { return RuntimeInjection{} }

func writeAMDCensusCompute(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "sys/class/kfd/kfd/topology/nodes/1/properties")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("vendor_id 4098\ndevice_id 5510\ndomain 0\nlocation_id 17152\ndrm_render_minor 135\ngfx_target_version 110501\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestVendorCensusExcludesAMDDisplayOnly(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:43:00.0", "0x1002", "0x1586", "0x030000", "amdgpu")
	writeBusCard(t, root, "0000:44:00.0", "0x10de", "0x2204", "0x030000", "nvidia")
	census := &VendorCensus{HostRoot: root}
	got := census.Vendors()
	if len(got) != 1 || got[0].Vendor != "nvidia" {
		t.Fatalf("display-only AMD offered: %#v", got)
	}
	writeAMDCensusCompute(t, root)
	got = census.Vendors()
	if len(got) != 2 {
		t.Fatalf("compute AMD not offered: %#v", got)
	}
	if err := os.Remove(filepath.Join(root, "sys/class/kfd/kfd/topology/nodes/1/properties")); err != nil {
		t.Fatal(err)
	}
	if got = census.Vendors(); len(got) != 1 || got[0].Vendor != "nvidia" {
		t.Fatalf("stale AMD compute candidate: %#v", got)
	}
}

func TestAMDAutoChoicePreservesEveryPhysicalVendor(t *testing.T) {
	for _, vendor := range []string{"nvidia", "ascend", "hygon", "ppu"} {
		t.Run(vendor, func(t *testing.T) {
			failed := &choiceTestProber{vendor: vendor}
			amd := &choiceTestProber{vendor: "amd", available: true}
			if got := firstAvailableWithBus([]Prober{failed, amd}, func(v string) bool { return v == vendor }); got != failed {
				t.Fatalf("replaced failed %s with %v", vendor, got)
			}
		})
	}
}

func TestPCIBusVendorViewsShareScanAndExcludeVFIO(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:44:00.0", "0x10de", "0x2204", "0x030000", "vfio-pci")
	all, managed := pciBusVendors(root)
	if !all("nvidia") || managed("nvidia") {
		t.Fatal("VFIO assigned to automatic management")
	}
	if err := os.RemoveAll(filepath.Join(root, "sys/bus/pci/devices")); err != nil {
		t.Fatal(err)
	}
	if !all("nvidia") {
		t.Fatal("bus unexpectedly rescanned")
	}
}
