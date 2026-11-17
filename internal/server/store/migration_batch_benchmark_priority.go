package store

import "gorm.io/gorm"

// addBatchAndBenchmarkPriorityClass adds the request priority class a batch
// runs at and the one an evaluation run asks for. Existing rows keep the
// empty name: a batch runs at the batch class as before, and a run asked
// for no class.
func addBatchAndBenchmarkPriorityClass(db *gorm.DB) error {
	for _, table := range []any{&Batch{}, &BenchmarkRun{}} {
		if !db.Migrator().HasTable(table) || db.Migrator().HasColumn(table, "PriorityClass") {
			continue
		}
		if err := db.Migrator().AddColumn(table, "PriorityClass"); err != nil {
			return err
		}
	}
	return nil
}
