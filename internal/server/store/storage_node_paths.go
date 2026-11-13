package store

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"gorm.io/gorm"
)

// SetStorageNodePath gives one node its own directory for a storage: the
// node's override row is created or its path replaced. On a storage that does
// not apply to every node the row also makes it a target.
func (s *Store) SetStorageNodePath(storageID, nodeID uint, dir string) error {
	return s.setStorageNodePath(storageID, nodeID, dir, false)
}

func (s *Store) setStorageNodePath(storageID, nodeID uint, dir string, overrideOnly bool) error {
	dir = strings.TrimSpace(dir)
	if !path.IsAbs(dir) {
		return fmt.Errorf("storage directory %q must be absolute", dir)
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var storage Storage
		if err := tx.First(&storage, storageID).Error; err != nil {
			return err
		}
		if err := tx.Model(&storage).Update("updated_at", nextStorageRevision(storage.UpdatedAt)).Error; err != nil {
			return err
		}
		var row StorageNode
		err := tx.Where("storage_id = ? AND node_id = ?", storageID, nodeID).First(&row).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if overrideOnly && strings.TrimSpace(row.Path) != "" {
			return nil
		}
		currentPath := storage.Path
		if row.Path != "" {
			currentPath = row.Path
		}
		if currentPath != dir {
			if err := checkNoCacheDeletion(tx.Where("storage_id = ? AND node_id = ?", storageID, nodeID)); err != nil {
				return err
			}
		}
		if row.ID == 0 {
			return tx.Create(&StorageNode{StorageID: storageID, NodeID: nodeID, Path: dir, OverrideOnly: overrideOnly}).Error
		}
		return tx.Model(&StorageNode{}).Where("id = ?", row.ID).Update("path", dir).Error
	})
	if err == nil {
		s.publishStorage(storageID, "updated")
	}
	return err
}

// EnsureNodeOwnModelsDir records dir as the node's directory for the default
// local storage unless the node already has one of its own. An agent that
// does not run as root offers it on connect: the storage's directory is not
// one it can create. The override is an ordinary one the operator sees and
// can change.
func (s *Store) EnsureNodeOwnModelsDir(node *Node, dir string) error {
	storage, err := s.StorageByID(s.DefaultLocalStorageID())
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, applies, err := s.EffectiveStorageForNode(storage, node); err != nil || !applies {
		return err
	}
	var row StorageNode
	err = s.db.Where("storage_id = ? AND node_id = ?", storage.ID, node.ID).First(&row).Error
	if err == nil && strings.TrimSpace(row.Path) != "" {
		return nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return s.setStorageNodePath(storage.ID, node.ID, dir, true)
}
