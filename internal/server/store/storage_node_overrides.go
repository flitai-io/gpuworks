package store

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StorageNodeOverridePatch only changes cache fields explicitly requested.
// A nil path keeps it; an empty path inherits. SetCacheMaxBytes distinguishes
// keeping the limit from restoring inheritance with a nil CacheMaxBytes.
type StorageNodeOverridePatch struct {
	CachePath        *string
	SetCacheMaxBytes bool
	CacheMaxBytes    *int64
}

func nextStorageRevision(previous time.Time) time.Time {
	next := time.Now().UTC()
	if !next.After(previous) {
		return previous.Add(time.Nanosecond)
	}
	return next
}

// PatchStorageNodeOverrides merges cache fields into the latest node rows in
// one transaction. It never changes node selection or any unmentioned field.
func (s *Store) PatchStorageNodeOverrides(storageID uint, nodeIDs []uint, patch StorageNodeOverridePatch) (*Storage, error) {
	if len(nodeIDs) == 0 || (patch.CachePath == nil && !patch.SetCacheMaxBytes) {
		return nil, fmt.Errorf("node selection and cache changes are required")
	}
	if patch.CacheMaxBytes != nil && *patch.CacheMaxBytes < 0 {
		return nil, fmt.Errorf("cache_max_bytes must be non-negative")
	}
	if patch.CachePath != nil {
		cleaned := strings.TrimSpace(*patch.CachePath)
		patch.CachePath = &cleaned
		if cleaned != "" && !path.IsAbs(cleaned) {
			return nil, ErrStoragePathRequired
		}
	}
	var storage Storage
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&storage, storageID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if storage.Type != proto.StorageNFSCache {
			return fmt.Errorf("only an nfs_cache storage has cache overrides")
		}
		var previous []StorageNode
		if err := tx.Where("storage_id = ?", storageID).Order("id asc").Find(&previous).Error; err != nil {
			return err
		}
		canonical := map[uint]StorageNode{}
		for _, row := range canonicalStorageNodes(previous) {
			if row.NodeID != 0 {
				canonical[row.NodeID] = row
			}
		}
		seen := map[uint]bool{}
		for _, nodeID := range nodeIDs {
			if nodeID == 0 || seen[nodeID] {
				return fmt.Errorf("node IDs must be unique and nonzero")
			}
			seen[nodeID] = true
			var count int64
			if err := tx.Model(&Node{}).Where("id = ?", nodeID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return ErrNotFound
			}
			target, exists := canonical[nodeID]
			if !exists {
				target = StorageNode{StorageID: storageID, NodeID: nodeID, OverrideOnly: true}
			}
			if target.ID != 0 {
				if err := tx.Where("storage_id = ? AND node_id = ? AND id <> ?", storageID, nodeID, target.ID).Delete(&StorageNode{}).Error; err != nil {
					return err
				}
			}
			if patch.CachePath != nil {
				target.CachePath = *patch.CachePath
			}
			if patch.SetCacheMaxBytes {
				target.CacheMaxBytes = patch.CacheMaxBytes
			}
			if target.OverrideOnly && target.Path == "" && target.CachePath == "" && target.CacheMaxBytes == nil && !target.Existing {
				if target.ID != 0 {
					if err := tx.Delete(&target).Error; err != nil {
						return err
					}
				}
				continue
			}
			if err := tx.Save(&target).Error; err != nil {
				return err
			}
		}
		if err := checkStorageReaderTargets(tx, storage, storage, previous); err != nil {
			return err
		}
		storage.UpdatedAt = nextStorageRevision(storage.UpdatedAt)
		if err := tx.Model(&Storage{}).Where("id = ?", storageID).Update("updated_at", storage.UpdatedAt).Error; err != nil {
			return err
		}
		return requestCacheOperationsTx(tx, storageID, nodeIDs, "trim", false, true)
	})
	if err != nil {
		return nil, err
	}
	s.publishStorage(storageID, "node_overrides")
	if s.Bus != nil {
		s.Bus.PublishChange(StorageCacheTopic, uintKey(storageID), "requested")
	}
	return &storage, nil
}

// deleteNodeStorageRecords removes selection, reports and pending operations
// together with a deleted node, so no orphan can trigger later cache work.
func deleteNodeStorageRecords(tx *gorm.DB, nodeID uint) error {
	for _, entity := range []any{&StorageNode{}, &StorageNodeState{}, &StorageCacheOperation{}} {
		if err := tx.Where("node_id = ?", nodeID).Delete(entity).Error; err != nil {
			return err
		}
	}
	return nil
}
