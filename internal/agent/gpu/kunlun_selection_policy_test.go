package gpu

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Automatic selection manages one vendor, not an aggregate of backends. Native
// Kunlun precedes CUDA-compatible tools; explicit preference remains decisive
// even when that vendor's CLI has failed. Other vendors keep their old order.
func TestKunlunAutomaticAndExplicitSelectionPolicy(t *testing.T) {
	for _, available := range []bool{true, false} {
		for _, nvidiaOnBus := range []bool{true, false} {
			p, _ := kunlunFixture(t)
			if !available {
				p.command = func(context.Context, ...string) (string, error) { return "", errors.New("driver failed") }
			}
			nv := availabilityProber{vendor: accelerator.VendorNVIDIA, available: true}
			probers := []Prober{p, nv}
			got := firstAvailableWithBus(probers, func(vendor string) bool {
				return vendor == accelerator.VendorKunlunxin || (vendor == accelerator.VendorNVIDIA && nvidiaOnBus)
			})
			want := accelerator.VendorNVIDIA
			if available || !nvidiaOnBus {
				want = accelerator.VendorKunlunxin
			}
			if got == nil || got.Vendor() != want {
				t.Fatalf("native=%v bus=%v: got %v", available, nvidiaOnBus, got)
			}
			if chosenProber(probers, accelerator.VendorNVIDIA) != nv || chosenProber(probers, accelerator.VendorKunlunxin) != p {
				t.Fatal("explicit preference did not override automatic order")
			}
		}
	}
}

// With no physical bus facts, the old CLI order still applies. A vendor whose
// tool cannot answer is not invented from an executable name alone.
func TestKunlunNoPCIFactsDoesNotOverrideWorkingNVIDIA(t *testing.T) {
	p, _ := kunlunFixture(t)
	p.command = func(context.Context, ...string) (string, error) { return "", errors.New("no native driver") }
	nv := availabilityProber{vendor: accelerator.VendorNVIDIA, available: true}
	got := firstAvailableWithBus([]Prober{p, nv}, func(string) bool { return false })
	if got.Vendor() != accelerator.VendorNVIDIA {
		t.Fatalf("fallback=%s", got.Vendor())
	}
}
