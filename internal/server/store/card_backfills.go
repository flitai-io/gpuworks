package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// One-time backfills for the card columns the migrations added, kept apart
// from the live report path so neither is read as the other.
//
// They call today's rules (cardIdent, accelerator.CardHealthLevel) rather than frozen
// copies of the rules as they were written. That is the deliberate choice:
// a frozen copy is another place the "can this card be used" rule lives,
// which is what this work went to some length to remove, and the value a
// backfill writes survives at most one status report before every card is
// rewritten from its node's next report anyway.

// backfillCardIdents gives every stored card the key it is now addressed
// by. A card keeps whatever identity its probe reported; one that reported
// none keeps behaving as it did, keyed by its device index.
func backfillCardIdents(db *gorm.DB) error {
	var rows []GPU
	if err := db.Select("ID", "UUID", "PCIeBDF", "Index").Where("ident = '' OR ident IS NULL").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		ident := cardIdent(proto.GPUInfo{UUID: row.UUID, PCIeBDF: row.PCIeBDF, Index: row.Index})
		if err := db.Model(&GPU{}).Where("id = ?", row.ID).Update("Ident", ident).Error; err != nil {
			return err
		}
	}
	return nil
}

// backfillCardHealthLevels gives stored cards the normalized health level
// that is now decided when a card is written. Reading the raw fields is the
// only way to recover it: the readers that used to derive it are gone.
func backfillCardHealthLevels(db *gorm.DB) error {
	// Healthy is read into a shape of its own: GPU has no field for it any
	// more (a card's health is its level and its ownership, and this
	// backfill is what makes that true of every stored row). A database
	// created after the column was dropped never had it, and its rows
	// carry the level already.
	columns := []string{"id", "ownership", "health_status"}
	legacy, err := tableHasColumn(db, "gpus", "healthy")
	if err != nil {
		return err
	}
	if legacy {
		columns = append(columns, "healthy")
	}
	var rows []struct {
		GPU
		Healthy bool
	}
	if err := db.Model(&GPU{}).Select(columns).Find(&rows).Error; err != nil {
		return err
	}
	for _, read := range rows {
		// A row with no stored boolean is healthy unless its level says
		// otherwise, which CardHealthLevel answers for.
		row, healthy := read.GPU, read.Healthy || !legacy
		level := accelerator.CardHealthLevel(row.Ownership, row.HealthStatus, healthy)
		if err := db.Model(&GPU{}).Where("id = ?", row.ID).Update("health_status", level).Error; err != nil {
			return err
		}
	}
	return nil
}
