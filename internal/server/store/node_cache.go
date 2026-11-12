package store

import (
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/modelcache"
)

// NodeCacheEntries lists an nfs_cache storage's cache rows on one node with
// the facts eviction needs: when each was last used, whether a deployment uses
// it (modelFilesInUse), whether it is still being copied, and
// whether the storage still holds the authoritative copy. "In use" is derived
// on every call, never stored.
func (s *Store) NodeCacheEntries(storageID, nodeID uint) ([]modelcache.Entry, error) {
	byNode, err := s.NodeCacheEntriesForNodes(storageID, []uint{nodeID})
	return byNode[nodeID], err
}

// NodeCacheEntriesForNodes reads shared and reference facts once for a node
// page or a fleet operation, rather than repeating those queries per node.
func (s *Store) NodeCacheEntriesForNodes(storageID uint, nodeIDs []uint) (map[uint][]modelcache.Entry, error) {
	out := map[uint][]modelcache.Entry{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	var rows []ModelFile
	if err := s.db.Where("storage_id = ? AND node_id IN ?", storageID, nodeIDs).Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	storage, err := s.StorageByID(storageID)
	if err != nil {
		return nil, err
	}
	targets, err := s.StorageNodes(storageID)
	if err != nil {
		return nil, err
	}
	// A directory change leaves historical files in place. They are neither
	// charged to the new directory's budget nor candidates for its cleanup.
	dirs := nodeCacheDirs(storage, targets)
	current := rows[:0]
	for _, row := range rows {
		if dirs.holds(&row) {
			current = append(current, row)
		}
	}
	rows = current
	if len(rows) == 0 {
		return out, nil
	}
	var shared []ModelFile
	if err := s.db.Where("storage_id = ? AND node_id = 0", storageID).Find(&shared).Error; err != nil {
		return nil, err
	}
	backed := map[uint]bool{}
	for i := range shared {
		backed[shared[i].ModelID] = shared[i].Ready()
	}
	inUse, err := modelFilesInUse(s.db, rows)
	if err != nil {
		return nil, err
	}

	for i := range rows {
		row := &rows[i]
		referenced := len(inUse[row.ID]) > 0
		out[row.NodeID] = append(out[row.NodeID], modelcache.Entry{
			ID: row.ID, SizeBytes: max(row.SizeBytes, row.Downloaded), LastUsed: row.LastUsedAt,
			Referenced: referenced, Transferring: !row.Ready(), Backed: backed[row.ModelID],
		})
	}
	return out, nil
}

// CacheEvictionFile repeats all safety checks immediately before deleting a
// planned row. The caller keeps the index until the node confirms removal.
func (s *Store) CacheEvictionFile(storageID, nodeID, id uint, expectedPath string) (*ModelFile, error) {
	storage, err := s.StorageByID(storageID)
	if err != nil {
		return nil, err
	}
	// A read-only shared library does not make its node caches read-only.
	if storage.Type != proto.StorageNFSCache {
		return nil, errors.New("cache storage changed type")
	}
	node, err := s.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	eff, applies, err := s.EffectiveStorageForNode(storage, node)
	if err != nil {
		return nil, err
	}
	if !applies {
		return nil, errors.New("storage no longer applies to node")
	}
	state, err := s.StorageNodeStateFor(storageID, nodeID)
	if err != nil {
		return nil, err
	}
	if state.State != proto.StorageStateReady || filepath.Clean(state.CachePath) != filepath.Clean(eff.CachePath) {
		return nil, errors.New("cache directory is not ready")
	}
	row, err := s.ModelFileByID(id)
	if err != nil {
		return nil, err
	}
	if row.StorageID != storageID || row.NodeID != nodeID || row.Path != expectedPath || !row.Ready() {
		return nil, errors.New("cache entry changed")
	}
	if !cacheDirectoryContains(eff.CachePath, row.Path) {
		return nil, errors.New("cache entry belongs to another directory")
	}
	entries, err := s.NodeCacheEntries(storageID, nodeID)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID == id && e.Evictable() {
			return row, nil
		}
	}
	return nil, errors.New("cache entry is protected")
}

// cacheDirs is each node's current cache directory: its own override,
// otherwise the storage's.
type cacheDirs struct {
	shared string
	byNode map[uint]string
}

func nodeCacheDirs(storage *Storage, targets []StorageNode) cacheDirs {
	dirs := cacheDirs{shared: storage.CachePath, byNode: map[uint]string{}}
	for _, target := range targets {
		if target.NodeID != 0 && target.CachePath != "" {
			dirs.byNode[target.NodeID] = target.CachePath
		}
	}
	return dirs
}

// holds reports whether row lies in its node's current cache directory.
func (d cacheDirs) holds(row *ModelFile) bool {
	dir := d.shared
	if override := d.byNode[row.NodeID]; override != "" {
		dir = override
	}
	filePath := row.Path
	// PrepareModelCopy records a relative destination until the agent
	// reports its absolute path. Keep that transfer's protected budget.
	if !row.Ready() && !filepath.IsAbs(filePath) {
		filePath, _ = SharedHostPath(dir, filePath)
	}
	return cacheDirectoryContains(dir, filePath)
}

func cacheDirectoryContains(dir, file string) bool {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(file) {
		return false
	}
	rel, err := filepath.Rel(dir, file)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
