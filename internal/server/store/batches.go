package store

import (
	"errors"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Errors of the batch tables.
var (
	// ErrBatchFileInUse: a batch that has not finished still reads the file.
	ErrBatchFileInUse = errors.New("a batch that has not finished uses the file")
	// ErrBatchTerminal: the batch has finished, so the change is refused.
	ErrBatchTerminal = errors.New("the batch has finished")
)

// BatchOwnerFilter narrows a listing to the rows of one user; nil lists every
// user's rows (the console's administrators).
type BatchOwnerFilter struct {
	UserID *uint
}

func (f BatchOwnerFilter) apply(db *gorm.DB) *gorm.DB {
	if f.UserID != nil {
		return db.Where("user_id = ?", *f.UserID)
	}
	return db
}

func createBatchTables(db *gorm.DB) error {
	return db.AutoMigrate(&BatchFile{}, &Batch{}, &BatchMetadata{})
}

// CreateBatchFile records an uploaded or written file.
func (s *Store) CreateBatchFile(f *BatchFile) error {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}
	return s.db.Create(f).Error
}

// GetBatchFile reads one file's row, ErrNotFound when there is none.
func (s *Store) GetBatchFile(id string) (*BatchFile, error) {
	var f BatchFile
	err := s.reader().Where("id = ?", id).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &f, err
}

// ListBatchFiles lists files newest first, those of purpose when it is set,
// starting after the file named by after when it is set. It returns at most
// limit rows.
func (s *Store) ListBatchFiles(owner BatchOwnerFilter, purpose, after string, limit int) ([]BatchFile, error) {
	q := owner.apply(s.reader().Model(&BatchFile{}))
	if purpose != "" {
		q = q.Where("purpose = ?", purpose)
	}
	if after != "" {
		var anchor BatchFile
		if err := s.reader().Where("id = ?", after).First(&anchor).Error; err == nil {
			q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", anchor.CreatedAt, anchor.CreatedAt, anchor.ID)
		}
	}
	var out []BatchFile
	err := q.Order("created_at desc, id desc").Limit(limit).Find(&out).Error
	return out, err
}

// DeleteBatchFile removes a file's row. A file a batch that has not finished
// reads is kept: ErrBatchFileInUse.
func (s *Store) DeleteBatchFile(id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var busy int64
		if err := tx.Model(&Batch{}).
			Where("input_file_id = ? AND status NOT IN ?", id, []string{BatchFailed, BatchCompleted, BatchExpired, BatchCancelled}).
			Count(&busy).Error; err != nil {
			return err
		}
		if busy > 0 {
			return ErrBatchFileInUse
		}
		res := tx.Where("id = ?", id).Delete(&BatchFile{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CreateBatch records a new batch and its metadata in one transaction and
// announces it once committed.
func (s *Store) CreateBatch(b *Batch, metadata map[string]string) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(b).Error; err != nil {
			return err
		}
		if len(metadata) == 0 {
			return nil
		}
		rows := make([]BatchMetadata, 0, len(metadata))
		for k, v := range metadata {
			rows = append(rows, BatchMetadata{BatchID: b.ID, Key: k, Value: v})
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("batch", b.ID, b.Status)
	return nil
}

// GetBatch reads one batch, ErrNotFound when there is none.
func (s *Store) GetBatch(id string) (*Batch, error) {
	var b Batch
	err := s.reader().Where("id = ?", id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &b, err
}

// BatchMetadataOf reads the metadata of the named batches in one query.
func (s *Store) BatchMetadataOf(ids []string) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []BatchMetadata
	if err := s.reader().Where("batch_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		if out[r.BatchID] == nil {
			out[r.BatchID] = map[string]string{}
		}
		out[r.BatchID][r.Key] = r.Value
	}
	return out, nil
}

// ListBatchesAfter lists batches newest first, starting after the batch named
// by after when it is set, at most limit rows (the OpenAI cursor listing).
func (s *Store) ListBatchesAfter(owner BatchOwnerFilter, after string, limit int) ([]Batch, error) {
	q := owner.apply(s.reader().Model(&Batch{}))
	if after != "" {
		var anchor Batch
		if err := s.reader().Where("id = ?", after).First(&anchor).Error; err == nil {
			q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", anchor.CreatedAt, anchor.CreatedAt, anchor.ID)
		}
	}
	var out []Batch
	err := q.Order("created_at desc, id desc").Limit(limit).Find(&out).Error
	return out, err
}

// ListBatchesPage lists one page of batches newest first with the number of
// batches that match, those in one of statuses when any is given (the
// console's list).
func (s *Store) ListBatchesPage(owner BatchOwnerFilter, statuses []string, offset, limit int) ([]Batch, int, error) {
	q := owner.apply(s.reader().Model(&Batch{}))
	if len(statuses) > 0 {
		q = q.Where("status IN ?", statuses)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []Batch
	err := q.Order("created_at desc, id desc").Offset(offset).Limit(limit).Find(&out).Error
	return out, int(total), err
}

// CountBatchesByStatus counts the owner's batches in each of the statuses,
// in one grouped count query; a status with none is absent.
func (s *Store) CountBatchesByStatus(owner BatchOwnerFilter, statuses []string) (map[string]int, error) {
	var rows []struct {
		Status string
		N      int
	}
	err := owner.apply(s.reader().Model(&Batch{})).Where("status IN ?", statuses).
		Select("status, COUNT(*) AS n").Group("status").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Status] = r.N
	}
	return out, nil
}

// UnfinishedBatches lists the batches the executor must run or resume: every
// one that is not in a terminal status, oldest first.
func (s *Store) UnfinishedBatches() ([]Batch, error) {
	var out []Batch
	err := s.reader().
		Where("status IN ?", []string{BatchValidating, BatchInProgress, BatchFinalizing, BatchCancelling}).
		Find(&out).Error
	// The unfinished set is small and read once at start, so it is sorted
	// here rather than by the database.
	slices.SortFunc(out, func(a, b Batch) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, err
}

// StartBatch moves a validated batch to in_progress; false when it was
// already past validating (a resumed batch, or one cancelled meanwhile).
func (s *Store) StartBatch(id string, at time.Time) (bool, error) {
	res := s.db.Model(&Batch{}).Where("id = ? AND status = ?", id, BatchValidating).
		Updates(map[string]any{"status": BatchInProgress, "in_progress_at": at.UTC()})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("batch", id, BatchInProgress)
	}
	return res.RowsAffected > 0, nil
}

// BatchProgress is the executor's checkpoint: how many requests have a result
// and the byte length of the partial result file that holds them.
type BatchProgress struct {
	Completed, Failed int
	Offset            int64
}

// SaveBatchProgress writes a checkpoint. It is one update per checkpoint,
// never one per request, and announces nothing: the console reads progress by
// polling.
func (s *Store) SaveBatchProgress(id string, p BatchProgress) error {
	return s.db.Model(&Batch{}).
		Where("id = ? AND status IN ?", id, []string{BatchInProgress, BatchFinalizing, BatchCancelling}).
		Updates(map[string]any{"completed": p.Completed, "failed": p.Failed, "checkpoint_offset": p.Offset}).Error
}

// RequestBatchCancel moves a batch that has not finished to cancelling and
// returns it; a batch already cancelling is returned as it is, a finished one
// is ErrBatchTerminal.
func (s *Store) RequestBatchCancel(id string, at time.Time) (*Batch, error) {
	res := s.db.Model(&Batch{}).
		Where("id = ? AND status IN ?", id, []string{BatchValidating, BatchInProgress}).
		Updates(map[string]any{"status": BatchCancelling, "cancelling_at": at.UTC()})
	if res.Error != nil {
		return nil, res.Error
	}
	b, err := s.GetBatchFresh(id)
	if err != nil {
		return nil, err
	}
	if BatchTerminal(b.Status) {
		return b, ErrBatchTerminal
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("batch", id, BatchCancelling)
	}
	return b, nil
}

// GetBatchFresh reads a batch through the writer connection, for a read that
// must see the write just made.
func (s *Store) GetBatchFresh(id string) (*Batch, error) {
	var b Batch
	err := s.db.Where("id = ?", id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &b, err
}

// BeginBatchFinalize moves an in_progress batch to finalizing, when the
// executor starts assembling its files; a cancelling batch keeps its status.
func (s *Store) BeginBatchFinalize(id string, at time.Time) error {
	res := s.db.Model(&Batch{}).Where("id = ? AND status = ?", id, BatchInProgress).
		Updates(map[string]any{"status": BatchFinalizing, "finalizing_at": at.UTC()})
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("batch", id, BatchFinalizing)
	}
	return res.Error
}

// BatchFinish is how a batch ends: its terminal status, the final counts and
// the output and error files written for it (nil for none).
type BatchFinish struct {
	Status       string
	At           time.Time
	Completed    int
	Failed       int
	ErrorCode    string
	ErrorMessage string
	Output       *BatchFile
	Error        *BatchFile
}

// FinishBatch ends a batch that has not finished: the files' rows and the
// batch's terminal status are written in one transaction, announced once
// committed. A batch that finished already is ErrBatchTerminal.
func (s *Store) FinishBatch(id string, f BatchFinish) error {
	at := f.At.UTC()
	column := map[string]string{
		BatchCompleted: "completed_at", BatchFailed: "failed_at",
		BatchExpired: "expired_at", BatchCancelled: "cancelled_at",
	}[f.Status]
	if column == "" {
		return errors.New("not a terminal batch status: " + f.Status)
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var b Batch
		if err := tx.Where("id = ?", id).First(&b).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if BatchTerminal(b.Status) {
			return ErrBatchTerminal
		}
		update := map[string]any{
			"status": f.Status, column: at, "completed": f.Completed, "failed": f.Failed,
			"error_code": f.ErrorCode, "error_message": f.ErrorMessage,
		}
		for _, file := range []*BatchFile{f.Output, f.Error} {
			if file == nil {
				continue
			}
			if file.CreatedAt.IsZero() {
				file.CreatedAt = at
			}
			if err := tx.Create(file).Error; err != nil {
				return err
			}
		}
		if f.Output != nil {
			update["output_file_id"] = f.Output.ID
		}
		if f.Error != nil {
			update["error_file_id"] = f.Error.ID
		}
		return tx.Model(&Batch{}).Where("id = ?", id).Updates(update).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("batch", id, f.Status)
	return nil
}

// DeleteBatchOutputsBefore removes the rows of the output and error files
// written before cutoff and returns their ids, for the caller to remove the
// bytes. The batch keeps naming the file; a read of it answers not found.
func (s *Store) DeleteBatchOutputsBefore(cutoff time.Time) ([]string, error) {
	var ids []string
	if err := s.db.Model(&BatchFile{}).
		Where("purpose = ? AND created_at < ?", BatchFilePurposeOutput, cutoff.UTC()).
		Pluck("id", &ids).Error; err != nil || len(ids) == 0 {
		return nil, err
	}
	if err := s.db.Where("id IN ?", ids).Delete(&BatchFile{}).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
