package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StorageCacheNotEmptyError refuses an nfs_cache storage's switch to nfs
// while its node caches still hold entries. The node caches belong to the
// nfs_cache type: once the storage is nfs the agents no longer manage those
// directories, so nothing could remove what was left there. Clearing the
// caches first goes through the cache deletion flow, which removes each
// directory on its node and keeps the entry until the node confirms.
// Transferring counts copies or downloads still running; the rest are cached
// copies or failed and suspended ones, which a node's own delete removes.
type StorageCacheNotEmptyError struct {
	NodeUUIDs    []string
	Entries      int
	Transferring int
}

func (e *StorageCacheNotEmptyError) Error() string {
	return fmt.Sprintf("node caches still hold entries (entries: %d, nodes: %d, transferring: %d); clear them before switching to nfs",
		e.Entries, len(e.NodeUUIDs), e.Transferring)
}

// settleNodeCachesForSwitch runs inside the switch's transaction, so no cache
// entry can be added between the check and the type change. Two kinds of
// entries are already unmanaged and out of every cleanup's reach, so their
// rows go with the switch instead of blocking it: those outside their node's
// current cache directory (left by an earlier directory change) and those of
// a node the storage no longer applies to (its agent no longer gets the
// storage). Every other entry blocks.
func settleNodeCachesForSwitch(tx *gorm.DB, storage *Storage) error {
	var rows []ModelFile
	if err := tx.Where("storage_id = ? AND node_id <> 0", storage.ID).Order("node_id asc, id asc").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var targets []StorageNode
	if err := tx.Where("storage_id = ?", storage.ID).Order("id asc").Find(&targets).Error; err != nil {
		return err
	}
	targets = canonicalStorageNodes(targets)
	dirs := nodeCacheDirs(storage, targets)
	inScope, err := storageScope(tx, storage, targets, rows)
	if err != nil {
		return err
	}
	out := &StorageCacheNotEmptyError{}
	var stale []uint
	var nodeIDs []uint
	for i := range rows {
		row := &rows[i]
		if !inScope[row.NodeID] || !dirs.holds(row) {
			stale = append(stale, row.ID)
			continue
		}
		out.Entries++
		if n := len(nodeIDs); n == 0 || nodeIDs[n-1] != row.NodeID {
			nodeIDs = append(nodeIDs, row.NodeID)
		}
		switch row.State {
		case proto.MFPending, proto.MFDownloading, proto.MFSyncing:
			out.Transferring++
		}
	}
	if out.Entries > 0 {
		if err := tx.Model(&Node{}).Where("id IN ?", nodeIDs).Order("id asc").Pluck("uuid", &out.NodeUUIDs).Error; err != nil {
			return err
		}
		return out
	}
	return tx.Where("id IN ?", stale).Delete(&ModelFile{}).Error
}

// storageScope reports, for each node holding one of rows, whether the
// storage still applies to it. A row whose node is gone is out of scope.
func storageScope(tx *gorm.DB, storage *Storage, targets []StorageNode, rows []ModelFile) (map[uint]bool, error) {
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.NodeID)
	}
	var nodes []Node
	if err := tx.Where("id IN ?", sortedUnique(ids)).Find(&nodes).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]bool, len(nodes))
	for i := range nodes {
		out[nodes[i].ID] = StorageAppliesTo(storage, targets, &nodes[i])
	}
	return out, nil
}

// ErrNoNodeCache refuses a node-level model file on a storage that has no
// node copies: nfs and shared_dir keep one shared row. A cache fill that read
// the storage as nfs_cache before a switch to nfs committed would otherwise
// create a row the switch's emptiness check never saw, under a type whose
// agents no longer manage the cache directory.
var ErrNoNodeCache = errors.New("this storage keeps no copies on nodes")

func checkNodeRowAllowed(tx *gorm.DB, ref FileRef) error {
	if ref.NodeID == 0 || ref.StorageID == 0 {
		return nil
	}
	var storage Storage
	if err := tx.Select("type").First(&storage, ref.StorageID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if storage.Type == proto.StorageNFS || storage.Type == proto.StorageSharedDir {
		return ErrNoNodeCache
	}
	return nil
}
