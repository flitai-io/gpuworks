package store

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// repairLegacyTemplateModelOperations replaces the chat placeholder migration
// 80 wrote to every pre-template model when a template snapshot declares a
// different API. A default normalized-chat snapshot does not prove that the
// template declared that API: the legacy writer could have replaced a non-chat
// declaration with the model's normalized placeholder. In that case, the
// catalog identity in the snapshot must resolve to a non-chat declaration
// before any write is allowed.
//
// An already normalized model contract is never rewritten here: it may be an
// explicit chat choice, and the current catalog cannot prove otherwise.
func repairLegacyTemplateModelOperations(db *gorm.DB) error {
	if !db.Migrator().HasTable("models") || !db.Migrator().HasTable("deployments") ||
		!db.Migrator().HasColumn("deployments", "model_id") ||
		!db.Migrator().HasColumn("deployments", "template_snapshot") {
		return nil
	}
	var rows []struct {
		ModelID    uint
		Operations string
		Snapshot   string
	}
	// The placeholder is recognized structurally below; SQL only narrows the
	// scan to rows that mention the chat operation at all.
	if err := db.Table("deployments").
		Select("deployments.model_id AS model_id, models.operations AS operations, deployments.template_snapshot AS snapshot").
		Joins("JOIN models ON models.id = deployments.model_id").
		Where("models.operations LIKE ? AND TRIM(COALESCE(deployments.template_snapshot, '')) <> ''",
			"%"+legacyChatOperationType+"%").
		Order("deployments.id DESC").
		Scan(&rows).Error; err != nil {
		return err
	}
	done := map[uint]bool{}
	for _, row := range rows {
		if done[row.ModelID] || !IsDefaultChatOperations(row.Operations) {
			continue
		}
		var snapshot templateSnapshotAPI
		if json.Unmarshal([]byte(row.Snapshot), &snapshot) != nil || snapshot.API == nil {
			continue
		}
		operations := string(snapshot.API.Operations)
		if !legacyOperationsPresent(operations) {
			continue
		}
		if !legacyChatOnlyOperations(operations) {
			if err := updateLegacyModelOperations(db, row.ModelID, row.Operations, operations); err != nil {
				return err
			}
			done[row.ModelID] = true
			continue
		}
		if !legacyNormalizedChatOperations(operations) {
			if !IsDefaultChatOperations(operations) {
				done[row.ModelID] = true
			}
			continue
		}
		declared, ok, err := legacyTemplateNonChatOperations(db, snapshot)
		if err != nil {
			return err
		}
		if ok {
			if err := updateLegacyModelOperations(db, row.ModelID, row.Operations, declared); err != nil {
				return err
			}
		}
		// Without a trustworthy declaration, preserve the old value and do not
		// turn an older deployment into the model's contract by accident.
		done[row.ModelID] = true
	}
	return nil
}

func legacyOperationsPresent(raw string) bool {
	var operations []json.RawMessage
	return json.Unmarshal([]byte(raw), &operations) == nil && len(operations) > 0
}

func legacyChatOnlyOperations(raw string) bool {
	var operations []struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(raw), &operations) == nil && len(operations) == 1 && operations[0].Type == legacyChatOperationType
}

func legacyNormalizedChatOperations(raw string) bool {
	var operations []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &operations) != nil || len(operations) != 1 {
		return false
	}
	expected := map[string]string{
		"type":             legacyChatOperationType,
		"endpoint":         "/v1/chat/completions",
		"mode":             "stream",
		"request_encoding": "json",
	}
	for key, raw := range operations[0] {
		if key == "inputs" {
			var inputs []json.RawMessage
			if json.Unmarshal(raw, &inputs) != nil || len(inputs) != 0 {
				return false
			}
			continue
		}
		want, ok := expected[key]
		if !ok {
			return false
		}
		var got string
		if json.Unmarshal(raw, &got) != nil || got != want {
			return false
		}
		delete(expected, key)
	}
	return len(expected) == 0
}

func legacyTemplateNonChatOperations(db *gorm.DB, snapshot templateSnapshotAPI) (string, bool, error) {
	if snapshot.TemplateID == 0 || strings.TrimSpace(snapshot.SpecName) == "" ||
		!db.Migrator().HasTable("inference_specs") ||
		!db.Migrator().HasColumn("inference_specs", "model_id") ||
		!db.Migrator().HasColumn("inference_specs", "source") ||
		!db.Migrator().HasColumn("inference_specs", "name") ||
		!db.Migrator().HasColumn("inference_specs", "api_contract") {
		return "", false, nil
	}
	source := strings.TrimSpace(snapshot.SpecSource)
	if source == "" {
		source = "upstream"
	}
	var raw string
	if err := db.Raw("SELECT COALESCE(api_contract, '') FROM inference_specs WHERE model_id = ? AND source = ? AND name = ? LIMIT 1",
		snapshot.TemplateID, source, strings.TrimSpace(snapshot.SpecName)).Scan(&raw).Error; err != nil {
		return "", false, err
	}
	var contract struct {
		Operations json.RawMessage `json:"operations"`
	}
	if json.Unmarshal([]byte(raw), &contract) != nil {
		return "", false, nil //nolint:nilerr // Unreadable historical contracts are excluded from operation recovery.
	}
	operations := string(contract.Operations)
	if !legacyOperationsPresent(operations) || legacyChatOnlyOperations(operations) {
		return "", false, nil
	}
	return operations, true, nil
}

func updateLegacyModelOperations(db *gorm.DB, id uint, previous, operations string) error {
	// A concurrent edit after the scan wins over the migration.
	return db.Table("models").Where("id = ? AND operations = ?", id, previous).
		Update("operations", operations).Error
}
