package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestMixedFrameworkSeriesAreSplitDuringMigration(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open("file:mixed_framework_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&SchemaMigration{}, &ImageSeries{}, &Image{}); err != nil {
		t.Fatal(err)
	}
	series := ImageSeries{Name: "mixed", Engine: "vllm", Purpose: ImagePurposeInference, Framework: "cuda", Source: "official", Enabled: true}
	if err := gdb.Create(&series).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&Image{SeriesID: series.ID, Name: "cuda", Framework: "cuda", ImageRef: "example/cuda:v1", Source: "official", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&Image{SeriesID: series.ID, Name: "cann", Framework: "cann", ImageRef: "example/cann:v1", Source: "official", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	for _, version := range migrations {
		if version.Version == 39 {
			continue
		}
		if err := gdb.Create(&SchemaMigration{Version: version.Version, Name: version.Name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	var rows []Image
	if err := gdb.Order("image_ref").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SeriesID == rows[1].SeriesID {
		t.Fatalf("versions were not split: %+v", rows)
	}
	var seriesRows []ImageSeries
	if err := gdb.Find(&seriesRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(seriesRows) != 2 {
		t.Fatalf("series count = %d, want 2", len(seriesRows))
	}
	for _, row := range seriesRows {
		var version Image
		if err := gdb.Where("series_id = ?", row.ID).First(&version).Error; err != nil {
			t.Fatal(err)
		}
		if row.Framework != version.Framework {
			t.Fatalf("series %q framework=%q version=%q", row.Name, row.Framework, version.Framework)
		}
	}
}
