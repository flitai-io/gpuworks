package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func addPeerTransferOwnership(db *gorm.DB) error {
	for _, model := range []any{&ModelFile{}, &ImageNode{}} {
		if db.Migrator().HasTable(model) && !db.Migrator().HasColumn(model, "TransferErrorCode") {
			if err := db.Migrator().AddColumn(model, "TransferErrorCode"); err != nil {
				return err
			}
		}
	}
	if db.Migrator().HasTable(&ImageNode{}) && !db.Migrator().HasColumn(&ImageNode{}, "LastTransferID") {
		return db.Migrator().AddColumn(&ImageNode{}, "LastTransferID")
	}
	return nil
}

// RestartModelSyncIfActive replaces only the attempt the caller still owns.
// An explicit rate includes zero (unlimited); archived rates are never guessed.
func (s *Store) RestartModelSyncIfActive(ref FileRef, oldID, nextID string, source uint, expected int64, rate ...int) (bool, error) {
	if oldID == "" || nextID == "" || oldID == nextID {
		return false, errors.New("distinct model sync attempt identities are required")
	}
	ref = s.resolveRef(ref)
	var row ModelFile
	if err := refQuery(s.db, ref).First(&row).Error; err != nil {
		return false, err
	}
	updates := map[string]any{"state": proto.MFSyncing, "acquisition": proto.ModelAcquirePeer, "transfer_id": nextID, "transfer_source_node_id": source, "downloaded": 0, "transferred_bytes": 0, "rate_bps": 0, "message": "", "transfer_error_code": ""}
	if expected > 0 {
		updates["size_bytes"] = expected
	}
	if len(rate) > 0 {
		if rate[0] < 0 {
			return false, errors.New("model sync rate must be nonnegative")
		}
		updates["rate_limit"] = rate[0]
	}
	res := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND state IN ?", row.ID, oldID, []string{proto.MFSyncing, proto.MFFailed}).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(row.ID, proto.MFSyncing)
	}
	return res.RowsAffected > 0, res.Error
}

func (s *Store) SetModelSyncDownloadRequiredCode(ref FileRef, id, message, code string) error {
	ref = s.resolveRef(ref)
	var row ModelFile
	if err := refQuery(s.db, ref).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	q := s.db.Model(&ModelFile{}).Where("id = ? AND state NOT IN ?", row.ID, []string{proto.MFPresent, proto.MFRegistered, proto.MFDeleting, proto.MFCanceled})
	if id != "" {
		q = q.Where("transfer_id = ?", id)
	}
	res := q.Updates(map[string]any{"state": proto.MFDownloadRequired, "acquisition": proto.ModelAcquirePeer, "rate_bps": 0, "last_rate_limit": gorm.Expr("CASE WHEN state = ? OR rate_limit > 0 THEN rate_limit ELSE last_rate_limit END", proto.MFSyncing), "rate_limit": 0, "message": message, "transfer_error_code": code})
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(row.ID, proto.MFDownloadRequired)
	}
	return res.Error
}

// MarkModelSyncWaiting retains a temporarily busy attempt as an owned task.
func (s *Store) MarkModelSyncWaiting(ref FileRef, id, message, code string) (bool, error) {
	ref = s.resolveRef(ref)
	var row ModelFile
	if err := refQuery(s.db, ref).First(&row).Error; err != nil {
		return false, err
	}
	res := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND state IN ?", row.ID, id, []string{proto.MFSyncing, proto.MFFailed}).Updates(map[string]any{"state": proto.MFSyncing, "message": message, "transfer_error_code": code, "rate_bps": 0})
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(row.ID, proto.MFSyncing)
	}
	return res.RowsAffected > 0, res.Error
}

func (s *Store) FailImageSyncIfActiveCode(imageID, nodeID uint, id, message, code string) error {
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ? AND state = ? AND transfer_id = ?", imageID, nodeID, ImageStateSyncing, id).Updates(map[string]any{"state": ImageStateFailed, "last_transfer_id": gorm.Expr("CASE WHEN COALESCE(transfer_id, '') <> '' THEN transfer_id ELSE last_transfer_id END"), "transfer_id": "", "rate_bps": 0, "last_rate_limit_kib": gorm.Expr("rate_limit_kib"), "rate_limit_kib": 0, "message": message, "transfer_error_code": code})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.Error
}

// CancelImageNodeTransferIfActive fails one in-flight attempt for an operator
// stop. code is carried in transfer_error_code: a peer attempt is marked
// PEER_TRANSFER_CANCELED so the receiver's own report of the load the stop
// killed cannot replace the reason (see ApplyImageNodeStatus).
func (s *Store) CancelImageNodeTransferIfActive(imageID, nodeID uint, id, message, code string) (bool, error) {
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ? AND transfer_id = ? AND state IN ?", imageID, nodeID, id, []string{ImageStatePulling, ImageStateSyncing}).Updates(map[string]any{"state": ImageStateFailed, "last_transfer_id": gorm.Expr("CASE WHEN COALESCE(transfer_id, '') <> '' THEN transfer_id ELSE last_transfer_id END"), "transfer_id": "", "rate_bps": 0, "last_rate_limit_kib": gorm.Expr("rate_limit_kib"), "rate_limit_kib": 0, "message": message, "transfer_error_code": code})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, res.Error
}

func (s *Store) RestartImageSyncIfActive(imageID, nodeID uint, oldID, nextID string, source uint, size, rate int64) (bool, error) {
	if oldID == "" || nextID == "" || oldID == nextID || rate < 0 {
		return false, errors.New("valid image attempt identities and rate are required")
	}
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ? AND transfer_id = ? AND state = ?", imageID, nodeID, oldID, ImageStateSyncing).Updates(map[string]any{"transfer_id": nextID, "source_node_id": source, "source": "peer", "size_bytes": size, "transferred_bytes": 0, "rate_bps": 0, "rate_limit_kib": rate, "message": "", "transfer_error_code": ""})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, res.Error
}

// MarkImageSyncWaiting runs before stopping a capacity-rejected receiver, so
// its final failure report cannot turn a temporary wait into a failed source.
func (s *Store) MarkImageSyncWaiting(imageID, nodeID uint, id, message, code string) (bool, error) {
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ? AND state = ? AND transfer_id = ?", imageID, nodeID, ImageStateSyncing, id).Updates(map[string]any{"message": message, "transfer_error_code": code, "rate_bps": 0})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, res.Error
}
