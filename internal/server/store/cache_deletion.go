package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

var ErrCacheDeletionPending = errors.New("cache deletion is awaiting confirmation; retry cache cleanup before reusing this model file")

// BeginCacheDeletion durably withdraws a ready file before the first remote
// mutation. The caller holds the node eviction reservation. TransferID becomes
// the idempotency key for this deletion and survives retries and restarts.
func (s *Store) BeginCacheDeletion(row *ModelFile) (*ModelFile, error) {
	if row.State == proto.MFDeleting {
		if row.TransferID == "" {
			return nil, ErrCacheDeletionPending
		}
		return row, nil
	}
	token := auth.NewUUID()
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// Parent-resource/config changes use the same write transaction. A
		// pre-transaction check would leave a gap where a new path could win.
		snapshot := &Store{db: tx}
		checked, err := snapshot.CacheEvictionFile(row.StorageID, row.NodeID, row.ID, row.Path)
		if err != nil {
			return err
		}
		res := tx.Model(&ModelFile{}).Where("id = ? AND state = ? AND path = ? AND COALESCE(transfer_id, '') = ?", checked.ID, checked.State, checked.Path, checked.TransferID).Updates(map[string]any{"state": proto.MFDeleting, "transfer_id": token, "rate_bps": 0, "message": ErrCacheDeletionPending.Error()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("cache entry changed before deletion")
		}
		// Inline admission also needs durable recovery after a server crash.
		// Preserve an existing clear, running request, or explicit failure.
		return requestCacheOperationsTx(tx, row.StorageID, []uint{row.NodeID}, "trim", false, false)
	})
	if err != nil {
		return nil, err
	}
	s.publishModelFile(row.ID, proto.MFDeleting)
	return s.ModelFileByID(row.ID)
}

func (s *Store) PendingCacheDeletions(storageID, nodeID uint) ([]ModelFile, error) {
	var rows []ModelFile
	err := s.db.Where("storage_id = ? AND node_id = ? AND state = ?", storageID, nodeID, proto.MFDeleting).Order("id asc").Find(&rows).Error
	return rows, err
}

// CompleteCacheDeletion only consumes the exact durable intent acknowledged
// by the agent. Ordinary model registration cannot supersede an unresolved one.
func (s *Store) CompleteCacheDeletion(row *ModelFile) error {
	res := s.db.Where("id = ? AND state = ? AND transfer_id = ? AND path = ?", row.ID, proto.MFDeleting, row.TransferID, row.Path).Delete(&ModelFile{})
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(row.ID, "deleted")
	}
	return res.Error
}
