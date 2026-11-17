package seed

import (
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
)

// reportedGPUNames are names probes report on real hardware, as the agent
// sends them: nvidia-smi on gpu3, the 4090 node (with and without the
// "NVIDIA " some drivers add) and the AutoDL RTX 6000D node, the Ascend probe
// on 910B4 and 910C hosts, hy-smi on K100_AI, ppu-smi on the PPU host. The
// server stores each normalized (accelerator.NormalizeGPUName).
var reportedGPUNames = []struct {
	vendor, raw, cc string
}{
	{"nvidia", "NVIDIA GeForce RTX 4090", "8.9"},
	{"nvidia", "GeForce RTX 4090", "8.9"},
	{"nvidia", "Quadro RTX 8000", "7.5"},
	{"nvidia", "NVIDIA GeForce RTX 2080 Ti", "7.5"},
	{"nvidia", "NVIDIA RTX PRO 6000 Blackwell Server Edition", "12.0"},
	{"nvidia", "NVIDIA RTX 6000D", "12.0"},
	{"nvidia", "NVIDIA GeForce RTX 5090", "12.0"},
	{"nvidia", "NVIDIA H100 80GB HBM3", "9.0"},
	{"nvidia", "NVIDIA B200", "10.0"},
	{"nvidia", "NVIDIA GB10", "12.1"},
	{"nvidia", "NVIDIA RTX Spark N1X (6144-core Blackwell RTX GPU)", "12.1"},
	{"nvidia", "NVIDIA A100-SXM4-80GB", "8.0"},
	{"ascend", "Ascend 910B4", ""},
	{"ascend", "Ascend 910B2C", ""},
	{"ascend", "Ascend 910C", ""},
	{"ascend", "Ascend Ascend910", ""},
	{"ascend", "Ascend 310P3", ""},
	{"hygon", "K100_AI", ""},
	{"hygon", "BW1000", ""},
	{"hygon", "Hygon DCU", ""},
	{"ppu", "PPU-ZW810E", ""},
	{"ppu", "PPU-M890", ""},
}

type namePredicate struct {
	vendor, name string
}

func catalogNamePredicates(t *testing.T) []namePredicate {
	t.Helper()
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[namePredicate]bool{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.GPUs.Name != "" {
				seen[namePredicate{spec.GPUs.Vendor, spec.GPUs.Name}] = true
			}
		}
		for _, rule := range model.Unsupported {
			if rule.GPUs.Name != "" {
				seen[namePredicate{rule.GPUs.Vendor, rule.GPUs.Name}] = true
			}
		}
	}
	out := make([]namePredicate, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].vendor+out[i].name < out[j].vendor+out[j].name })
	return out
}

// Every name predicate in the catalog gives the same verdict on the stored,
// normalized name as on the name the probe reported, so normalizing names at
// ingest changes no catalog match.
func TestCatalogNamePredicatesAgreeOnNormalizedNames(t *testing.T) {
	predicates := catalogNamePredicates(t)
	if len(predicates) < 10 {
		t.Fatalf("catalog has only %d distinct name predicates", len(predicates))
	}
	for _, p := range predicates {
		predicate := chipmatch.Predicate{Vendor: p.vendor, Name: p.name}
		for _, g := range reportedGPUNames {
			raw := chipmatch.Evaluate(chipmatch.ReportedGPU(g.raw, g.vendor, "", g.cc), predicate)
			stored := chipmatch.Evaluate(chipmatch.ReportedGPU(accelerator.NormalizeGPUName(g.raw), g.vendor, "", g.cc), predicate)
			if raw.Matched != stored.Matched {
				t.Errorf("%s predicate %q: reported %q matched=%v, stored %q matched=%v",
					p.vendor, p.name, g.raw, raw.Matched, accelerator.NormalizeGPUName(g.raw), stored.Matched)
			}
		}
	}
}

// The cards the catalog has name predicates for are matched by at least one
// of them under their stored names; the cards it has none for are matched by
// none. H100 is matched since the vLLM Hopper profiles of GLM-5.3-Flash and
// Qwen3.8-Flash-Next name it. ppu-smi on the PPU host reports PPU-ZW810E (architecture Beethoven),
// which the PPU predicates name next to PPU-M890.
func TestCatalogNamePredicatesMatchStoredNames(t *testing.T) {
	predicates := catalogNamePredicates(t)
	matchedBy := func(vendor, raw, cc string) int {
		gpu := chipmatch.ReportedGPU(accelerator.NormalizeGPUName(raw), vendor, "", cc)
		n := 0
		for _, p := range predicates {
			if chipmatch.Evaluate(gpu, chipmatch.Predicate{Vendor: p.vendor, Name: p.name}).Matched {
				n++
			}
		}
		return n
	}
	for _, g := range reportedGPUNames {
		want := true
		switch g.raw {
		case "Quadro RTX 8000", "NVIDIA GeForce RTX 2080 Ti", "NVIDIA A100-SXM4-80GB",
			"Ascend Ascend910", "Ascend 310P3", "Hygon DCU":
			want = false
		}
		if got := matchedBy(g.vendor, g.raw, g.cc) > 0; got != want {
			t.Errorf("%s %q (stored %q) matched by a catalog name predicate = %v, want %v", g.vendor, g.raw, accelerator.NormalizeGPUName(g.raw), got, want)
		}
	}
}

// RTX Spark carries the same sm_121 GPU and unified memory pool as DGX Spark
// (GB10), so every single-node spec written for GB10 serves it too. The
// two-node GB10 profile is left out: it relies on the pair of Sparks being
// linked to each other, which a single RTX Spark host is not.
func TestRTXSparkMatchesEverySingleNodeGB10Spec(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	gb10 := chipmatch.ReportedGPU(accelerator.NormalizeGPUName("NVIDIA GB10"), "nvidia", "", "12.1")
	rtxSpark := chipmatch.ReportedGPU(accelerator.NormalizeGPUName("NVIDIA RTX Spark N1X (6144-core Blackwell RTX GPU)"), "nvidia", "", "12.1")
	matched := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.GPUs.Name == "" || spec.Name == tensorfoldGB10Spec {
				continue
			}
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
			if !chipmatch.Evaluate(gb10, predicate).Matched {
				continue
			}
			matched++
			if !chipmatch.Evaluate(rtxSpark, predicate).Matched {
				t.Errorf("%s spec %s matches GB10 but not RTX Spark: name %q", model.ModelID, spec.Name, spec.GPUs.Name)
			}
		}
	}
	if matched == 0 {
		t.Fatal("no catalog spec names GB10")
	}
}
