package store

import (
	"fmt"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StorageCacheNotEmptyError refuses an nfs_cache storage's switch to nfs while
// its node caches still hold entries. The node caches belong to the nfs_cache
// type: once the storage is nfs the agents no longer manage those directories,
// so nothing could remove what was left there. Clearing the caches first goes
// through the cache deletion flow, which removes each directory on its node
// and keeps the entry until the node confirms. Transferring counts entries
// still being copied or downloaded, which a clear does not touch.
type StorageCacheNotEmptyError struct {
	NodeIDs      []uint
	Entries      int
	Transferring int
}

func (e *StorageCacheNotEmptyError) Error() string {
	return fmt.Sprintf("%d node cache entries on %d nodes (%d still transferring) must be cleared before switching to nfs",
		e.Entries, len(e.NodeIDs), e.Transferring)
}

// checkNodeCachesEmpty runs inside the switch's transaction, so no cache entry
// can be added between the check and the type change.
func checkNodeCachesEmpty(tx *gorm.DB, storageID uint) error {
	var rows []ModelFile
	if err := tx.Select("id", "node_id", "state", "path").Where("storage_id = ? AND node_id <> 0", storageID).Order("node_id asc").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	out := &StorageCacheNotEmptyError{Entries: len(rows)}
	for _, row := range rows {
		if n := len(out.NodeIDs); n == 0 || out.NodeIDs[n-1] != row.NodeID {
			out.NodeIDs = append(out.NodeIDs, row.NodeID)
		}
		if !row.Ready() && row.State != proto.MFDeleting {
			out.Transferring++
		}
	}
	return out
}
