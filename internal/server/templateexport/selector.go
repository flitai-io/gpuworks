package templateexport

import (
	"fmt"
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// Selector is the gpus selector of a spec verified on card: its vendor and a
// name pattern that matches this card's model and no other. One deployment
// proves one card model, so the export claims no more; widening it is a
// decision for whoever reviews the template. Stored card names are already
// normalized (accelerator.NormalizeGPUName), so the whole name is anchored.
func Selector(card GPUFact) (inferencecatalog.GPUSelector, error) {
	name := strings.TrimSpace(card.Name)
	vendor := accelerator.InferVendor(card.Vendor, name, "", card.ComputeCap)
	if name == "" || vendor == "" {
		return inferencecatalog.GPUSelector{}, Unavailable(ReasonNoGPUFacts, nil)
	}
	selector := inferencecatalog.GPUSelector{Vendor: vendor, Name: "^" + regexp.QuoteMeta(name) + "$"}
	reported := chipmatch.ReportedGPU(name, card.Vendor, "", card.ComputeCap)
	if verdict := chipmatch.Evaluate(reported, chipmatch.Predicate{Vendor: selector.Vendor, Name: selector.Name}); !verdict.Matched {
		return inferencecatalog.GPUSelector{}, fmt.Errorf("selector for %q does not match the card: %s", name, verdict.Reason)
	}
	return selector, nil
}

// weightSource is the one weight source a deployment without a template
// read: either a registered local model identity, or the repository it was
// downloaded from pinned to the commit every copy resolved. Local identities
// refer to registered artifacts, never the host path or an invented download.
func weightSource(facts ModelFacts) (inferencecatalog.WeightSource, error) {
	provider := strings.ToLower(strings.TrimSpace(facts.Source))
	if provider == "local" {
		return inferencecatalog.WeightSource{Provider: provider, RepoID: strings.TrimSpace(facts.RepoID), Subfolder: strings.TrimSpace(facts.Subfolder)}, nil
	}
	if provider != "huggingface" && provider != "modelscope" {
		return inferencecatalog.WeightSource{}, Unavailable(ReasonWeightsNotPublic, map[string]any{"source": provider})
	}
	revision := ""
	for _, resolved := range facts.ResolvedRevisions {
		resolved = strings.TrimSpace(resolved)
		if resolved == "" {
			continue
		}
		if revision != "" && resolved != revision {
			return inferencecatalog.WeightSource{}, Unavailable(ReasonRevisionsDiffer, map[string]any{"revisions": facts.ResolvedRevisions})
		}
		revision = resolved
	}
	if !modelrepo.IsCommitRevision(revision) {
		return inferencecatalog.WeightSource{}, Unavailable(ReasonRevisionUnpinned, map[string]any{"revision": revision})
	}
	return inferencecatalog.WeightSource{Provider: provider, RepoID: strings.TrimSpace(facts.RepoID), Revision: revision, Subfolder: strings.TrimSpace(facts.Subfolder)}, nil
}
