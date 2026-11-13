package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

type benchmarkRunBeforeCurveSnapshot struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (benchmarkRunBeforeCurveSnapshot) TableName() string { return "benchmark_runs" }

type benchmarkPointBeforeCurve struct {
	ID        uint `gorm:"primaryKey"`
	RunID     uint
	Seq       int
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (benchmarkPointBeforeCurve) TableName() string { return "benchmark_points" }

type benchmarkBaselineBeforeRemoval struct {
	ID          uint `gorm:"primaryKey"`
	Source      string
	Label       string
	Model       string
	Hardware    string
	ISL         int
	OSL         int
	Concurrency int
}

func (benchmarkBaselineBeforeRemoval) TableName() string { return "benchmark_baselines" }

func TestBenchmarkMigrationsPreserveRunsAndRemoveBaselines(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "benchmark-v56.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&SchemaMigration{}, &benchmarkRunBeforeCurveSnapshot{}, &benchmarkPointBeforeCurve{}, &benchmarkBaselineBeforeRemoval{}); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 56; version++ {
		if err := database.Create(&SchemaMigration{Version: version, Name: "already_applied", AppliedAt: time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	createdAt := time.Now().UTC().Add(-time.Hour)
	if err := database.Create(&benchmarkRunBeforeCurveSnapshot{ID: 41, Name: "historical", State: "done", CreatedAt: createdAt, UpdatedAt: createdAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&benchmarkPointBeforeCurve{ID: 52, RunID: 41, Seq: 1, State: "done", CreatedAt: createdAt, UpdatedAt: createdAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&benchmarkBaselineBeforeRemoval{ID: 63, Source: "historical", Label: "removed by M28", Model: "legacy", Hardware: "legacy", ISL: 1, OSL: 1, Concurrency: 1}).Error; err != nil {
		t.Fatal(err)
	}

	st := &Store{db: database, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	var curveMigration SchemaMigration
	if err := database.First(&curveMigration, "version = ?", 58).Error; err != nil {
		t.Fatal(err)
	}
	if curveMigration.Name != "benchmark_curve_and_target_snapshot" {
		t.Fatalf("migration 58 = %+v", curveMigration)
	}
	var removalMigration SchemaMigration
	if err := database.First(&removalMigration, "version = ?", 59).Error; err != nil {
		t.Fatal(err)
	}
	if removalMigration.Name != "remove_benchmark_baselines" {
		t.Fatalf("migration 59 = %+v", removalMigration)
	}
	run, err := st.GetBenchmarkRun(41)
	if err != nil {
		t.Fatal(err)
	}
	if run.Name != "historical" || run.TargetSnapshot != "" || run.Warnings != "" {
		t.Fatalf("historical run after migration = %+v", run)
	}
	points, err := st.ListBenchmarkPoints(41)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Curve != "" || points[0].State != "done" {
		t.Fatalf("historical points after migration = %+v", points)
	}
	if database.Migrator().HasTable("benchmark_baselines") {
		t.Fatal("migration 59 kept the historical benchmark_baselines table")
	}
}
