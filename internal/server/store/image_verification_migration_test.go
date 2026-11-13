package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestImageIDVerificationColumnsAreDropped(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open("file:image_verification_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&SchemaMigration{}, &ImageSeries{}, &ImageNode{}); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`CREATE TABLE image_versions (id INTEGER PRIMARY KEY, image_ref TEXT, expected_image_id TEXT, verify_state TEXT, verify_message TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 37; version++ {
		if err := gdb.Create(&SchemaMigration{Version: version, Name: "legacy"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"expected_image_id", "verify_state", "verify_message"} {
		if gdb.Migrator().HasColumn(&Image{}, column) {
			t.Fatalf("legacy column %s still exists", column)
		}
	}
}
