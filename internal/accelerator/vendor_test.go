package accelerator

import (
	"reflect"
	"testing"
)

func TestNormalizeVendorCanonicalizesKnownAliases(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"nvidia", VendorNVIDIA},
		{"NVIDIA Corporation", VendorNVIDIA},
		{" CUDA ", VendorNVIDIA},
		{"amd", VendorAMD},
		{"ROCm", VendorAMD},
		{"gfx942", VendorAMD},
		{"hygon", VendorHygon},
		{"DCU", VendorHygon},
		{"haiguang", VendorHygon},
		{"ascend", VendorAscend},
		{"Huawei", VendorAscend},
		{"npu", VendorAscend},
		{"ppu", VendorPPU},
		{"thead", VendorPPU},
		{"t-head", VendorPPU},
		{"alibaba", VendorPPU},
		{"beethoven", VendorPPU},
		{"apple", VendorApple},
		{"Apple", VendorApple},
		{"metal", VendorApple},
		{"Apple M4 Pro", VendorApple},
		// Only the brand word itself names Apple: a vendor label that merely
		// contains the letters must not.
		{"pineapple", ""},
		{"", ""},
		{"   ", ""},
		{"intel-gaudi", ""},
	}
	for _, tt := range tests {
		if got := NormalizeVendor(tt.in); got != tt.want {
			t.Fatalf("NormalizeVendor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeVendorTriesEachArgumentInOrder(t *testing.T) {
	if got := NormalizeVendor("", "  ", "Ascend 910B4"); got != VendorAscend {
		t.Fatalf("NormalizeVendor fallthrough = %q", got)
	}
	if got := NormalizeVendor("nvidia", "ascend"); got != VendorNVIDIA {
		t.Fatalf("NormalizeVendor must take the first identified value, got %q", got)
	}
	if got := NormalizeVendor("intel", "moore-threads"); got != "" {
		t.Fatalf("NormalizeVendor of only-unknown values = %q, want empty", got)
	}
}

// VendorKey keeping an unrecognized label instead of discarding it is what
// stops an unknown accelerator from matching every catalog entry; NormalizeVendor
// returning "" for the same input is what lets a caller detect "not identified".
func TestVendorKeyKeepsUnknownVendorsVerbatim(t *testing.T) {
	if got := VendorKey("  Moore Threads  "); got != "moore threads" {
		t.Fatalf("VendorKey(unknown) = %q", got)
	}
	if got := VendorKey("CUDA"); got != VendorNVIDIA {
		t.Fatalf("VendorKey(known) = %q", got)
	}
	if got := VendorKey(""); got != "" {
		t.Fatalf("VendorKey(empty) = %q", got)
	}
	if got := NormalizeVendor("Moore Threads"); got != "" {
		t.Fatalf("NormalizeVendor(unknown) = %q, want empty", got)
	}
}

func TestKnownVendorsIsTheClosedSetNormalizeVendorProduces(t *testing.T) {
	known := KnownVendors()
	if !reflect.DeepEqual(known, []string{VendorNVIDIA, VendorAMD, VendorAscend, VendorPPU, VendorHygon, VendorApple, VendorKunlunxin}) {
		t.Fatalf("KnownVendors() = %v", known)
	}
	set := make(map[string]struct{}, len(known))
	for _, vendor := range known {
		set[vendor] = struct{}{}
		if got := NormalizeVendor(vendor); got != vendor {
			t.Fatalf("NormalizeVendor(%q) = %q; a canonical name must normalize to itself", vendor, got)
		}
	}
	for _, alias := range []string{"cuda", "rocm", "gfx942", "dcu", "haiguang", "huawei", "npu", "thead", "t-head", "alibaba", "beethoven", "metal"} {
		got := NormalizeVendor(alias)
		if _, ok := set[got]; !ok {
			t.Fatalf("NormalizeVendor(%q) = %q, which is outside KnownVendors()", alias, got)
		}
	}
}

func TestFrameworkVendorDerivesTheClosedFrameworkSet(t *testing.T) {
	for in, want := range map[string]string{
		"cuda":  VendorNVIDIA,
		"CUDA":  VendorNVIDIA,
		" rocm": VendorAMD,
		"cann":  VendorAscend,
		"dtk":   VendorHygon,
		"hggc":  VendorPPU,
		"metal": VendorApple,
		"":      "",
		"cuda2": "",
	} {
		if got := FrameworkVendor(in); got != want {
			t.Fatalf("FrameworkVendor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"amd64":   "x86_64",
		"x86_64":  "x86_64",
		"x86-64":  "x86_64",
		"arm64":   "arm64",
		"AArch64": "arm64",
		" ":       "",
		"riscv64": "riscv64",
	} {
		if got := NormalizeArch(in); got != want {
			t.Fatalf("NormalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDomesticAndVendorCatalogImageDifferOnHygon(t *testing.T) {
	for _, vendor := range []string{VendorAscend, VendorPPU, "huawei", "beethoven"} {
		if !IsDomestic(vendor) {
			t.Fatalf("IsDomestic(%q) = false", vendor)
		}
		if !RequiresVendorCatalogImage(vendor) {
			t.Fatalf("RequiresVendorCatalogImage(%q) = false", vendor)
		}
	}
	// Hygon runs a ROCm-derived stack, so it is not a domestic partition
	// target, yet its DTK images share neither the CUDA nor the upstream ROCm
	// engine default: exactly the case the two predicates disagree on.
	if IsDomestic(VendorHygon) {
		t.Fatal("IsDomestic(hygon) = true")
	}
	if !RequiresVendorCatalogImage(VendorHygon) {
		t.Fatal("RequiresVendorCatalogImage(hygon) = false")
	}
	// Apple's engines are macOS environment packages: it is no partition
	// target, and a Linux CUDA/ROCm default image cannot run on it at all.
	if IsDomestic(VendorApple) {
		t.Fatal("IsDomestic(apple) = true")
	}
	if !RequiresVendorCatalogImage(VendorApple) {
		t.Fatal("RequiresVendorCatalogImage(apple) = false")
	}
	for _, vendor := range []string{VendorAMD, "rocm"} {
		if IsDomestic(vendor) || !RequiresVendorCatalogImage(vendor) {
			t.Fatalf("ROCm must select a target image: %s", vendor)
		}
	}
	for _, vendor := range []string{VendorNVIDIA, "", "moore-threads"} {
		if IsDomestic(vendor) {
			t.Fatalf("IsDomestic(%q) = true", vendor)
		}
		if RequiresVendorCatalogImage(vendor) {
			t.Fatalf("RequiresVendorCatalogImage(%q) = true", vendor)
		}
	}
}

func TestInferVendorPrefersTheReportedLabel(t *testing.T) {
	if got := InferVendor("Huawei", "NVIDIA H100", "Hopper", "9.0"); got != VendorAscend {
		t.Fatalf("InferVendor must trust the vendor label, got %q", got)
	}
	// An accelerator this build cannot name must stay itself rather than be
	// relabeled NVIDIA by the name search.
	if got := InferVendor("Moore Threads", "MTT S4000", "", ""); got != "moore threads" {
		t.Fatalf("InferVendor(unknown label) = %q", got)
	}
}

func TestInferVendorFromGPUFields(t *testing.T) {
	tests := []struct {
		name string
		gpu  string
		arch string
		cc   string
		want string
	}{
		{name: "nvidia compute capability", gpu: "NVIDIA H100", arch: "Hopper", cc: "9.0", want: VendorNVIDIA},
		{name: "ascend model", gpu: "Huawei Ascend 910B4", arch: "910B4", want: VendorAscend},
		{name: "amd gfx arch", gpu: "AMD Instinct MI300X", arch: "gfx942", want: VendorAMD},
		{name: "hygon dcu", gpu: "Hygon DCU", arch: "gfx928", want: VendorHygon},
		{name: "ppu model", gpu: "PPU-ZW810E", arch: "Beethoven", want: VendorPPU},
		{name: "apple silicon", gpu: "Apple M1 Max", arch: "M1 Max", want: VendorApple},
		{name: "unidentifiable falls back to nvidia", gpu: "Accelerator X", want: VendorNVIDIA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InferVendor("", tt.gpu, tt.arch, tt.cc); got != tt.want {
				t.Fatalf("InferVendor(%q,%q,%q) = %q, want %q", tt.gpu, tt.arch, tt.cc, got, tt.want)
			}
		})
	}
}

// Every vendor the product knows has a brand spelling to fall back on when a
// client has no translation; anything else has none to invent.
func TestVendorBrandCoversEveryKnownVendor(t *testing.T) {
	for _, vendor := range KnownVendors() {
		if brand := VendorBrand(vendor); brand == "" {
			t.Errorf("%s has no brand spelling", vendor)
		}
	}
	if got := VendorBrand("NVIDIA Corporation"); got != "NVIDIA" {
		t.Errorf("a spelling NormalizeVendor accepts = %q", got)
	}
	for _, unknown := range []string{"", "   ", "mystery"} {
		if got := VendorBrand(unknown); got != "" {
			t.Errorf("VendorBrand(%q) = %q, want none", unknown, got)
		}
	}
}

func TestKunlunVendorIdentity(t *testing.T) {
	if FrameworkVendor("xre") != VendorKunlunxin || NormalizeVendor("Kunlunxin") != VendorKunlunxin || RequiresVendorCatalogImage(VendorKunlunxin) == false {
		t.Fatal("Kunlun runtime identity")
	}
	if NormalizeVendor("Intel XPU") != "" || InferVendor("Intel XPU", "", "", "") == VendorKunlunxin {
		t.Fatal("Intel XPU identified as Kunlun")
	}
	if InferVendor("", "P800 PCIe", "KL3", "10.2") != VendorKunlunxin {
		t.Fatal("compatibility CUDA field overrode Kunlun")
	}
	if arch := GPUArch(VendorKunlunxin, "kl3", ""); arch.ID != "kl3" || arch.Family != "kl3" {
		t.Fatalf("architecture: %+v", arch)
	}
}
