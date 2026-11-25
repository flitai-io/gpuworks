package accelerator

import "strings"

// Canonical vendor names. A vendor is the accelerator runtime family a GPU
// belongs to; it is the key every layer uses to match hardware facts against
// catalog data, so it must be spelled the same everywhere.
const (
	VendorNVIDIA    = "nvidia"
	VendorAMD       = "amd"
	VendorAscend    = "ascend"
	VendorPPU       = "ppu"
	VendorHygon     = "hygon"
	VendorApple     = "apple"
	VendorKunlunxin = "kunlunxin"
)

// FrameworkVendor returns the canonical vendor that owns a framework's
// runtime (cuda→nvidia, rocm→amd, cann→ascend, dtk→hygon, hggc→ppu,
// metal→apple).
// Inference images declare a framework rather than a vendor, and a
// self-reported vendor must not be trusted, so every layer derives the
// vendor from this single table. An unknown framework yields the empty
// string.
func FrameworkVendor(framework string) string {
	framework = strings.ToLower(strings.TrimSpace(framework))
	for _, spec := range vendorSpecs {
		for _, own := range spec.frameworks {
			if own == framework {
				return spec.id
			}
		}
	}
	return ""
}

// KnownVendors returns every canonical vendor name, in a stable order. It is
// the closed set NormalizeVendor can produce, and the set every vendor-keyed
// table in the product (image catalog metadata, the static hardware catalog,
// the dtype matrix) must stay within.
func KnownVendors() []string {
	ids := make([]string, len(vendorSpecs))
	for i, spec := range vendorSpecs {
		ids[i] = spec.id
	}
	return ids
}

// NormalizeVendor returns the canonical vendor name used on the wire. Each
// argument is tried in order, so a caller may pass a GPU-level label followed
// by a node-level one and get the first that identifies a vendor. An
// unrecognized label yields the empty string: callers that must not treat an
// unknown accelerator as "any vendor" want VendorKey instead.
func NormalizeVendor(values ...string) string {
	for _, value := range values {
		v := strings.ToLower(strings.TrimSpace(value))
		for _, spec := range vendorsByPrecedence {
			// A definition that says no name is never the answer.
			if spec.named != nil && spec.named(v) {
				return spec.id
			}
		}
	}
	return ""
}

// VendorKey returns a comparison key for a vendor label of unknown provenance.
// A recognized label collapses to its canonical name; an unrecognized one is
// lowercased and returned verbatim rather than discarded, so two spellings of
// the same unknown vendor still compare equal and an accelerator this build
// does not know about is never silently treated as compatible with everything.
//
// Use NormalizeVendor when an unrecognized label must be reported as such (the
// empty string); use VendorKey only to compare labels drawn from catalog data
// against observed hardware.
func VendorKey(value string) string {
	if canonical := NormalizeVendor(value); canonical != "" {
		return canonical
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func NormalizeArch(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "amd64", "x86_64", "x86-64":
		return "x86_64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// IsDomestic reports whether a vendor is a domestic accelerator, the axis that
// groups hardware in the catalog. Partition support is a separate capability.
//
// It is deliberately narrower than RequiresVendorCatalogImage: Hygon runs a
// ROCm-derived stack and is catalogued separately, yet it still
// cannot inherit a CUDA/ROCm engine default image.
func IsDomestic(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.domestic
}

// HasRuntimeAssetBundles reports whether the vendor's partition backend ships
// a gpuworks-built shared object embedded in the agent binary
// (internal/assets/bin/runtime-assets, build/runtime-assets/manifest.json),
// the way InstallDomesticRuntimeAssets extracts and installs it on the node.
// It is deliberately broader than IsDomestic: AMD is not a domestic
// accelerator (RequiresVendorCatalogImage still covers that), but its dGPU
// shim (libvrocm.so) is bundled the same way Ascend's and PPU's are. NVIDIA's
// HAMi-core is embedded by a separate, unconditional mechanism, and Hygon's
// dGPU backend composes host-installed driver libraries in place instead of
// shipping its own, so neither belongs here.
func HasRuntimeAssetBundles(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.assetBundles
}

// RequiresVendorCatalogImage identifies runtimes that cannot safely inherit a
// CUDA/ROCm engine default image and must be given a vendor-specific catalog
// entry instead. This is deliberately vendor-level: model and SKU
// compatibility remain catalog data, so a new chip generation never requires
// another deployment branch.
//
// It differs from IsDomestic by including Hygon, whose DTK images share
// neither the CUDA nor the upstream ROCm engine defaults, AMD whose ROCm
// image must not fall back to the CUDA default, and Apple, whose
// engines are macOS environment packages rather than Linux images at all.
func RequiresVendorCatalogImage(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.ownCatalogImage
}

// VendorBrand writes a canonical vendor the way the vendor writes it. It is
// the English fallback for a client that has no translation of its own; the
// vendor id stays the key everywhere else.
func VendorBrand(vendor string) string {
	spec, _ := vendorSpecOf(vendor)
	return spec.brand
}
