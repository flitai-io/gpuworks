package store

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// freeReservedLabelNamespace is migration 119. The gpuworks.ai namespace
// becomes the platform's own with this release (planning.SystemLabels), and
// nothing stopped an operator from writing a key in it before. Such keys are
// deleted: no released build ever gave the namespace a meaning, so a key in
// it is a stray rather than an annotation anyone relies on.
//
// The two places a written label lives are cleared together. A node loses
// the key, and a network attachment selecting on it is removed, because an
// attachment whose key no node may carry any longer would match nothing
// while still looking like it attaches something.
//
// A deployment whose stored selector names such a key is left as it is.
// Dropping the key from the selector would widen the set of nodes the
// deployment may run on; kept, the selector no longer parses, which is the
// existing fail-closed path for a selector the platform cannot honour (see
// planning.placementLabels) and keeps the deployment off every node until
// someone edits it.
//
// Nodes whose labels are untouched are not rewritten, which keeps the
// migration a no-op on the overwhelmingly common database that never used
// the namespace.
func freeReservedLabelNamespace(db *gorm.DB) error {
	if err := deleteReservedNodeLabels(db); err != nil {
		return err
	}
	return deleteReservedLabelAttachments(db)
}

func deleteReservedNodeLabels(db *gorm.DB) error {
	if !db.Migrator().HasTable("nodes") {
		return nil
	}
	type nodeLabelRow struct {
		ID     uint
		Labels string
	}
	var rows []nodeLabelRow
	if err := db.Raw("SELECT id, labels FROM nodes ORDER BY id").Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		// A case-insensitive pre-filter: the reserved check ignores case,
		// so "GPUWORKS.AI/x" must not slip past it.
		if !strings.Contains(strings.ToLower(row.Labels), nodelabel.Domain) {
			continue
		}
		labels := map[string]string{}
		if json.Unmarshal([]byte(row.Labels), &labels) != nil {
			// An unreadable map is left exactly as it is: this migration
			// deletes keys, it does not repair rows.
			continue
		}
		deleted := false
		for key := range labels {
			if nodelabel.Reserved(key) {
				delete(labels, key)
				deleted = true
			}
		}
		if !deleted {
			continue
		}
		encoded, err := json.Marshal(labels)
		if err != nil {
			return err
		}
		if err := db.Exec("UPDATE nodes SET labels = ? WHERE id = ?", string(encoded), row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

func deleteReservedLabelAttachments(db *gorm.DB) error {
	if !db.Migrator().HasTable("network_attachments") {
		return nil
	}
	type attachmentRow struct {
		ID       uint
		LabelKey string
	}
	var rows []attachmentRow
	if err := db.Raw("SELECT id, label_key FROM network_attachments WHERE label_key <> '' ORDER BY id").Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if !nodelabel.Reserved(row.LabelKey) {
			continue
		}
		if err := db.Exec("DELETE FROM network_attachments WHERE id = ?", row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}
