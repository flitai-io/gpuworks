package store

import (
	"testing"

	"gorm.io/gorm"
)

func TestEffectiveStorageForNodeReadsOnlyItsTargets(t *testing.T) {
	f := newStorageFixture(t)
	targets := []StorageNode{{StorageID: f.cache.ID, NodeID: f.n1.ID, Path: "/chosen"}, {StorageID: f.cache.ID, LabelKey: "pool", LabelValue: "a"}}
	for i := uint(100); i < 300; i++ {
		targets = append(targets, StorageNode{StorageID: f.cache.ID, NodeID: i, Path: "/unrelated"})
	}
	if err := f.st.db.Create(&targets).Error; err != nil {
		t.Fatal(err)
	}
	var read int64
	if err := f.st.db.Callback().Query().After("gorm:query").Register("audit_storage_targets", func(tx *gorm.DB) {
		if tx.Statement.Table == "storage_nodes" {
			read += tx.RowsAffected
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer f.st.db.Callback().Query().Remove("audit_storage_targets")
	eff, ok, err := f.st.EffectiveStorageForNode(f.cache, f.n1)
	if err != nil || !ok || eff.Path != "/chosen" {
		t.Fatalf("effective storage: %+v %v %v", eff, ok, err)
	}
	if read != 2 {
		t.Fatalf("read %d targets for one node; want its override and label target only", read)
	}
}

func TestEffectiveStorageForNodePreservesLegacyOverrideNormalization(t *testing.T) {
	f := newStorageFixture(t)
	cap := int64(200)
	rows := []StorageNode{
		{StorageID: f.cache.ID, NodeID: f.n1.ID, Path: " /legacy/mount ", CachePath: " /old/cache ", Existing: true},
		{StorageID: f.cache.ID, NodeID: f.n1.ID, LabelKey: "legacy", CachePath: " /effective/cache ", CacheMaxBytes: &cap, OverrideOnly: true},
	}
	if err := f.st.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	eff, ok, err := f.st.EffectiveStorageForNode(f.cache, f.n1)
	if err != nil || !ok || eff.Path != "/legacy/mount" || eff.CachePath != "/effective/cache" || eff.CacheMaxBytes != cap || eff.Existing {
		t.Fatalf("legacy overrides: %+v applies=%v err=%v", eff, ok, err)
	}
}
