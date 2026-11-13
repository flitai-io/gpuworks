package logstore

import (
	"database/sql"
	"fmt"
)

// migrateLegacy converts a v1 store — a single plain FTS5 virtual table named
// `logs` with columns (line, instance, node UNINDEXED) — into partitioned v2,
// inside one transaction: either the legacy table disappears with all its rows
// copied into day partitions, or nothing changes. Reopening after a crash
// mid-migration therefore just redoes it; rows cannot be duplicated.
func (s *sqliteStore) migrateLegacy() error {
	var name string
	err := s.writer.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='logs'`).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := s.writer.Query(`SELECT rowid, ts, instance, node, line FROM logs ORDER BY rowid`)
	if err != nil {
		return err
	}
	byDay := map[string][]Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TimeMS, &e.Instance, &e.Node, &e.Line); err != nil {
			rows.Close()
			return err
		}
		day := partitionDay(e.TimeMS)
		byDay[day] = append(byDay[day], e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	tx, err := s.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit/statement errors take precedence; a committed transaction is already closed.
	for day, entries := range byDay {
		dayStart, err := partitionStartMS(day)
		if err != nil {
			return err
		}
		if err := ensurePartition(tx, dayStart); err != nil {
			return err
		}
		name := partitionPrefix + day
		stmt, err := tx.Prepare(fmt.Sprintf(
			`INSERT INTO %s(id, ts, instance, node, level, source, line) VALUES (?,?,?,?,?,?,?)`, name))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := stmt.Exec(e.ID, e.TimeMS, e.Instance, e.Node, e.Level, SourceInstance, e.Line); err != nil {
				stmt.Close()
				return err
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE logs`); err != nil {
		return err
	}
	return tx.Commit()
}
