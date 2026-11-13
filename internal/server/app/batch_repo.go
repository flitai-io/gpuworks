package app

import (
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// batchRepo gives the batch service its files and batches over the store.
type batchRepo struct {
	st *store.Store
}

func (r batchRepo) owner(o batch.Owner) store.BatchOwnerFilter {
	if o.All {
		return store.BatchOwnerFilter{}
	}
	id := o.UserID
	return store.BatchOwnerFilter{UserID: &id}
}

func batchErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return batch.ErrNotFound
	case errors.Is(err, store.ErrBatchFileInUse):
		return batch.ErrFileInUse
	case errors.Is(err, store.ErrBatchTerminal):
		return batch.ErrTerminal
	}
	return err
}

func fileRow(f batch.File) *store.BatchFile {
	return &store.BatchFile{ID: f.ID, UserID: f.UserID, APIKeyID: f.APIKeyID, Purpose: f.Purpose, Filename: f.Filename,
		Bytes: f.Bytes, Lines: f.Lines, Endpoint: f.Endpoint, CreatedAt: f.CreatedAt}
}

func fileOf(f store.BatchFile) batch.File {
	return batch.File{ID: f.ID, UserID: f.UserID, APIKeyID: f.APIKeyID, Purpose: f.Purpose, Filename: f.Filename,
		Bytes: f.Bytes, Lines: f.Lines, Endpoint: f.Endpoint, CreatedAt: f.CreatedAt}
}

func batchOf(b store.Batch, metadata map[string]string) batch.Batch {
	return batch.Batch{
		ID: b.ID, UserID: b.UserID, APIKeyID: b.APIKeyID, Endpoint: b.Endpoint, InputFileID: b.InputFileID,
		OutputFileID: b.OutputFileID, ErrorFileID: b.ErrorFileID, CompletionWindow: b.CompletionWindow, Model: b.ModelOverride, Status: b.Status,
		ErrorCode: b.ErrorCode, ErrorMessage: b.ErrorMessage, Total: b.Total, Completed: b.Completed, Failed: b.Failed,
		CheckpointOffset: b.CheckpointOffset, Metadata: metadata, CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt,
		InProgressAt: b.InProgressAt, FinalizingAt: b.FinalizingAt, CompletedAt: b.CompletedAt, FailedAt: b.FailedAt,
		ExpiredAt: b.ExpiredAt, CancellingAt: b.CancellingAt, CancelledAt: b.CancelledAt,
	}
}

// batchesOf converts rows, reading their metadata in one query.
func (r batchRepo) batchesOf(rows []store.Batch) ([]batch.Batch, error) {
	ids := make([]string, len(rows))
	for i, b := range rows {
		ids[i] = b.ID
	}
	meta, err := r.st.BatchMetadataOf(ids)
	if err != nil {
		return nil, err
	}
	out := make([]batch.Batch, len(rows))
	for i, b := range rows {
		out[i] = batchOf(b, meta[b.ID])
	}
	return out, nil
}

func (r batchRepo) CreateFile(f batch.File) error { return r.st.CreateBatchFile(fileRow(f)) }

func (r batchRepo) GetFile(id string) (batch.File, error) {
	f, err := r.st.GetBatchFile(id)
	if err != nil {
		return batch.File{}, batchErr(err)
	}
	return fileOf(*f), nil
}

func (r batchRepo) ListFiles(o batch.Owner, purpose, after string, limit int) ([]batch.File, error) {
	rows, err := r.st.ListBatchFiles(r.owner(o), purpose, after, limit)
	out := make([]batch.File, len(rows))
	for i, f := range rows {
		out[i] = fileOf(f)
	}
	return out, err
}

func (r batchRepo) DeleteFile(id string) error { return batchErr(r.st.DeleteBatchFile(id)) }

func (r batchRepo) CreateBatch(b batch.Batch) error {
	return r.st.CreateBatch(&store.Batch{
		ID: b.ID, UserID: b.UserID, APIKeyID: b.APIKeyID, Endpoint: b.Endpoint, InputFileID: b.InputFileID,
		CompletionWindow: b.CompletionWindow, ModelOverride: b.Model, Status: b.Status, Total: b.Total, CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt,
	}, b.Metadata)
}

func (r batchRepo) GetBatch(id string) (batch.Batch, error) {
	row, err := r.st.GetBatch(id)
	if err != nil {
		return batch.Batch{}, batchErr(err)
	}
	out, err := r.batchesOf([]store.Batch{*row})
	if err != nil {
		return batch.Batch{}, err
	}
	return out[0], nil
}

func (r batchRepo) ListBatchesAfter(o batch.Owner, after string, limit int) ([]batch.Batch, error) {
	rows, err := r.st.ListBatchesAfter(r.owner(o), after, limit)
	if err != nil {
		return nil, err
	}
	return r.batchesOf(rows)
}

func (r batchRepo) ListBatchesPage(o batch.Owner, statuses []string, offset, limit int) ([]batch.Batch, int, error) {
	rows, total, err := r.st.ListBatchesPage(r.owner(o), statuses, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	out, err := r.batchesOf(rows)
	return out, total, err
}

func (r batchRepo) CountByStatus(o batch.Owner, statuses []string) (map[string]int, error) {
	return r.st.CountBatchesByStatus(r.owner(o), statuses)
}

func (r batchRepo) Unfinished() ([]batch.Batch, error) {
	rows, err := r.st.UnfinishedBatches()
	if err != nil {
		return nil, err
	}
	return r.batchesOf(rows)
}

func (r batchRepo) Start(id string, at time.Time) (bool, error) { return r.st.StartBatch(id, at) }

func (r batchRepo) SaveProgress(id string, p batch.Progress) error {
	return r.st.SaveBatchProgress(id, store.BatchProgress{Completed: p.Completed, Failed: p.Failed, Offset: p.Offset})
}

func (r batchRepo) RequestCancel(id string, at time.Time) (batch.Batch, error) {
	row, err := r.st.RequestBatchCancel(id, at)
	if row == nil {
		return batch.Batch{}, batchErr(err)
	}
	out, merr := r.batchesOf([]store.Batch{*row})
	if merr != nil {
		return batch.Batch{}, merr
	}
	return out[0], batchErr(err)
}

func (r batchRepo) BeginFinalize(id string, at time.Time) error {
	return r.st.BeginBatchFinalize(id, at)
}

func (r batchRepo) Finish(id string, f batch.Finish) error {
	fin := store.BatchFinish{Status: f.Status, At: f.At, Completed: f.Completed, Failed: f.Failed, ErrorCode: f.ErrorCode, ErrorMessage: f.ErrorMessage}
	if f.Output != nil {
		fin.Output = fileRow(*f.Output)
	}
	if f.Error != nil {
		fin.Error = fileRow(*f.Error)
	}
	return batchErr(r.st.FinishBatch(id, fin))
}

func (r batchRepo) DeleteOutputsBefore(cutoff time.Time) ([]string, error) {
	return r.st.DeleteBatchOutputsBefore(cutoff)
}
