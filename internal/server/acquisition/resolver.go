// Package acquisition resolves deployment weight and image acquisition choices
// from an immutable snapshot of server facts. It never performs I/O or mutates
// state; callers may safely expose its result from evaluate endpoints.
package acquisition

import (
	"fmt"
	"sort"
)

type WeightSource struct {
	Provider  string
	RepoID    string
	Revision  string
	Suggested bool
	// DisplayName names the provider for a person when the provider name alone
	// does not identify it. An HTTP directory's provider is always "url"; the
	// host it is served from is the only thing that tells two of them apart,
	// and nothing else in this struct carries it.
	DisplayName string
}
type WeightFacts struct {
	Providers []WeightSource
	// Present lists every storage the target nodes can read that already
	// holds the model, ready or not; Targets lists the storages a node may
	// save the model into when it has to fetch it.
	Present []StoragePresence
	Targets []StorageTarget
}

// StoragePresence is one storage that holds the model, as the target nodes see it.
type StoragePresence struct {
	StorageID   uint
	StorageName string
	StorageType string
	SizeBytes   int64
	// OnNode: the files are already on the target node's disk (a local
	// storage, or an nfs_cache storage that has cached them there).
	OnNode bool
	// CacheBytes is what an nfs_cache storage still has to copy to the node.
	CacheBytes int64
	// Unavailable says why the storage cannot serve every target node now;
	// such a presence is listed but never recommended.
	Unavailable string
}

// StorageTarget is a storage a node may save the model into.
type StorageTarget struct {
	StorageID   uint
	StorageName string
	StorageType string
	Default     bool
}
type ImageFacts struct {
	Canonical string
	Cached    *bool
}
type Candidate struct {
	Key, Label, Reason string
	Recommended        bool
	// Storage fields are set on present candidates and storage targets.
	StorageID   uint
	StorageType string
	SizeBytes   int64
	CacheBytes  int64
	OnNode      bool
	Unavailable string
	Default     bool
	// DisplayName is a human-readable name for this candidate when its Label
	// is an identifier rather than something worth showing.
	DisplayName string
}
type Result struct {
	Providers []Candidate
	// Present has one candidate per storage holding the model; Targets are
	// the storages a node may save the model into.
	Present []Candidate
	Targets []Candidate
	Image   ImageResult
}
type ImageResult struct {
	Canonical string
	Cached    *bool
}

// Resolve turns a snapshot of facts into the lists the deploy form shows:
// the storages that hold the model, the storages a node may save it into, and
// the public weight sources the template offers. Where a node gets the model
// and the image from is not among them: a node copies them from a node of its
// sync group or fetches them through the pull settings, whichever applies when
// it starts.
func Resolve(w WeightFacts, image ImageFacts) Result {
	return Result{
		Image:     ImageResult(image),
		Present:   resolvePresent(w.Present),
		Targets:   resolveTargets(w.Targets),
		Providers: resolveProviders(w.Providers),
	}
}

// resolveProviders lists the public repositories the weights can come from,
// preferring the one the inference template suggested. With no suggestion the
// first provider stands in, so the form always has a default.
func resolveProviders(providers []WeightSource) []Candidate {
	var out []Candidate
	recommended := false
	seen := make(map[string]struct{}, len(providers))
	suggested := false
	for _, provider := range providers {
		if provider.Suggested {
			suggested = true
			break
		}
	}
	for _, provider := range providers {
		key := "public:" + provider.Provider + ":" + provider.RepoID + ":" + provider.Revision
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		take := !recommended && (provider.Suggested || !suggested)
		recommended = recommended || take
		out = append(out, Candidate{
			Key: key, Label: provider.Provider, DisplayName: provider.DisplayName,
			Reason: "configured model provider", Recommended: take,
		})
	}
	return out
}

// resolvePresent orders the storages that hold the model by what a launch has
// to wait for: files already on the node's disk first, then a shared storage
// read in place, then a shared storage that must first copy into the node
// cache. The first usable one is recommended.
// resolvePresent orders the storages that hold the model the way a launch
// picks among them (store.ModelSourceOn): a copy on the node's disk, then one
// read straight from a shared storage, then one a cache must be filled from.
// This leaf cannot ask store, so the order is restated here over the facts the
// caller computed from the same locations; change both together.
func resolvePresent(present []StoragePresence) []Candidate {
	rank := func(p StoragePresence) int {
		switch {
		case p.Unavailable != "":
			return 4
		case p.OnNode:
			return 0
		case p.CacheBytes > 0:
			return 2
		default:
			return 1
		}
	}
	sorted := append([]StoragePresence(nil), present...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i]) < rank(sorted[j]) })
	out := make([]Candidate, 0, len(sorted))
	recommended := false
	for _, p := range sorted {
		take := !recommended && p.Unavailable == ""
		recommended = recommended || take
		reason := "model files are in this storage"
		if p.Unavailable != "" {
			reason = p.Unavailable
		}
		out = append(out, Candidate{
			Key: fmt.Sprintf("present:%d", p.StorageID), Label: p.StorageName, Reason: reason,
			Recommended: take, StorageID: p.StorageID, StorageType: p.StorageType,
			SizeBytes: p.SizeBytes, CacheBytes: p.CacheBytes, OnNode: p.OnNode, Unavailable: p.Unavailable,
		})
	}
	return out
}

// resolveTargets lists the storages a copy or download may save into, the
// default storage first and recommended.
func resolveTargets(targets []StorageTarget) []Candidate {
	sorted := append([]StorageTarget(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Default && !sorted[j].Default })
	out := make([]Candidate, 0, len(sorted))
	for i, target := range sorted {
		out = append(out, Candidate{
			Key: fmt.Sprintf("storage:%d", target.StorageID), Label: target.StorageName, Reason: "writable storage",
			Recommended: i == 0, StorageID: target.StorageID, StorageType: target.StorageType, Default: target.Default,
		})
	}
	return out
}
