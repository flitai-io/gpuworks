package logstore

import (
	"fmt"
	"sort"
)

// Append routes entries into the partition of their timestamp's UTC day and
// inserts each day's batch in one transaction. Agents batch ~64 lines every
// couple of seconds, so transaction counts stay far below what the single
// writer can absorb (measured: ~40K rows/s on this schema shape).
func (s *sqliteStore) Append(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	byDay := map[string][]Entry{}
	days := make([]string, 0, 1)
	for _, e := range entries {
		day := partitionDay(e.TimeMS)
		if _, ok := byDay[day]; !ok {
			days = append(days, day)
		}
		byDay[day] = append(byDay[day], e)
	}
	sort.Strings(days) // deterministic order; newest-last so concurrent reads
	// that race this append see older days first, never the reverse gap.

	tx, err := s.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit/statement errors take precedence; a committed transaction is already closed.
	for _, day := range days {
		dayStart, err := partitionStartMS(day)
		if err != nil {
			return err
		}
		if err := s.ensurePartition(tx, dayStart); err != nil {
			return fmt.Errorf("ensure partition %s: %w", day, err)
		}
		name := partitionPrefix + day
		stmt, err := tx.Prepare(fmt.Sprintf(
			`INSERT INTO %s(ts, instance, node, level, source, line) VALUES (?,?,?,?,?,?)`, name))
		if err != nil {
			return err
		}
		for _, e := range byDay[day] {
			if _, err := stmt.Exec(e.TimeMS, e.Instance, e.Node, e.Level, e.Source, e.Line); err != nil {
				stmt.Close()
				return err
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	}
	return tx.Commit()
}
