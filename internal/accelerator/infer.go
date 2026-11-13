package accelerator

import "strings"

// InferVendor derives a vendor key from whatever a GPU row happens to carry.
// The reported vendor label wins when it identifies anything at all; otherwise
// the GPU name, architecture family and compute capability are searched for
// product-line markers, because several probers report a model name without a
// vendor field. A row that names no recognizable accelerator is reported as
// NVIDIA: that is the only vendor whose engine defaults are safe to inherit,
// and every vendor whose defaults are not (RequiresVendorCatalogImage) is
// detectable from its product names.
//
// Unlike NormalizeVendor, an unrecognized but non-empty vendor label is kept
// verbatim (VendorKey semantics) rather than triggering the name search: an
// accelerator this build does not know about must not be relabeled NVIDIA.
func InferVendor(vendor, name, arch, computeCapability string) string {
	if v := VendorKey(vendor); v != "" {
		return v
	}
	hay := strings.ToLower(strings.TrimSpace(name + " " + arch + " " + computeCapability))
	switch {
	case strings.Contains(hay, "kunlun"), strings.EqualFold(strings.TrimSpace(arch), "kl3"):
		return VendorKunlunxin
	case strings.Contains(hay, "ascend"), strings.Contains(hay, "910b"), strings.Contains(hay, "910c"), strings.Contains(hay, "310p"),
		strings.Contains(hay, "davinci"), strings.Contains(hay, "npu"):
		return VendorAscend
	case strings.Contains(hay, "hygon"), strings.Contains(hay, "dcu"), strings.HasPrefix(hay, "k100"), strings.HasPrefix(hay, "bw1"):
		return VendorHygon
	case strings.Contains(hay, "ppu"), strings.Contains(hay, "beethoven"), strings.Contains(hay, "zw810e"):
		return VendorPPU
	case strings.Contains(hay, "apple"), strings.Contains(hay, "metal"):
		return VendorApple
	case strings.Contains(hay, "amd"), strings.Contains(hay, "rocm"), strings.Contains(hay, "gfx"), strings.Contains(hay, "instinct"), strings.Contains(hay, "mi300"):
		return VendorAMD
	case strings.Contains(hay, "nvidia"), strings.Contains(hay, "rtx"), strings.Contains(hay, "tesla"), strings.Contains(hay, "a100"), strings.Contains(hay, "h100"), strings.Contains(hay, "l40"), computeCapability != "":
		return VendorNVIDIA
	default:
		return VendorNVIDIA
	}
}

// GPUVendor is the vendor of one GPU a node reports, together with the node's
// framework (the runtime its probe found). It is InferVendor over the GPU row,
// except that a row carrying no vendor, name or architecture at all - older
// probes persisted only the node's framework - takes the framework's vendor
// instead of defaulting to NVIDIA. Every server layer that asks which vendor
// a node's card is (scheduling, placement checks, previews) asks this.
func GPUVendor(vendor, name, arch, computeCapability, framework string) string {
	inferred := InferVendor(vendor, name, arch, computeCapability)
	if inferred == VendorNVIDIA && strings.TrimSpace(vendor) == "" && strings.TrimSpace(name) == "" && strings.TrimSpace(arch) == "" {
		if fromFramework := FrameworkVendor(framework); fromFramework != "" {
			return fromFramework
		}
	}
	return inferred
}
