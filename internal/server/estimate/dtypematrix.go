package estimate

import "strings"

// DtypeMatrix is the hardware support matrix (plan §4): which compute dtypes and
// quantization paths an accelerator architecture supports.
// Data-driven and small; new archs/quant paths are one-line additions. (A YAML
// hot-update path can back this later; the shape is intentionally simple.)
type archSupport struct {
	BF16 bool
	FP8  bool
	FP4  bool
	// Quant gates quantization implementations by architecture. Missing entries
	// mean unknown quantization and are treated as best-effort allowed.
	Quant map[string]bool
	// Quirks applied by the deploy layer, surfaced here for warnings.
	Quirks []string
}

// byArch keys NVIDIA GPUs on arch family (lowercased).
var byArch = map[string]archSupport{
	"pascal":    {BF16: false, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}},
	"volta":     {BF16: false, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}},
	"turing":    {BF16: false, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}, Quirks: []string{"no bf16; float16 pinned"}},
	"ampere":    {BF16: true, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}},
	"ada":       {BF16: true, FP8: true, FP4: false, Quant: map[string]bool{"fp8": true, "fp4": false, "nvfp4": false}},
	"hopper":    {BF16: true, FP8: true, FP4: false, Quant: map[string]bool{"fp8": true, "fp4": false, "nvfp4": false}},
	"blackwell": {BF16: true, FP8: true, FP4: true, Quant: map[string]bool{"fp8": true, "fp4": true, "nvfp4": true}},
}

var byVendorArch = map[string]map[string]archSupport{
	"ascend": {
		"910b4": {BF16: true, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}},
	},
	"ppu": {
		"zw810e": {BF16: true, FP8: false, FP4: false, Quant: map[string]bool{"fp8": false, "fp4": false, "nvfp4": false}},
	},
}

// DtypeSupport reports whether the requested dtype/quant is usable on an arch,
// with a human reason when not.
func DtypeSupport(archFamily, dtype, quant string, computeCap float64) (bool, string) {
	return DtypeSupportForGPU("nvidia", "", archFamily, dtype, quant, computeCap)
}

// DtypeSupportForGPU checks dtype/quant support by vendor and architecture. For
// unknown vendors/arch IDs it returns allowed so hardware enablement can proceed
// before the matrix has full coverage.
func DtypeSupportForGPU(vendor, archID, archFamily, dtype, quant string, computeCap float64) (bool, string) {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	if vendor == "" || vendor == "nvidia" {
		sup, ok := byArch[strings.ToLower(archFamily)]
		if !ok {
			return true, "" // unknown arch: don't block (best-effort)
		}
		return checkDtypeSupport(sup, archFamily, dtype, quant)
	}
	archID = strings.ToLower(strings.TrimSpace(archID))
	if byArchID, ok := byVendorArch[vendor]; ok {
		if sup, ok := byArchID[archID]; ok {
			label := archID
			if label == "" {
				label = vendor
			}
			return checkDtypeSupport(sup, label, dtype, quant)
		}
	}
	return true, ""
}

func checkDtypeSupport(sup archSupport, label, dtype, quant string) (bool, string) {
	d := strings.ToLower(dtype)
	switch {
	case strings.Contains(d, "bf16") || strings.Contains(d, "bfloat16"):
		if !sup.BF16 {
			return false, label + " does not support bfloat16 (use float16)"
		}
	case strings.Contains(d, "fp8") || strings.Contains(d, "float8"):
		if !sup.FP8 {
			return false, label + " does not support fp8"
		}
	}
	if quant != "" {
		q := strings.ToLower(quant)
		if ok, known := sup.Quant[q]; known && !ok {
			return false, label + " does not support " + quant + " quantization"
		}
	}
	return true, ""
}

// ArchQuirks returns advisory quirks for an arch.
func ArchQuirks(archFamily string) []string {
	if s, ok := byArch[strings.ToLower(archFamily)]; ok {
		return s.Quirks
	}
	return nil
}
