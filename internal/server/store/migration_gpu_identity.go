package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// normalizeGPUIdentity is migration 117. GPU names are stored normalized
// (accelerator.NormalizeGPUName: no leading "NVIDIA " or "Ascend ", single
// spaces) and the architecture as raw facts only, so gpus.arch_family goes:
// the family is derived when read (accelerator.GPUArch). Before the column
// is dropped, the one fact it held that no other column did is kept: a Hygon
// card that named only its series (arch_family K100_FAMILY_AMBIGUOUS, no
// arch_id) gets the series as its arch_id. NVIDIA's arch_id, which repeats the
// compute capability as sm_XY, is cleared where the capability is present.
// The next status report from an agent rewrites the same values, so a row
// the migration and a later ingest both touch agree. A missing table is left
// alone.
func normalizeGPUIdentity(db *gorm.DB) error {
	if !db.Migrator().HasTable("gpus") {
		return nil
	}
	hasFamily, err := tableHasColumn(db, "gpus", "arch_family")
	if err != nil {
		return err
	}
	type gpuIdentityRow struct {
		ID                uint
		Name              string
		Vendor            string
		ArchID            string
		ComputeCapability string
		ArchFamily        string
	}
	columns := "id, name, vendor, arch_id, compute_capability"
	if hasFamily {
		columns += ", arch_family"
	}
	var rows []gpuIdentityRow
	if err := db.Raw("SELECT " + columns + " FROM gpus ORDER BY id").Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		name := accelerator.NormalizeGPUName(row.Name)
		archID := accelerator.StoredArchID(row.Vendor, row.ArchID, row.ComputeCapability)
		if archID == "" && row.ArchFamily == "K100_FAMILY_AMBIGUOUS" && accelerator.NormalizeVendor(row.Vendor) == accelerator.VendorHygon {
			archID = "K100"
		}
		if name == row.Name && archID == row.ArchID {
			continue
		}
		if err := db.Exec("UPDATE gpus SET name = ?, arch_id = ? WHERE id = ?", name, archID, row.ID).Error; err != nil {
			return err
		}
	}
	return dropColumns(db, "gpus", "arch_family")
}
