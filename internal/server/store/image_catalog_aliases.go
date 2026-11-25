package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InheritRunnerAliases runs before reconciliation can delete the retired rows.
// A single-platform archive cannot be attached to a multi-platform tag:
// retain its original, architecture-specific row for explicit selection.
func (s *Store) InheritRunnerAliases(aliases map[string][]string, seen map[string]struct{}) error {
	if len(aliases) == 0 {
		return nil
	}
	const key = "image_catalog.runner_aliases"
	return s.db.Transaction(func(tx *gorm.DB) error {
		var marker Setting
		var applied map[string]uint
		err := tx.Where("key = ?", key).Take(&marker).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal([]byte(marker.Value), &applied); err != nil {
				return err
			}
		}
		if applied == nil {
			applied = make(map[string]uint)
		}
		dirty := false
		// Read the incoming refs once: retaining an old row must not make it a
		// migration target within this import.
		var refs []string
		for ref := range aliases {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			names := aliases[ref]
			if len(names) == 0 {
				continue
			}
			var olds []Image
			if err := tx.Where("source = ? AND image_ref IN ?", "official", names).Order("id").Find(&olds).Error; err != nil {
				return err
			}
			if len(olds) == 0 {
				continue
			}
			var dst Image
			err := tx.Where("image_ref = ?", ref).Take(&dst).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// A local series-name conflict can prevent importing the replacement.
				for _, old := range olds {
					seen[old.ImageRef] = struct{}{}
				}
				continue
			}
			if err != nil {
				return err
			}
			changed := false
			for _, old := range olds {
				if dst.Source != "official" {
					seen[old.ImageRef] = struct{}{}
					continue
				}
				if dst.Arch != old.Arch && preserveClusterArchiveSources("", old.RegistrySources) != "[]" {
					seen[old.ImageRef] = struct{}{}
				}
				if applied[old.ImageRef] == dst.ID {
					continue
				}
				dst.Enabled = dst.Enabled && old.Enabled
				if dst.Arch == old.Arch {
					dst.RegistrySources = preserveClusterArchiveSources(dst.RegistrySources, old.RegistrySources)
				}
				applied[old.ImageRef] = dst.ID
				changed = true
			}
			if changed {
				if err := tx.Model(&dst).Select("enabled", "registry_sources").Updates(&dst).Error; err != nil {
					return err
				}
				dirty = true
			}
		}
		if !dirty {
			return nil
		}
		encoded, err := json.Marshal(applied)
		if err != nil {
			return fmt.Errorf("encode runner alias migration: %w", err)
		}
		marker = Setting{Key: key, Value: string(encoded)}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"})}).Create(&marker).Error
	})
}
