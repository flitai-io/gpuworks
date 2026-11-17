package statsdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// Opening a third-schema file keys the quality rows on the priority class
// too: the rows it had keep their figures under the empty class, and a second
// open changes nothing.
func TestSchemaV4SplitsQualityByClass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var q rollup.Quality
	q.Requests, q.Completed = 3, 3
	q.TTFT.Observe(200)
	stmts := append(append(append(append([]string{}, schemaV1...), schemaV2...), schemaV3...), `PRAGMA user_version = 3`)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO quality_hourly VALUES (?, 7, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{hourMS(21, 1)}, qualityArgs(q)...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quality_daily VALUES (20261020, 7, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, qualityArgs(q)...); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.Quality(context.Background(), Range{From: hourMS(21, 0), To: hourMS(22, 0)}, 0)
		if err != nil || len(rows) != 1 || rows[0].PriorityClass != "" || rows[0].Requests != 3 || rows[0].TTFT.Count() != 1 {
			t.Fatalf("hour rows = %+v %v", rows, err)
		}
		days, err := s.Quality(context.Background(), Range{Daily: true, From: 20261020, To: 20261021}, 7)
		if err != nil || len(days) != 1 || days[0].PriorityClass != "" || days[0].Requests != 3 {
			t.Fatalf("day rows = %+v %v", days, err)
		}
		_ = s.Close()
	}
}

// The hour and day rows of one deployment keep each class apart.
func TestQualityRowsPerClass(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	var high, low rollup.Quality
	high.Requests, high.Completed = 2, 2
	high.TTFT.Observe(100)
	low.Requests, low.Completed = 5, 4
	low.Error = 1
	low.TTFT.Observe(9000)
	hour := hourMS(21, 16)
	for range 2 {
		if err := s.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{
			{HourMS: hour, DeploymentID: 17, PriorityClass: "high"}: high,
			{HourMS: hour, DeploymentID: 17, PriorityClass: "low"}:  low,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Quality(ctx, Range{From: hour, To: hour + 1}, 17)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v %v", rows, err)
	}
	byClass := map[string]QualityRow{}
	for _, r := range rows {
		byClass[r.PriorityClass] = r
	}
	highRow := byClass["high"]
	if highRow.Requests != 4 || byClass["low"].Requests != 10 || byClass["low"].Error != 2 || highRow.TTFT.Count() != 2 {
		t.Fatalf("by class = %+v", byClass)
	}
	if err := s.RollupDay(ctx, 20261022, shanghai); err != nil {
		t.Fatal(err)
	}
	days, err := s.Quality(ctx, Range{Daily: true, From: 20261022, To: 20261023}, 17)
	if err != nil || len(days) != 2 {
		t.Fatalf("day rows = %+v %v", days, err)
	}
	for _, d := range days {
		if want := byClass[d.PriorityClass].Requests; d.Requests != want {
			t.Fatalf("day %s = %d, want %d", d.PriorityClass, d.Requests, want)
		}
	}
}
