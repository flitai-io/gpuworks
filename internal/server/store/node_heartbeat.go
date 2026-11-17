package store

import (
	"time"

	"gorm.io/gorm"
)

// TouchNode bumps last-seen (heartbeat). Kept cheap; no event published.
func (s *Store) TouchNode(uuid string) error {
	return s.TouchNodesAt(map[string]time.Time{uuid: time.Now().UTC()})
}

// TouchNodesAt records heartbeats, each node's at the time it was observed,
// in one transaction: a fleet's heartbeats arrive tens a second, and one
// writer transaction each kept them queuing behind the store's single
// writer until nodes with live sessions read as stale.
func (s *Store) TouchNodesAt(seen map[string]time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for uuid, at := range seen {
			if err := tx.Model(&Node{}).Where("uuid = ?", uuid).Update("last_seen_at", at).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		// A heartbeat is routine: FleetNodes shows it after the report window.
		s.noteNodeChange(NodeChangeStatus)
	}
	return err
}
