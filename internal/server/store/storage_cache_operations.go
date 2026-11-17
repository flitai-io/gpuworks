package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const StorageCacheTopic = "storage_cache"

// CacheOperationProtectedEntriesRemain is the outcome a completed clear
// records in its error column when entries a running deployment still uses
// were kept. It is a stored code, compared by value, never a sentence.
const CacheOperationProtectedEntriesRemain = "protected_entries_remain"

// StorageCacheOperation records user intent and execution outcomes. Capacity,
// references, applicability and whether a budget is met are derived facts.
type StorageCacheOperation struct {
	StorageID         uint `gorm:"primaryKey"`
	NodeID            uint `gorm:"primaryKey;index"`
	Generation        uint64
	Mode              string `gorm:"size:16"`
	State             string `gorm:"size:16"`
	Error             string `gorm:"size:1024"`
	RequestedAt       time.Time
	AttemptedAt       *time.Time
	DiskInvalidatedAt *time.Time
}

func createStorageCacheOperations(db *gorm.DB) error {
	if err := db.AutoMigrate(&StorageCacheOperation{}); err != nil {
		return err
	}
	if db.Migrator().HasTable(&Node{}) && !db.Migrator().HasColumn(&Node{}, "PathUsageObservedAt") {
		return db.Migrator().AddColumn(&Node{}, "PathUsageObservedAt")
	}
	return nil
}

func (s *Store) CacheOperations(storageID uint) ([]StorageCacheOperation, error) {
	var rows []StorageCacheOperation
	q := s.db
	if storageID != 0 {
		q = q.Where("storage_id = ?", storageID)
	}
	err := q.Order("storage_id,node_id").Find(&rows).Error
	return rows, err
}

// WaitingCacheOperationsForNode supports cheap node heartbeat/reconnect work.
func (s *Store) WaitingCacheOperationsForNode(nodeID uint) ([]StorageCacheOperation, error) {
	var rows []StorageCacheOperation
	err := s.db.Where("node_id = ? AND state IN ?", nodeID, []string{"pending", "waiting"}).Find(&rows).Error
	return rows, err
}

func (s *Store) CacheOperation(storageID, nodeID uint) (*StorageCacheOperation, error) {
	var row StorageCacheOperation
	err := s.db.Where("storage_id = ? AND node_id = ?", storageID, nodeID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// RequestCacheOperations changes a fixed node set atomically. Automatic trim
// preserves unfinished clear and failed requests; only an explicit retry or a
// configuration change should replace a failure. Completed clear is consumed.
func (s *Store) RequestCacheOperations(storageID uint, nodeIDs []uint, mode string, explicit bool) error {
	return s.requestCacheOperations(storageID, nodeIDs, mode, explicit, false)
}

// RequestCachePolicyTrim retries the new policy without replacing an unfinished
// manual clear. Ordinary fact notifications do not erase a failed outcome.
func (s *Store) RequestCachePolicyTrim(storageID uint, nodeIDs []uint) error {
	return s.requestCacheOperations(storageID, nodeIDs, "trim", false, true)
}

func (s *Store) requestCacheOperations(storageID uint, nodeIDs []uint, mode string, explicit, policyChange bool) error {
	if mode != "trim" && mode != "clear" {
		return fmt.Errorf("invalid cache operation %q", mode)
	}
	if len(nodeIDs) == 0 {
		return errors.New("cache operation requires nodes")
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		return requestCacheOperationsTx(tx, storageID, nodeIDs, mode, explicit, policyChange)
	})
	if err == nil && s.Bus != nil {
		s.Bus.PublishChange(StorageCacheTopic, uintKey(storageID), "requested")
	}
	return err
}

// requestCacheOperationsTx lets a fixed-node policy edit persist its cleanup
// intent in the same transaction; the caller publishes after committing.
func requestCacheOperationsTx(tx *gorm.DB, storageID uint, nodeIDs []uint, mode string, explicit, policyChange bool) error {
	for _, id := range sortedUnique(nodeIDs) {
		var old StorageCacheOperation
		err := tx.Where("storage_id = ? AND node_id = ?", storageID, id).First(&old).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && !explicit && !policyChange && (old.State == "failed" || old.State == "running" || old.State == "pending") {
			continue
		}
		if err == nil && !explicit && mode == "trim" && old.Mode == "clear" && old.State != "completed" {
			continue
		}
		row := StorageCacheOperation{StorageID: storageID, NodeID: id, Generation: old.Generation + 1, Mode: mode, State: "pending", RequestedAt: time.Now().UTC(), DiskInvalidatedAt: old.DiskInvalidatedAt}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "storage_id"}, {Name: "node_id"}}, UpdateAll: true}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// SetCacheOperationResult uses a generation compare-and-set: an old RPC result
// must never complete a request made while that RPC was in flight.
func (s *Store) SetCacheOperationResult(op *StorageCacheOperation, state, message string) (bool, error) {
	updates := map[string]any{"state": state, "error": truncate(message, 1024)}
	if state == "running" {
		updates["attempted_at"] = time.Now().UTC()
	}
	result := s.db.Model(&StorageCacheOperation{}).Where("storage_id = ? AND node_id = ? AND generation = ?", op.StorageID, op.NodeID, op.Generation).Updates(updates)
	if result.Error == nil && result.RowsAffected > 0 && s.Bus != nil {
		s.Bus.PublishChange(StorageCacheTopic, uintKey(op.StorageID), "result")
	}
	return result.RowsAffected > 0, result.Error
}

func (s *Store) RecoverCacheOperations() error {
	return s.db.Model(&StorageCacheOperation{}).Where("state = ?", "running").Update("state", "pending").Error
}

// InvalidateCacheDisk fences disk observations before a potentially mutating
// RPC and again when it confirms success. A crash between filesystem deletion
// and index update must not reuse the old statfs value after restart.
func (s *Store) InvalidateCacheDisk(storageID, nodeID uint) error {
	now := time.Now().UTC()
	row := StorageCacheOperation{StorageID: storageID, NodeID: nodeID, Generation: 1, Mode: "trim", State: "completed", DiskInvalidatedAt: &now}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "storage_id"}, {Name: "node_id"}}, DoUpdates: clause.AssignmentColumns([]string{"disk_invalidated_at"})}).Create(&row).Error
}
