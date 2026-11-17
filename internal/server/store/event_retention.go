package store

import (
	"context"
	"time"
)

// The event timeline is trimmed on the one connection SQLite lets write, the
// one every control-plane write waits for. So the trim never holds it for
// long: rows go in small batches, each its own statement, and the reads that
// decide what to trim go to the read pool, which WAL lets run beside a write.
// Measured on a 1M-event table (about 600 MB) before this was so: the byte
// estimate alone held every write for 2.1 s, and deleting half the table by
// age in one statement for 47 s; after, the estimate held writes for 7 ms at
// most, and the age delete for 0.8 s at most, over a longer sweep. A longer
// sweep stops between batches once its context ends, so it never holds up a
// shutdown; the next sweep picks up the rest.

// eventRetentionBatch is how many events one delete removes; between deletes a
// waiting write goes first. Measured deleting 200k of a 1M-event table, a
// write during the sweep waited p50 92 ms / p99 371 ms with batches of 500,
// 39 / 121 ms with 200 and 10 / 271 ms with 100, at 4391, 4080 and 3518
// rows/s; the longest wait stayed about half a second at every size. A test
// lowers it.
var eventRetentionBatch = 200

// eventSizeExpr is the byte budget's estimate of an event: the length of its
// text columns, in characters. It ignores row overhead, which is irrelevant at
// the granularity the budget is set at (tens of gigabytes). A column an older
// release left NULL (code and data came with a later migration) counts as
// empty rather than making the whole row's size NULL, which SUM would skip.
// The total and what a trim frees are both counted with it, so the two agree.
const eventSizeExpr = "LENGTH(COALESCE(message, '')) + LENGTH(COALESCE(data, '')) + LENGTH(COALESCE(code, ''))"

// DeleteEventsBefore trims the event table. Time and byte thresholds both apply
// and whichever trips first wins: a burst of failures can outgrow a byte budget
// long before it is old, and an old backlog can outlive its usefulness long
// before it is large. It stops between batches once ctx ends, returning what
// it deleted so far and ctx's error.
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time, maxBytes int64) (int64, error) {
	var deleted int64
	if !cutoff.IsZero() {
		n, err := s.deleteEventsOlderThan(ctx, cutoff.UTC())
		deleted += n
		if err != nil {
			return deleted, err
		}
	}
	if maxBytes <= 0 {
		return deleted, nil
	}
	n, err := s.trimEventsToBytes(ctx, maxBytes)
	return deleted + n, err
}

// batchDone reports a deleted batch to the test hook.
func (s *Store) batchDone(n int64) {
	if s.eventRetentionBatchHook != nil {
		s.eventRetentionBatchHook(n)
	}
}

// deleteEventsOlderThan deletes the events created before cutoff, a batch per
// statement: the subquery walks the created_at index for at most a batch of
// ids, so each statement holds the write connection only for its batch.
func (s *Store) deleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		res := s.db.WithContext(ctx).Exec("DELETE FROM instance_events WHERE id IN (SELECT id FROM instance_events WHERE created_at < ? LIMIT ?)", cutoff, eventRetentionBatch)
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += res.RowsAffected
		if res.RowsAffected == 0 {
			return deleted, nil
		}
		s.batchDone(res.RowsAffected)
		if res.RowsAffected < int64(eventRetentionBatch) {
			return deleted, nil
		}
	}
}

// eventSize is an event's id and its size by eventSizeExpr.
type eventSize struct {
	ID   uint
	Size int64
}

// readEventsSize reads the table's size by eventSizeExpr on the read pool.
func (s *Store) readEventsSize(ctx context.Context) (int64, error) {
	var total int64
	err := s.reader().WithContext(ctx).Model(&InstanceEvent{}).
		Select("COALESCE(SUM(" + eventSizeExpr + "), 0)").Scan(&total).Error
	return total, err
}

// trimEventsToBytes deletes the oldest events until the table's estimated
// size fits maxBytes. SQLite has no cheap per-table byte counter, so the
// estimate is a scan; it and the choice of the oldest events are read on the
// read pool, and only the deletes take the write connection.
//
// Other writes going on meanwhile leave the running total safe. Events
// written while it runs are newer than every event it counted, and it stops on
// the total of those, so it never reaches them. Events are deleted nowhere
// else today (they outlive their deployment); one that did go between a
// batch's read and its delete would only make the total read high, so the
// trim deletes a little more, never stops over budget.
func (s *Store) trimEventsToBytes(ctx context.Context, maxBytes int64) (int64, error) {
	total, err := s.readEventsSize(ctx)
	if err != nil {
		return 0, err
	}
	var deleted int64
	var after uint
	for total > maxBytes {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		var batch []eventSize
		if err := s.reader().WithContext(ctx).Model(&InstanceEvent{}).
			Select("id, "+eventSizeExpr+" AS size").
			Where("id > ?", after).Order("id ASC").Limit(eventRetentionBatch).
			Scan(&batch).Error; err != nil {
			return deleted, err
		}
		if len(batch) == 0 {
			break
		}
		// Only as many of the oldest as bring the table within the budget.
		ids := make([]uint, 0, len(batch))
		var freed int64
		for _, e := range batch {
			if total-freed <= maxBytes {
				break
			}
			ids = append(ids, e.ID)
			freed += e.Size
		}
		after = batch[len(batch)-1].ID
		res := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&InstanceEvent{})
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += res.RowsAffected
		total -= freed
		s.batchDone(res.RowsAffected)
	}
	return deleted, nil
}
