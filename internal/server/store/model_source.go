package store

import (
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// What a node has to do to read a model, as ModelSourceOn decides it.
const (
	// SourceReady: the model is readable at Location now.
	SourceReady = "ready"
	// SourceFillCache: an nfs_cache storage holds the model and the node can
	// mount it; the node copies it into its cache first.
	SourceFillCache = "fill_cache"
	// SourceAcquire: no storage the node reads has the model; the node gets
	// it into Location's local storage (from a peer, else the public source).
	SourceAcquire = "acquire"
	// SourceWaitShared: a write into Location's shared storage is under way.
	SourceWaitShared = "wait_shared"
	// SourceRefused: nothing can bring the model to the node; Reason says why
	// and Location names the storage it is about, when there is one.
	SourceRefused = "refused"
)

// ModelSource is where one node reads one model from.
type ModelSource struct {
	Action   string
	Location ModelLocation
	Reason   string
}

// SourceRules are what a deployment says about where its model comes from.
type SourceRules struct {
	// Pinned limits the node to these storages (the deployment's storage node
	// requirement); empty allows every storage that applies to the node.
	Pinned []uint
	// SaveTo is the storage a node that has to acquire the model saves it
	// into, when that storage applies to the node (deployments.weight_storage_id).
	SaveTo uint
	// Deployment is the deployment the node reads the model for, zero in a
	// preview. With no SaveTo, a write into a shared storage this deployment
	// started is waited for rather than fetched again (see acquireSource).
	Deployment uint
}

// StoragePins are the storages a deployment's requirements limit it to, by
// id; empty when it reads from any storage.
func StoragePins(reqs nodelabel.Requirements) []uint {
	var out []uint
	for _, value := range reqs[nodelabel.KeyStorage] {
		if id, err := strconv.ParseUint(value, 10, 64); err == nil && id > 0 {
			out = append(out, uint(id))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SourceRulesFor are a stored deployment's source rules: the storages its
// requirements limit it to and the storage it saves into.
func (s *Store) SourceRulesFor(dep *Deployment) (SourceRules, error) {
	reqs, err := s.DeploymentRequirements(dep.ID)
	if err != nil {
		return SourceRules{}, err
	}
	return SourceRules{Pinned: StoragePins(reqs), SaveTo: dep.SaveToStorageID, Deployment: dep.ID}, nil
}

// ModelSourceOn decides where a node reads a model from. It is the single
// rule every launch, admission check and preview follows
// (docs/adr/0034-model-files-belong-to-storages.md, decision 4):
//
//  1. A copy the node can read now is used, whichever storage holds it: one on
//     the node's own disk (a local storage, an nfs_cache cache) before one read
//     straight from a shared storage.
//  2. Else an nfs_cache storage that holds the model fills the node's cache.
//  3. Else the model is acquired: into SaveTo when it applies to the node, else
//     into the default storage, else into another enabled local storage. A
//     shared storage is never written automatically: one only qualifies while
//     a write into it is under way (with no SaveTo, one the deployment itself
//     started does), and a SaveTo shared storage without the model refuses
//     rather than letting every node download its own copy.
//
// Within each step SaveTo comes first, then the default storage, then storage
// id order.
func (s *Store) ModelSourceOn(modelID uint, node *Node, rules SourceRules) (ModelSource, error) {
	facts, err := s.loadNodeModelFacts(modelID, node)
	if err != nil {
		return ModelSource{}, err
	}
	result := facts.source(rules)
	if result.Location.Storage.ID != 0 && result.Location.Storage.Name == "" {
		if storage, err := s.StorageByID(result.Location.Storage.ID); err == nil {
			result.Location.Storage = *storage
		} else {
			result.Location.Storage = Storage{}
		}
	}
	return result, nil
}

func (facts nodeModelFacts) source(rules SourceRules) ModelSource {
	candidates := pinnedStorages(facts.storages, rules.Pinned)
	if len(candidates) == 0 {
		refused := ModelSource{Action: SourceRefused, Reason: LocationStorageNotOnNode}
		if len(rules.Pinned) > 0 {
			refused.Location.Storage.ID = rules.Pinned[0]
		}
		return refused
	}
	sortBySourcePreference(candidates, rules.SaveTo)
	locations := make([]ModelLocation, len(candidates))
	for i := range candidates {
		locations[i] = facts.locate(&candidates[i])
	}
	best := -1
	for i, loc := range locations {
		if loc.Ready && (best < 0 || readTier(loc.Storage.Type) < readTier(locations[best].Storage.Type)) {
			best = i
		}
	}
	if best >= 0 {
		return ModelSource{Action: SourceReady, Location: locations[best]}
	}
	for _, loc := range locations {
		if loc.Storage.Type == proto.StorageNFSCache && loc.Reason == LocationNotCached {
			return ModelSource{Action: SourceFillCache, Location: loc}
		}
	}
	return acquireSource(locations, rules)
}

// acquireSource is step 3 of ModelSourceOn over locations already in
// preference order.
func acquireSource(locations []ModelLocation, rules SourceRules) ModelSource {
	saveTo := rules.SaveTo
	// With no storage chosen, a shared storage the node reads qualifies while
	// a write of the model into it that this deployment started is under way
	// - a create that fetched a template component next to its primary
	// started one - so the node waits for that one copy rather than
	// downloading its own. A write someone else started is not waited for: a
	// stuck writer then holds up only the deployment that asked for it. A
	// chosen storage, local included, keeps its say.
	if saveTo == 0 && rules.Deployment != 0 {
		for _, loc := range locations {
			if proto.StorageShared(loc.Storage.Type) && loc.Reason == LocationModelMissingInStore && TransferUnderWay(loc.Shared) && loc.Shared.DownloadDeploymentID == rules.Deployment {
				return ModelSource{Action: SourceWaitShared, Location: loc}
			}
		}
	}
	var refused *ModelSource
	note := func(loc ModelLocation, reason string) {
		if refused == nil {
			refused = &ModelSource{Action: SourceRefused, Location: loc, Reason: reason}
		}
	}
	for _, loc := range locations {
		storage := loc.Storage
		target := storage.ID == saveTo || storage.IsDefault || storage.Type == proto.StorageLocal
		if !target {
			continue
		}
		if storage.Type == proto.StorageLocal {
			// A read-only storage takes nothing new; only a transfer already
			// under way there is followed to its end.
			if storage.Enabled || TransferUnderWay(loc.File) {
				return ModelSource{Action: SourceAcquire, Location: loc}
			}
			note(loc, LocationStorageDisabled)
			continue
		}
		if loc.Reason == LocationModelMissingInStore && TransferUnderWay(loc.Shared) {
			return ModelSource{Action: SourceWaitShared, Location: loc}
		}
		if storage.ID == saveTo {
			return ModelSource{Action: SourceRefused, Location: loc, Reason: loc.Reason}
		}
		note(loc, loc.Reason)
	}
	if refused != nil {
		return *refused
	}
	// Only shared storages that are neither SaveTo nor the default apply:
	// nothing may be written, so the first one's reason is the answer.
	return ModelSource{Action: SourceRefused, Location: locations[0], Reason: locations[0].Reason}
}

// TransferUnderWay reports whether a row is a download, copy or sync that has
// not finished yet; a failed or missing one is not.
func TransferUnderWay(file *ModelFile) bool {
	if file == nil {
		return false
	}
	switch file.State {
	case proto.MFCanceled, proto.MFDeleting, proto.MFMissing, proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk, proto.MFAbsent:
		return false
	}
	return !file.Ready()
}

// readTier ranks ready copies: the node's own disk first.
func readTier(storageType string) int {
	if storageType == proto.StorageLocal || storageType == proto.StorageNFSCache {
		return 0
	}
	return 1
}

func pinnedStorages(effective []EffectiveStorage, pinned []uint) []Storage {
	allowed := map[uint]bool{}
	for _, id := range pinned {
		allowed[id] = true
	}
	out := make([]Storage, 0, len(effective))
	for _, eff := range effective {
		if len(allowed) == 0 || allowed[eff.Storage.ID] {
			out = append(out, eff.Storage)
		}
	}
	return out
}

// sortBySourcePreference orders storages SaveTo first, then the default, then
// by id.
func sortBySourcePreference(storages []Storage, saveTo uint) {
	rank := func(st Storage) int {
		switch {
		case saveTo != 0 && st.ID == saveTo:
			return 0
		case st.IsDefault:
			return 1
		}
		return 2
	}
	sort.SliceStable(storages, func(i, j int) bool {
		if ri, rj := rank(storages[i]), rank(storages[j]); ri != rj {
			return ri < rj
		}
		return storages[i].ID < storages[j].ID
	})
}
