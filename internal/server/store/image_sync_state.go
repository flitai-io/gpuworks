package store

import "errors"

// UpdateImageSyncProgressIfActive records progress only for the currently
// active transfer. A late relay callback must not overwrite an agent-reported
// terminal state or a newer retry.
func (s *Store) UpdateImageSyncProgressIfActive(imageID, nodeID uint, transferID string, sizeBytes, transferredBytes, rateBPS int64) (bool, error) {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ? AND state = ? AND transfer_id = ?", imageID, nodeID, ImageStateSyncing, transferID).
		Updates(map[string]any{
			"size_bytes":        sizeBytes,
			"transferred_bytes": transferredBytes,
			"rate_bps":          rateBPS,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, nil
}

// BeginDirectImageSync persists the selected source and per-transfer limit.
// The limit is cleared by ApplyImageNodeStatus or a terminal failure helper.
func (s *Store) BeginDirectImageSync(imageID, nodeID, sourceNodeID uint, transferID string, sizeBytes, rateLimitKiB int64) error {
	if transferID == "" || sourceNodeID == 0 || rateLimitKiB < 0 {
		return errors.New("direct image sync parameters are invalid")
	}
	updates := map[string]any{
		"state": ImageStateSyncing, "transfer_id": transferID, "source_node_id": sourceNodeID,
		"source": "peer", "transferred_bytes": int64(0), "rate_bps": int64(0),
		"rate_limit_kib": rateLimitKiB, "message": "starting direct peer sync", "transfer_error_code": "",
	}
	if sizeBytes > 0 {
		updates["size_bytes"] = sizeBytes
	}
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ?", imageID, nodeID).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "syncing")
	}
	return res.Error
}
