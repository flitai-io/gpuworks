package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TryStartImageTransfer records next only if the row still matches observed.
// A nil observation means the row did not exist. The hub decides whether the
// observed state is eligible; the store only protects that observation from a
// concurrent start, cancellation, completion or heartbeat.
//
// A false result is a conflict, not a database error. The caller must read and
// reconsider the row, and must not dispatch next unless started is true. The
// returned row belongs to this write, even if another operation changes it
// immediately after commit.
func (s *Store) TryStartImageTransfer(ctx context.Context, observed, next *ImageNode) (row *ImageNode, started bool, err error) {
	if next == nil || next.TransferID == "" || (next.State != ImageStatePulling && next.State != ImageStateSyncing) {
		return nil, false, errors.New("an image transfer requires an identity and a running state")
	}
	candidate := *next
	if candidate.ImageVersionID == 0 {
		candidate.ImageVersionID = candidate.ImageID
	}
	candidate.ImageID = candidate.ImageVersionID
	if candidate.ImageVersionID == 0 || candidate.NodeID == 0 {
		return nil, false, errors.New("an image transfer requires an image and a node")
	}
	if observed != nil && (observed.ID == 0 || observed.ImageVersionID != candidate.ImageVersionID || observed.NodeID != candidate.NodeID) {
		return nil, false, errors.New("image transfer observation belongs to a different row")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if observed == nil {
			candidate.ID = 0
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate)
			if res.Error != nil {
				return res.Error
			}
			started = res.RowsAffected == 1
		} else {
			res := imageTransferObservation(tx, observed).Updates(map[string]any{
				"state": candidate.State, "transfer_id": candidate.TransferID,
				"size_bytes": candidate.SizeBytes, "runtime_size_bytes": nil,
				"transferred_bytes": int64(0), "rate_bps": int64(0),
				"source": candidate.Source, "source_node_id": candidate.SourceNodeID,
				"rate_limit_kib": candidate.RateLimitKiB, "transfer_error_code": "",
				"message": candidate.Message,
			})
			if res.Error != nil {
				return res.Error
			}
			started = res.RowsAffected == 1
		}
		if !started {
			return nil
		}
		// Read under the same transaction: an unconditional read after commit
		// could accidentally acknowledge a different caller's transfer id.
		var saved ImageNode
		if err := tx.Where("image_version_id = ? AND node_id = ?", candidate.ImageVersionID, candidate.NodeID).First(&saved).Error; err != nil {
			return err
		}
		saved.ImageID = saved.ImageVersionID
		row = &saved
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if started {
		s.Bus.PublishChange("image", uintKey(candidate.ImageVersionID), "node")
	}
	return row, started, nil
}

func imageTransferObservation(db *gorm.DB, observed *ImageNode) *gorm.DB {
	return db.Model(&ImageNode{}).Where(
		"id = ? AND image_version_id = ? AND node_id = ? AND state = ? AND COALESCE(transfer_id, '') = ? AND last_transfer_id = ? AND transfer_error_code = ? AND updated_at = ?",
		observed.ID, observed.ImageVersionID, observed.NodeID, observed.State, observed.TransferID, observed.LastTransferID, observed.TransferErrorCode, observed.UpdatedAt,
	)
}

// FailImageTransferIfActive cannot fail a replacement attempt or overwrite a
// terminal result that arrived while the start RPC was returning.
func (s *Store) FailImageTransferIfActive(imageID, nodeID uint, transferID, message string) error {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ? AND transfer_id = ? AND state IN ?", imageID, nodeID, transferID, []string{ImageStatePulling, ImageStateSyncing}).
		Updates(map[string]any{"state": ImageStateFailed, "last_transfer_id": transferID, "transfer_id": "", "rate_bps": int64(0), "message": message})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.Error
}

// FinishImagePullIfUnchanged adds the candidate-chain outcome without letting
// an old watcher rewrite a retry, a cancellation or a newer terminal result.
func (s *Store) FinishImagePullIfUnchanged(ctx context.Context, observed *ImageNode, message string) error {
	completedID := observed.TransferID
	if completedID == "" {
		completedID = observed.LastTransferID
	}
	res := imageTransferObservation(s.db.WithContext(ctx), observed).Updates(map[string]any{
		"state": ImageStateFailed, "last_transfer_id": completedID, "transfer_id": "", "rate_bps": int64(0), "message": message,
	})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(observed.ImageVersionID), "node")
	}
	return res.Error
}
