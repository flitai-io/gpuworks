package store

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// SetNodeLabels replaces the user-managed map. Node telemetry never writes
// these labels.
func (s *Store) SetNodeLabels(uuid string, labels map[string]string) (*Node, error) {
	if err := nodelabel.ValidateWritten(labels); err != nil {
		return nil, err
	}
	return s.updateNodeLabels(uuid, "labels", func(map[string]string) map[string]string { return labels })
}

func (s *Store) updateNodeLabels(uuid, reason string, update func(map[string]string) map[string]string) (*Node, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n Node
		if err := tx.Where("uuid = ?", uuid).First(&n).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		labels, err := NodeLabels(n.Labels)
		if err != nil {
			return err
		}
		labels = update(labels)
		if err := nodelabel.ValidateWritten(labels); err != nil {
			return err
		}
		if labels == nil {
			labels = map[string]string{}
		}
		if err := checkCacheDeletionLabels(tx, n.ID, labels); err != nil {
			return err
		}
		raw, err := json.Marshal(labels)
		if err != nil {
			return err
		}
		return tx.Model(&Node{}).Where("id = ?", n.ID).Update("labels", string(raw)).Error
	})
	if err != nil {
		return nil, err
	}
	s.publishNode(uuid, reason)
	return s.NodeWithGPUs(uuid)
}
