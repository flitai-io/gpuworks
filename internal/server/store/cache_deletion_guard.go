package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// checkNoCacheDeletion runs inside the parent mutation transaction. Removing
// an unacknowledged deletion would allow a replacement to race its remote RPC.
func checkNoCacheDeletion(scope *gorm.DB) error {
	var count int64
	if err := scope.Model(&ModelFile{}).Where("state = ?", proto.MFDeleting).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return ErrCacheDeletionPending
	}
	return nil
}

// Keep the effective directories and membership needed to finish deletion.
// Budget changes do not affect the identity of the operation or its bytes.
func checkCacheDeletionTargets(tx *gorm.DB, before, after Storage, oldTargets, newTargets []StorageNode) error {
	pendingNodes := tx.Model(&ModelFile{}).Select("node_id").Where("storage_id = ? AND state = ?", before.ID, proto.MFDeleting)
	var nodes []Node
	if err := tx.Where("id IN (?)", pendingNodes).Find(&nodes).Error; err != nil {
		return err
	}
	for _, node := range nodes {
		labels, err := NodeLabels(node.Labels)
		if err != nil {
			return err
		}
		old, _ := effectiveStorage(before, oldTargets, node.ID, labels)
		next, applies := effectiveStorage(after, newTargets, node.ID, labels)
		if !applies || old.Path != next.Path || old.CachePath != next.CachePath || old.Existing != next.Existing {
			return ErrCacheDeletionPending
		}
	}
	return nil
}

// Labels can remove membership just as a storage selector edit can. Restrict
// these reads to storages with an outstanding deletion on this node.
func checkCacheDeletionLabels(tx *gorm.DB, nodeID uint, labels map[string]string) error {
	pendingStorages := tx.Model(&ModelFile{}).Select("storage_id").Where("node_id = ? AND state = ?", nodeID, proto.MFDeleting)
	var storages []Storage
	if err := tx.Where("id IN (?)", pendingStorages).Find(&storages).Error; err != nil || len(storages) == 0 {
		return err
	}
	var targets []StorageNode
	if err := tx.Where("storage_id IN (?)", pendingStorages).Find(&targets).Error; err != nil {
		return err
	}
	byStorage := map[uint][]StorageNode{}
	for _, target := range targets {
		byStorage[target.StorageID] = append(byStorage[target.StorageID], target)
	}
	for _, storage := range storages {
		if _, applies := effectiveStorage(storage, byStorage[storage.ID], nodeID, labels); !applies {
			return ErrCacheDeletionPending
		}
	}
	return nil
}
