package accelerator

import "testing"

func TestNormalizeGPUName(t *testing.T) {
	cases := map[string]string{
		"NVIDIA GeForce RTX 4090":    "GeForce RTX 4090",
		"GeForce RTX 4090":           "GeForce RTX 4090",
		"  NVIDIA  Quadro RTX  8000": "Quadro RTX 8000",
		"nvidia H100 80GB HBM3":      "H100 80GB HBM3",
		"NVIDIA GeForce RTX 2080 Ti": "GeForce RTX 2080 Ti",
		"Tesla V100-SXM2-32GB":       "Tesla V100-SXM2-32GB",
		"Ascend 910B4":               "910B4",
		"Ascend 910C":                "910C",
		// What the Ascend probe reported for 910C before it knew the layout:
		// the brand word goes, the glued one stays.
		"Ascend Ascend910":    "Ascend910",
		"Ascend910B4":         "Ascend910B4",
		"Hygon DCU":           "DCU",
		"K100_AI":             "K100_AI",
		"PPU-ZW810E":          "PPU-ZW810E",
		"AMD Instinct MI300X": "Instinct MI300X",
		"Apple M4 Pro":        "M4 Pro",
		"Apple M1 Max":        "M1 Max",
		// A name that is only the brand is all there is.
		"NVIDIA":  "NVIDIA",
		"":        "",
		"  \t\n ": "",
	}
	for raw, want := range cases {
		if got := NormalizeGPUName(raw); got != want {
			t.Errorf("NormalizeGPUName(%q) = %q, want %q", raw, got, want)
		}
		// Normalizing a normalized name changes nothing, so a migration that
		// already ran and a later ingest agree.
		if got := NormalizeGPUName(NormalizeGPUName(raw)); got != NormalizeGPUName(raw) {
			t.Errorf("NormalizeGPUName is not idempotent on %q: %q", raw, got)
		}
	}
}

func TestGPUArch(t *testing.T) {
	cases := []struct {
		name               string
		vendor, archID, cc string
		want               Arch
	}{
		{name: "turing", vendor: "nvidia", cc: "7.5", want: Arch{ID: "sm_75", Family: "turing"}},
		{name: "volta", vendor: "nvidia", cc: "7.0", want: Arch{ID: "sm_70", Family: "volta"}},
		{name: "ampere", vendor: "nvidia", cc: "8.6", want: Arch{ID: "sm_86", Family: "ampere"}},
		{name: "ada", vendor: "nvidia", cc: "8.9", want: Arch{ID: "sm_89", Family: "ada"}},
		{name: "hopper", vendor: "nvidia", cc: "9.0", want: Arch{ID: "sm_90", Family: "hopper"}},
		{name: "blackwell datacenter", vendor: "nvidia", cc: "10.0", want: Arch{ID: "sm_100", Family: "blackwell"}},
		{name: "blackwell consumer", vendor: "NVIDIA", cc: "12.0", want: Arch{ID: "sm_120", Family: "blackwell"}},
		{name: "pascal", vendor: "nvidia", cc: "6.1", want: Arch{ID: "sm_61", Family: "pascal"}},
		// A stored sm_XY never overrides the capability it was derived from.
		{name: "stale arch id", vendor: "nvidia", archID: "sm_80", cc: "8.9", want: Arch{ID: "sm_89", Family: "ada"}},
		{name: "unknown generation", vendor: "nvidia", cc: "3.5", want: Arch{ID: "sm_35"}},
		{name: "no capability", vendor: "nvidia", want: Arch{}},
		{name: "sm_XY without a capability", vendor: "nvidia", archID: "sm_90", want: Arch{ID: "sm_90", Family: "hopper"}},
		{name: "sm_120 without a capability", vendor: "nvidia", archID: "sm_120", want: Arch{ID: "sm_120", Family: "blackwell"}},
		{name: "not an sm_XY", vendor: "nvidia", archID: "sm_x", want: Arch{ID: "sm_x"}},
		{name: "vendor inferred from capability", cc: "8.0", want: Arch{ID: "sm_80", Family: "ampere"}},
		{name: "910B4", vendor: "ascend", archID: "910B4", want: Arch{ID: "910B4", Family: "910b"}},
		{name: "910B2C", vendor: "ascend", archID: "910B2C", want: Arch{ID: "910B2C", Family: "910b"}},
		{name: "910C", vendor: "ascend", archID: "910C", want: Arch{ID: "910C", Family: "910c"}},
		{name: "910C vendor inferred", archID: "910C", want: Arch{ID: "910C", Family: "910c"}},
		{name: "310P3", vendor: "ascend", archID: "310P3", want: Arch{ID: "310P3", Family: "310p"}},
		{name: "first-generation 910 name", vendor: "ascend", archID: "Ascend910", want: Arch{ID: "Ascend910"}},
		{name: "950PR", vendor: "ascend", archID: "950PR", want: Arch{ID: "950PR"}},
		{name: "K100_AI", vendor: "hygon", archID: "K100AI", want: Arch{ID: "K100AI", Family: "k100"}},
		{name: "ambiguous K100", vendor: "hygon", archID: "K100", want: Arch{ID: "K100", Family: "k100"}},
		{name: "Z100", vendor: "hygon", archID: "Z100L", want: Arch{ID: "Z100L", Family: "z100"}},
		{name: "BW1000", vendor: "hygon", archID: "BW1000", want: Arch{ID: "BW1000", Family: "bw1000"}},
		{name: "BW", vendor: "hygon", archID: "BW", want: Arch{ID: "BW", Family: "bw"}},
		{name: "BW1100", vendor: "hygon", archID: "BW1100", want: Arch{ID: "BW1100", Family: "bw1100"}},
		{name: "unknown hygon", vendor: "hygon", want: Arch{}},
		{name: "ZW810E", vendor: "ppu", archID: "ZW810E", want: Arch{ID: "ZW810E", Family: "beethoven"}},
		{name: "unknown ppu", vendor: "ppu", archID: "ZW900", want: Arch{ID: "ZW900"}},
		{name: "amd", vendor: "amd", archID: "gfx942", want: Arch{ID: "gfx942"}},
		{name: "M1 Max", vendor: "apple", archID: "M1 Max", want: Arch{ID: "M1 Max", Family: "m1"}},
		{name: "M4", vendor: "apple", archID: "M4", want: Arch{ID: "M4", Family: "m4"}},
		{name: "M5 Pro", vendor: "apple", archID: "M5 Pro", want: Arch{ID: "M5 Pro", Family: "m5"}},
		{name: "not an apple generation", vendor: "apple", archID: "Max M1", want: Arch{ID: "Max M1"}},
		{name: "unknown apple", vendor: "apple", want: Arch{}},
	}
	for _, c := range cases {
		if got := GPUArch(c.vendor, c.archID, c.cc); got != c.want {
			t.Errorf("%s: GPUArch(%q, %q, %q) = %+v, want %+v", c.name, c.vendor, c.archID, c.cc, got, c.want)
		}
	}
}

func TestStoredArchIDDropsWhatTheCapabilityDerives(t *testing.T) {
	cases := []struct{ vendor, archID, cc, want string }{
		{"nvidia", "sm_89", "8.9", ""},
		{"NVIDIA", "sm_89", "8.9", ""},
		// Without a capability the reported identity is the only fact.
		{"nvidia", "sm_89", "", "sm_89"},
		{"ascend", " 910B4 ", "", "910B4"},
		{"hygon", "K100", "", "K100"},
		{"ppu", "ZW810E", "", "ZW810E"},
	}
	for _, c := range cases {
		if got := StoredArchID(c.vendor, c.archID, c.cc); got != c.want {
			t.Errorf("StoredArchID(%q, %q, %q) = %q, want %q", c.vendor, c.archID, c.cc, got, c.want)
		}
	}
}

// A family is written the way its vendor writes it: a name is a word, a part
// number is upper case. The rule covers every family the derivation above can
// produce, so adding one needs nothing here.
func TestArchBrandWritesAFamilyTheWayItsVendorDoes(t *testing.T) {
	for family, want := range map[string]string{
		"turing":    "Turing",
		"ada":       "Ada",
		"blackwell": "Blackwell",
		"beethoven": "Beethoven",
		"910b":      "910B",
		"910c":      "910C",
		"310p":      "310P",
		"k100":      "K100",
		"bw1100":    "BW1100",
		"":          "",
		"  ":        "",
	} {
		if got := ArchBrand(family); got != want {
			t.Errorf("ArchBrand(%q) = %q, want %q", family, got, want)
		}
	}
	// Every family the derivation produces has a spelling; none comes back
	// empty or unchanged-but-lowercase.
	for _, arch := range []Arch{
		GPUArch(VendorNVIDIA, "", "7.5"),
		GPUArch(VendorAscend, "910B4", ""),
		GPUArch(VendorAscend, "910C", ""),
		GPUArch(VendorHygon, "K100_AI", ""),
		GPUArch(VendorPPU, "PPU-ZW810E", ""),
	} {
		if brand := ArchBrand(arch.Family); brand == "" || brand == arch.Family {
			t.Errorf("family %q brands as %q", arch.Family, brand)
		}
	}
}

// The predicate decides how a card's memory is sized and how much of it must
// stay outside an engine's share, so a near miss is expensive in both
// directions: the whole model token has to match.
//
// It also has to answer for both spellings of the same card. The agent asks
// with the probe's raw name and the server with the stored, normalized one -
// the rule this replaced required the brand word and would have said "GB10"
// was a discrete card.
func TestUnifiedMemoryMatchesTheWholeModelToken(t *testing.T) {
	for name, want := range map[string]bool{
		"NVIDIA GB10":             true,
		"GB10":                    true,
		"gb10":                    true,
		"nvidia  gb10":            true,
		"NVIDIA GB10 (DGX Spark)": true,
		"NVIDIA GB100":            false,
		"NVIDIA GB102":            false,
		"NVIDIA GB10X":            false,
		"NVIDIA GB200":            false,
		"NVIDIA B200":             false,
		"NVIDIA GeForce RTX 5090": false,
		"GeForce RTX 5090":        false,
		"Tesla GB10":              false,
		"910B4":                   false,
		// RTX Spark: raw probe name, stored name, and bare family name.
		"NVIDIA RTX Spark N1X":                               true,
		"NVIDIA RTX Spark N1X (6144-core Blackwell RTX GPU)": true,
		"RTX Spark N1X (6144-core Blackwell RTX GPU)":        true,
		"RTX Spark":     true,
		"rtx spark n1x": true,
		// Every other RTX card stays discrete: the second token is the guard.
		"RTX 5090":        false,
		"RTX 4090":        false,
		"RTX A6000":       false,
		"RTX 6000 Ada":    false,
		"NVIDIA RTX 4090": false,
		"RTX":             false,
		// Apple silicon, by generation, raw and stored.
		"Apple M1 Max": true,
		"M1 Max":       true,
		"Apple M4":     true,
		"M5 Pro":       true,
		"Apple M10":    false,
		"Quadro M2000": false,
		"Tesla M60":    false,
		"":             false,
		"   ":          false,
	} {
		if got := UnifiedMemory(name); got != want {
			t.Errorf("UnifiedMemory(%q) = %v, want %v", name, got, want)
		}
	}
}
