package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentImageTarget is the single node-derived image-catalog query. Every
// caller that resolves a runtime image for a concrete node must go through it:
// preview, option generation, and deployment admission then cannot disagree
// because one of them forgot a node fact (the driver version once made them
// pick different images). A target vendor is useful before the user has
// selected a concrete node; node facts take precedence as soon as one exists.
// Override and TrustedOverride belong to the caller: they express a per-request
// choice, not a fact about the node.
func deploymentImageTarget(engine, targetVendor string, node *store.Node) deploy.ImageTarget {
	return deploymentImageTargetForGPU(engine, targetVendor, node, nil)
}

func deploymentImageTargetForGPU(engine, targetVendor string, node *store.Node, gpu *store.GPU) deploy.ImageTarget {
	target := deploy.ImageTarget{Engine: engine, Vendor: targetVendor}
	if node == nil {
		return target
	}
	family, _, vendor := nodeEngineTarget(node)
	if gpu != nil {
		family, _, vendor = gpuEngineTarget(node, *gpu)
	}
	target.Vendor = vendor
	// The card family lets the catalog rank a runner image built for the
	// node's chip first and one for another chip (an A3 build on a 910B
	// node) last; a node without it orders nothing.
	target.ChipFamily = family
	target.PlatformArch = node.Arch
	target.PlatformOS = "linux"
	if node.RuntimeDriver == proto.RuntimeDriverHost {
		target.PlatformOS = node.OS
	}
	target.DriverVersion = node.GPUDriver
	target.Framework = node.Framework
	target.FrameworkVersion = node.FrameworkVersion
	return target
}

// deploymentImageOptions returns canonical images a user can choose. It does
// not reuse acquisition.image.candidates: those values are registry mirrors
// for one selected image and must remain in agent pull order.
func (s *Service) deploymentImageOptions(target deploy.ImageTarget, selection templateSelection, selected string, cached *bool) []dto.ImageOption {
	type candidate struct {
		image, reason string
		trusted       bool
	}
	values := make([]candidate, 0, 8)
	if selection.Spec != nil && strings.TrimSpace(selection.Spec.Image) != "" {
		values = append(values, candidate{image: selection.Spec.Image, reason: "template_image", trusted: true})
	}
	catalogCandidates, catalogErr := imagecatalog.Candidates(s.d.Store)
	var automatic deploy.ImageResolution
	if catalogErr == nil {
		automatic = deploy.ResolveImageCatalog(target, catalogCandidates)
	} else {
		automatic = imagecatalog.Resolve(s.d.Store, target)
	}
	if automatic.Image != "" {
		reason := "catalog_match"
		if automatic.Source != "catalog" {
			reason = "engine_default"
		}
		values = append(values, candidate{image: automatic.Image, reason: reason})
	}
	// Offer every enabled catalog image that satisfies the same target-chip
	// predicate as automatic resolution. The first catalog entry above remains
	// the default; these additional entries make that choice directly editable.
	if catalogErr == nil {
		for _, catalogCandidate := range catalogCandidates {
			candidateTarget := target
			if selection.Spec == nil {
				// Without an inference template there is no engine contract to
				// preserve. Offer every runtime image compatible with the target
				// accelerator; choosing one also chooses its catalog engine.
				candidateTarget.Engine = catalogCandidate.Engine
				if _, err := deploy.ProfileForEngine(catalogCandidate.Engine); err != nil {
					continue
				}
			}
			if deploy.ImageCandidateSelectable(candidateTarget, catalogCandidate) {
				values = append(values, candidate{image: catalogCandidate.ImageRef, reason: "catalog_match", trusted: true})
			}
		}
	}
	if strings.TrimSpace(selected) != "" {
		values = append(values, candidate{image: selected, reason: "selected_image", trusted: templateOwnsImage(selection, selected)})
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]dto.ImageOption, 0, len(values))
	for _, value := range values {
		value.image = strings.TrimSpace(value.image)
		key := strings.ToLower(value.image)
		if value.image == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		checkTarget := target
		checkTarget.Override = value.image
		checkTarget.TrustedOverride = value.trusted
		var compatible bool
		if catalogErr == nil {
			compatible = !deploy.ResolveImageCatalog(checkTarget, catalogCandidates).Blocking
		} else {
			compatible = !imagecatalog.Resolve(s.d.Store, checkTarget).Blocking
		}
		option := dto.ImageOption{Image: value.image, Reason: value.reason, Compatible: compatible}
		if strings.EqualFold(value.image, strings.TrimSpace(selected)) && cached != nil {
			if *cached {
				option.Reason = "node_cached"
			}
		}
		out = append(out, option)
	}
	return out
}
