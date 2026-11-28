package accelerator

import (
	"net/url"
	"slices"
	"testing"
	"time"
)

// TestCardSpecsAreTraceable holds every vendor's card specs to the rules a
// figure shown as a card's peak must meet: each figure is vouched for by a
// vendor document, the name was read off real hardware, and no precision is
// left unaccounted for, so an absent rate can only mean the card lacks it.
func TestCardSpecsAreTraceable(t *testing.T) {
	today := time.Now().UTC().Format(time.DateOnly)
	for _, vendor := range vendorSpecs {
		seen := map[string]bool{}
		for _, card := range vendor.cardSpecs {
			where := vendor.id + "/" + card.Name
			if card.Name == "" || NormalizeGPUName(card.Name) != card.Name {
				t.Errorf("%s: name must be the stored form, got %q (stored as %q)", where, card.Name, NormalizeGPUName(card.Name))
			}
			if seen[card.Name] {
				t.Errorf("%s: listed twice", where)
			}
			seen[card.Name] = true
			if card.NameSeen == "" {
				t.Errorf("%s: no record of the hardware the name was read off", where)
			}

			covered := map[SpecFact]bool{}
			for _, source := range card.Sources {
				u, err := url.Parse(source.URL)
				if err != nil || u.Scheme != "https" || u.Host == "" {
					t.Errorf("%s: source URL %q is not an https address", where, source.URL)
				}
				if source.Title == "" || source.Version == "" {
					t.Errorf("%s: source %s lacks a title or version", where, source.URL)
				}
				if d, err := time.Parse(time.DateOnly, source.Retrieved); err != nil || source.Retrieved > today {
					t.Errorf("%s: source %s retrieved %q, want a past YYYY-MM-DD (%v, %v)", where, source.URL, source.Retrieved, d, err)
				}
				if len(source.Covers) == 0 {
					t.Errorf("%s: source %s vouches for nothing", where, source.URL)
				}
				for _, fact := range source.Covers {
					covered[fact] = true
				}
			}
			need := func(fact SpecFact, why string) {
				if !covered[fact] {
					t.Errorf("%s: %s, but no source covers %s", where, why, fact)
				}
			}

			stated := map[Precision]int{}
			for _, peak := range card.Peaks {
				stated[peak.Precision]++
				if peak.Stated <= 0 {
					t.Errorf("%s: %s peak %v is not positive", where, peak.Precision, peak.Stated)
				}
			}
			for _, p := range card.Unsupported {
				stated[p]++
			}
			for _, p := range Precisions {
				if stated[p] != 1 {
					t.Errorf("%s: %s is stated %d times between Peaks and Unsupported, want once", where, p, stated[p])
				}
				delete(stated, p)
			}
			for p := range stated {
				t.Errorf("%s: %q is not a precision", where, p)
			}
			if len(card.Peaks)+len(card.Unsupported) > 0 {
				need(FactCompute, "it states rates")
			}
			if card.MemoryBandwidthGBps < 0 {
				t.Errorf("%s: negative memory bandwidth", where)
			}
			if card.MemoryBandwidthGBps > 0 {
				need(FactMemoryBandwidth, "it states a memory bandwidth")
			}
			for _, link := range card.Interconnects {
				if link.Kind != InterconnectNVLink && link.Kind != InterconnectPCIe {
					t.Errorf("%s: unknown interconnect %q", where, link.Kind)
				}
				if link.GBps <= 0 {
					t.Errorf("%s: %s bandwidth %v is not positive", where, link.Kind, link.GBps)
				}
			}
			if len(card.Interconnects) > 0 {
				need(FactInterconnect, "it states interconnects")
				need(FactInterconnectDirection, "it says which direction its interconnect figures count")
			}
		}
	}
}

func TestCardSpecOfMatchesTheStoredNameExactly(t *testing.T) {
	vendor := vendorSpec{id: "spec-test", cardSpecs: []CardSpec{{Name: "X100 80GB"}}}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"X100 80GB", true},
		{"  X100   80GB ", true},
		{"X100", false},
		{"X100 80GB PCIe", false},
		{"x100 80gb", false},
	} {
		got, ok := vendor.cardSpec(tc.name)
		if ok != tc.want || (ok && got.Name != "X100 80GB") {
			t.Errorf("cardSpec(%q) = %q, %v; want found=%v", tc.name, got.Name, ok, tc.want)
		}
	}
	if _, ok := CardSpecOf("no-such-vendor", nvidiaCardSpecs[0].Name); ok {
		t.Error("an unknown vendor found a card")
	}
}

func TestCardSpecOfStripsTheBrandWordTheToolPrints(t *testing.T) {
	for _, card := range nvidiaCardSpecs {
		if _, ok := CardSpecOf(VendorNVIDIA, "NVIDIA "+card.Name); !ok {
			t.Errorf("CardSpecOf(nvidia, %q) not found", "NVIDIA "+card.Name)
		}
		if _, ok := CardSpecOf(VendorAMD, card.Name); ok {
			t.Errorf("CardSpecOf(amd, %q) found an NVIDIA card", card.Name)
		}
	}
}

func TestCardSpecOfReturnsACopy(t *testing.T) {
	card, ok := CardSpecOf(VendorNVIDIA, nvidiaCardSpecs[0].Name)
	if !ok {
		t.Fatal("not found")
	}
	card.Peaks[0].Stated = -1
	card.Sources[0].Covers[0] = "changed"
	again, _ := CardSpecOf(VendorNVIDIA, nvidiaCardSpecs[0].Name)
	if again.Peaks[0].Stated == -1 || again.Sources[0].Covers[0] == "changed" {
		t.Fatal("a caller's change reached the definition")
	}
}

func TestPeakDenseHalvesASparseFigure(t *testing.T) {
	if got := (Peak{Stated: 1979, Sparse: true}).Dense(); got != 989.5 {
		t.Errorf("sparse 1979 → dense %v, want 989.5", got)
	}
	if got := (Peak{Stated: 989.4}).Dense(); got != 989.4 {
		t.Errorf("dense 989.4 → %v, want unchanged", got)
	}
	card := CardSpec{Peaks: []Peak{{Precision: PrecisionBF16, Stated: 100, Sparse: true}}, Unsupported: []Precision{PrecisionFP8}}
	if got, ok := card.Peak(PrecisionBF16); !ok || got != 50 {
		t.Errorf("Peak(bf16) = %v, %v; want 50, true", got, ok)
	}
	if _, ok := card.Peak(PrecisionFP8); ok {
		t.Error("Peak(fp8) found a rate the card does not have")
	}
}

func TestOnlyIntegerPrecisionsCountOperations(t *testing.T) {
	for _, p := range Precisions {
		if p.Integer() != (p == PrecisionINT8) {
			t.Errorf("%s.Integer() = %v", p, p.Integer())
		}
	}
	if !slices.Contains(Precisions, DefaultPrecision) {
		t.Errorf("default precision %s is not one of %v", DefaultPrecision, Precisions)
	}
}
