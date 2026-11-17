package deploy

import (
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ImageTarget describes the runtime image lookup dimensions. Vendor is the
// accelerator runtime family, not the cloud provider.
type ImageTarget struct {
	Engine       string
	Vendor       string
	PlatformArch string // host image architecture, for example amd64 or arm64
	PlatformOS   string // execution OS: linux containers or the host environment OS
	// ChipFamily is the node card's family (accelerator.Arch.Family: 910b,
	// 910c, 310p). Empty when unknown, which excludes nothing.
	ChipFamily       string
	Framework        string
	FrameworkVersion string
	DriverVersion    string // raw node-reported driver string; normalized by accelerator compat checks
	// Override is the per-deployment image override. It wins over every table
	// default because it is an explicit user choice.
	Override string
	// TrustedOverride marks an immutable image selected by an audited inference
	// catalog spec. It is still an override of the separate image catalog, but
	// it must not be presented as an unverified user-supplied image.
	TrustedOverride bool
}

// ImageCandidate is the persistence-neutral shape used by the catalog
// resolver. The server/store layer converts catalog rows into this shape so
// the deploy package does not depend on a database package. Every field here
// is a selection input: the engine, framework (and its version, the driver
// compatibility fact), host arch, source identity, and lifecycle flags.
type ImageCandidate struct {
	Engine           string
	EngineVersion    string
	Framework        string
	FrameworkVersion string
	Arch             string
	// ChipFamily is the chip family the image is built for, derived from its
	// reference (accelerator.RunnerImageChipFamily). Empty for a generic image,
	// which runs on every family.
	ChipFamily string
	ImageRef   string
	Source     string
	Deprecated bool
	Enabled    bool
	// RollingTag marks a tag whose content moves (latest, nightly, ...). It
	// may be chosen by hand but is never picked automatically: two replicas
	// of one deployment could otherwise run different builds.
	RollingTag bool
	CreatedAt  time.Time
}

type ImageResolution struct {
	Image    string
	Source   string
	Warnings []messages.Warning
	// Blocking means the selected image is known to be incompatible with the
	// target runtime. Unknown custom images remain advisory because a private
	// image can be valid even when it is absent from the local catalog.
	Blocking bool
}

// ResolveImage chooses the container image for an engine/runtime combination.
// Unknown combinations fall back to the engine profile default.
func ResolveImage(t ImageTarget) string {
	if strings.TrimSpace(t.Override) != "" {
		return strings.TrimSpace(t.Override)
	}
	engine := normalizeKey(t.Engine)
	if engine == "" {
		engine = DefaultEngine
	}
	// Profile defaults are CUDA-oriented. Non-NVIDIA vendors must obtain their
	// default from the image catalog; returning a CUDA image here would silently
	// create a cross-vendor deployment.
	if vendor := accelerator.VendorKey(t.Vendor); vendor != "" && vendor != accelerator.VendorNVIDIA {
		return ""
	}
	if p, err := ProfileForEngine(engine); err == nil {
		return p.DefaultImage
	}
	if p, err := ProfileForEngine(DefaultEngine); err == nil {
		return p.DefaultImage
	}
	return "vllm/vllm-openai:latest"
}

// IsEngineDefaultImage reports whether ref is the configured default image
// for engine. A default image may be absent from the catalog (for example on a
// fresh installation), but it still belongs to the engine command contract.
func IsEngineDefaultImage(engine, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	p, err := ProfileForEngine(engine)
	return err == nil && strings.EqualFold(strings.TrimSpace(p.DefaultImage), ref)
}

// ResolveImageCatalog applies the automatic image precedence rules. An
// explicit override wins unless its known execution platform cannot run on the target.
// Automatic selection hard-filters on engine, framework, framework-implied
// vendor, host architecture and chip family (an image built for a known other
// family, such as the A3 build on a 910B node, is out), then orders survivors deterministically:
// driver-compatible framework versions first (an unusable driver version only
// sinks a candidate with a warning, it never eliminates it), then framework
// version descending, engine version descending, and finally catalog creation
// time. When the catalog has no compatible entry the legacy resolver remains
// the final fallback so older databases and incomplete manifests keep working.
func ResolveImageCatalog(t ImageTarget, candidates []ImageCandidate) ImageResolution {
	if override := strings.TrimSpace(t.Override); override != "" {
		// An audited template cannot make an incompatible binary executable.
		if candidate, ok := findCandidateByRef(candidates, override); ok && !imagePlatformCompatible(t, candidate) {
			return ImageResolution{Image: override, Source: "override", Blocking: true, Warnings: []messages.Warning{
				messages.NewWarning(messages.WarningImageOverrideIncompatible, map[string]any{"image": override}),
			}}
		}
		if t.TrustedOverride {
			return ImageResolution{Image: override, Source: "catalog-spec"}
		}
		res := ImageResolution{Image: override, Source: "override"}
		if candidate, ok := findCandidateByRef(candidates, override); ok {
			if !candidateCompatible(t, candidate) {
				res.Warnings = append(res.Warnings, messages.NewWarning(messages.WarningImageOverrideIncompatible, map[string]any{"image": override}))
			}
		} else if IsEngineDefaultImage(t.Engine, override) {
			if engineDefaultImageIncompatible(t, override) {
				res.Warnings = append(res.Warnings, messages.NewWarning(messages.WarningImageEngineDefaultIncompatible, map[string]any{"image": override}))
			}
		} else {
			res.Warnings = append(res.Warnings, messages.NewWarning(messages.WarningImageOverrideUnverified, map[string]any{"image": override}))
		}
		// Framework and driver compatibility remain advisory for an explicit
		// choice; the known execution platform above is a hard constraint.
		return res
	}

	// VendorKey, not NormalizeVendor: targetVendor is compared against the
	// vendor each candidate's framework implies, and an accelerator this build
	// does not recognize must not collapse to "" and thereby match every
	// candidate.
	targetVendor := accelerator.VendorKey(t.Vendor)
	// The score only orders eligible candidates: a framework version the driver
	// may not support sinks a candidate but never removes it, so it still wins
	// when nothing better is eligible.
	bestScore := -1 << 30
	var best *ImageCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.RollingTag || !imageCandidateEligible(t, *candidate) {
			continue
		}
		compatible, score := catalogCompatibility(t, targetVendor, *candidate)
		if !compatible || score < bestScore {
			continue
		}
		if best != nil && score == bestScore && compareCandidateVersions(*candidate, *best) <= 0 {
			continue
		}
		bestScore = score
		best = candidate
	}
	if best != nil {
		res := ImageResolution{Image: strings.TrimSpace(best.ImageRef), Source: "catalog"}
		if level, reason := accelerator.CheckDriverCompat(t.DriverVersion, best.Framework, best.FrameworkVersion); level != "" {
			res.Warnings = append(res.Warnings, messages.NewWarning(messages.WarningImageDriverCompatibility, map[string]any{
				"framework": best.Framework, "framework_version": best.FrameworkVersion, "level": string(level), "reason": reason,
			}))
		}
		return res
	}
	if targetVendor != "" && targetVendor != accelerator.VendorNVIDIA {
		return ImageResolution{Source: "catalog", Blocking: true, Warnings: []messages.Warning{messages.NewWarning(messages.WarningImageCatalogNoMatchDomestic, nil)}}
	}
	return ImageResolution{Image: ResolveImage(t), Source: "fallback", Warnings: []messages.Warning{messages.NewWarning(messages.WarningImageCatalogNoMatch, nil)}}
}

// ImageCandidateSelectable reports whether an enabled catalog image may be
// offered for the target. Engine, framework, the vendor the framework implies
// and host architecture decide it; the chip family only orders the automatic
// pick (chipFamilyWeight), so every image stays a manual choice.
func ImageCandidateSelectable(t ImageTarget, candidate ImageCandidate) bool {
	return imageCandidateEligible(t, candidate)
}

func imageCandidateEligible(t ImageTarget, candidate ImageCandidate) bool {
	if !candidate.Enabled || candidate.Deprecated || normalizeKey(candidate.ImageRef) == "" {
		return false
	}
	targetEngine := normalizeKey(t.Engine)
	if targetEngine == "" {
		targetEngine = DefaultEngine
	}
	if normalizeKey(candidate.Engine) != targetEngine {
		return false
	}
	if (strings.EqualFold(candidate.Source, "community") || strings.EqualFold(candidate.Source, "custom")) && strings.TrimSpace(candidate.Framework) == "" {
		// An unclassified user image remains available through the expert
		// custom-image path, but it is not safe to recommend in quick deploy.
		return false
	}
	return candidateCompatible(t, candidate)
}
func engineDefaultImageIncompatible(t ImageTarget, ref string) bool {
	// VendorKey, not NormalizeVendor: an unrecognized accelerator must not be
	// told a CUDA engine default is fine for it just because this build cannot
	// name its vendor.
	vendor := accelerator.VendorKey(t.Vendor)
	return vendor != "" && vendor != accelerator.VendorNVIDIA && IsEngineDefaultImage(t.Engine, ref)
}

func findCandidateByRef(candidates []ImageCandidate, ref string) (ImageCandidate, bool) {
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.ImageRef), strings.TrimSpace(ref)) {
			return candidate, true
		}
	}
	return ImageCandidate{}, false
}

func candidateCompatible(t ImageTarget, candidate ImageCandidate) bool {
	targetEngine := normalizeKey(t.Engine)
	if targetEngine == "" {
		targetEngine = DefaultEngine
	}
	if normalizeKey(candidate.Engine) != targetEngine {
		return false
	}
	compatible, _ := catalogCompatibility(t, accelerator.VendorKey(t.Vendor), candidate)
	return compatible
}

// catalogCompatibility reports whether a candidate satisfies the hard runtime
// constraints, and its ordering weight. The only weight inputs are the declared
// framework, the driver compatibility of its framework version and the chip
// family the image is built for (chipFamilyWeight): every other
// dimension (vendor labels, official source, arch specificity, engine-version
// pinning) was either derivable from the framework or never set by a caller,
// and their hidden bonuses made the automatic pick flip when one call site
// forgot a node fact. Ordering beyond this weight is the deterministic
// version/creation comparison in compareCandidateVersions.
func catalogCompatibility(t ImageTarget, targetVendor string, candidate ImageCandidate) (bool, int) {
	score := 0
	if !imagePlatformCompatible(t, candidate) {
		return false, 0
	}
	if framework := normalizeKey(candidate.Framework); framework != "" {
		if targetFramework := normalizeKey(t.Framework); targetFramework != "" && framework != targetFramework {
			return false, 0
		}
		if frameworkVendor := accelerator.FrameworkVendor(framework); frameworkVendor != "" && targetVendor != "" && frameworkVendor != targetVendor {
			return false, 0
		}
		score += 20
	} else if normalizeKey(t.Framework) != "" {
		return false, 0
	}
	if candidate.FrameworkVersion != "" {
		if level, _ := accelerator.CheckDriverCompat(t.DriverVersion, candidate.Framework, candidate.FrameworkVersion); level == accelerator.CompatWarning {
			score -= 20
		} else if level == accelerator.CompatUnknown {
			score -= 4
		}
	}
	return true, score + chipFamilyWeight(candidate.ChipFamily, t.ChipFamily)
}

// imagePlatformCompatible checks binary execution facts independently of
// engine/framework advice. Catalog Metal artifacts are macOS host environment
// packages; the other known accelerator frameworks ship Linux containers.
// Unknown frameworks and unset target facts cannot prove a mismatch.
func imagePlatformCompatible(t ImageTarget, candidate ImageCandidate) bool {
	if !archMatches(candidate.Arch, t.PlatformArch) {
		return false
	}
	os := ""
	if vendor := accelerator.FrameworkVendor(candidate.Framework); vendor != "" {
		os = "linux"
		if vendor == accelerator.VendorApple {
			os = "darwin"
		}
	}
	return os == "" || normalizeKey(t.PlatformOS) == "" || os == normalizeKey(t.PlatformOS)
}

// chipFamilyWeight orders images built for a chip family (the 910B and A3
// builds of vllm-ascend): one built for the node's own family first, a generic
// one next, and one built for another known family last, below a driver
// warning. It only orders: no image is removed, so a manual choice of any of
// them stands. When either family is unknown nothing is weighed.
func chipFamilyWeight(candidate, target string) int {
	candidate = normalizeKey(candidate)
	target = normalizeKey(target)
	switch {
	case candidate == "" || target == "":
		return 0
	case candidate == target:
		return 5
	default:
		return -40
	}
}

func archMatches(candidate, platform string) bool {
	candidate = normalizeArch(candidate)
	platform = normalizeArch(platform)
	return candidate == "" || candidate == "multi" || candidate == "any" || platform == "" || candidate == platform
}

func normalizeArch(value string) string {
	switch accelerator.NormalizeArch(value) {
	case "x86_64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return normalizeKey(value)
	}
}

func compareCandidateVersions(a, b ImageCandidate) int {
	if cmp := compareVersion(a.FrameworkVersion, b.FrameworkVersion); cmp != 0 {
		return cmp
	}
	if cmp := compareVersion(a.EngineVersion, b.EngineVersion); cmp != 0 {
		return cmp
	}
	if a.CreatedAt.Before(b.CreatedAt) {
		return -1
	}
	if a.CreatedAt.After(b.CreatedAt) {
		return 1
	}
	return 0
}

func compareVersion(a, b string) int {
	return compareVersionParts(parseVersion(a), parseVersion(b))
}

func parseVersion(value string) []int {
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "v"))
	if value == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '_' })
	result := make([]int, 0, len(parts))
	for _, part := range parts {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		result = append(result, n)
	}
	return result
}

func compareVersionParts(a, b []int) int {
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func normalizeKey(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
