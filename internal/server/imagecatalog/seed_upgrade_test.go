package imagecatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The server seeds the official catalog on every start, writing each series'
// common_args/common_env. A database from the benchmark branch (37, 38 and 40
// recorded under benchmark names) lacked those columns, so the start failed.
func TestSyncSeedAfterBenchmarkLineageV40Upgrade(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "store", "testdata", "benchmark_lineage_v40.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		if err := db.Exec(line).Error; err != nil {
			t.Fatalf("fixture statement %q: %v", line, err)
		}
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatalf("open benchmark-lineage database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	result, err := SyncSeed(st)
	if err != nil {
		t.Fatalf("seed after upgrade: %v", err)
	}
	if result.Created == 0 {
		t.Fatalf("seed created no series or images: %+v", result)
	}
	series, err := st.ListImageSeries()
	if err != nil {
		t.Fatal(err)
	}
	withArgs := 0
	for _, row := range series {
		if row.CommonArgs != "" && row.CommonArgs != "[]" {
			withArgs++
		}
	}
	if withArgs == 0 {
		t.Fatalf("no seeded series kept its common args across %d series", len(series))
	}
}
