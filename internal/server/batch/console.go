package batch

import (
	"errors"
	"io"
	"mime/multipart"
	"os"
	"strings"
	"time"
)

// CreateFromUpload creates a batch from the console's form: a "file" with the
// JSONL requests and optionally, in any order, an "endpoint" (the url the
// file's requests ask for when absent), a "completion_window"
// (CompletionWindow by default, MinWindowHours to MaxWindowHours) and a
// "model" that every request is sent to and a "priority_class" its requests
// run at. The file is validated while it is
// saved; one that is refused, or an endpoint it does
// not fit, leaves nothing behind.
func (s *Service) CreateFromUpload(owner Owner, mr *multipart.Reader) (Batch, error) {
	var (
		endpoint, window, model, class string
		saved                          *File
		stats                          inputStats
	)
	window = CompletionWindow
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.discard(owner, saved)
			return Batch{}, invalid("invalid_multipart", "", "the multipart body cannot be read: "+err.Error())
		}
		switch part.FormName() {
		case "endpoint", "completion_window", "model", "priority_class":
			raw, _ := io.ReadAll(io.LimitReader(part, 256))
			switch part.FormName() {
			case "endpoint":
				endpoint = strings.TrimSpace(string(raw))
			case "model":
				model = strings.TrimSpace(string(raw))
			case "priority_class":
				class = strings.TrimSpace(string(raw))
			default:
				window = strings.TrimSpace(string(raw))
			}
		case "file":
			if saved != nil {
				s.discard(owner, saved)
				return Batch{}, invalid("invalid_multipart", "file", "the request has more than one file")
			}
			f, st, err := s.upload(owner, part.FileName(), part)
			if err != nil {
				return Batch{}, err
			}
			saved, stats = &f, st
		}
	}
	if saved == nil {
		return Batch{}, invalid("missing_required_parameter", "file", "file is required")
	}
	if endpoint == "" {
		if stats.MixedLine > 0 {
			s.discard(owner, saved)
			return Batch{}, lineError(stats.MixedLine, "mixed_endpoints", "url", "the url differs from the first request's; a batch calls one endpoint")
		}
		endpoint = saved.Endpoint
	}
	b, err := s.CreateBatch(owner, CreateRequest{InputFileID: saved.ID, Endpoint: endpoint, CompletionWindow: window, Model: model, PriorityClass: class})
	if err != nil {
		s.discard(owner, saved)
		return Batch{}, err
	}
	return b, nil
}

// OpenBatchFile opens the input, output or error file of a batch for its
// owner: kind is "input", "output" or "error". The file's metadata comes
// with it.
func (s *Service) OpenBatchFile(owner Owner, batchID, kind string) (File, *os.File, error) {
	b, err := s.Batch(owner, batchID)
	if err != nil {
		return File{}, nil, err
	}
	id := map[string]string{"input": b.InputFileID, "output": b.OutputFileID, "error": b.ErrorFileID}[kind]
	if id == "" {
		return File{}, nil, notFound(CodeFileNotFound, "The batch has no "+kind+" file")
	}
	// The files of a batch are its owner's by definition, whoever asks.
	return s.OpenFile(Owner{All: true}, id)
}

// FileOf reads a file's metadata without checking who asks; for a caller
// that has checked access through the batch that names it.
func (s *Service) FileOf(id string) (File, error) {
	f, err := s.repo.GetFile(id)
	if errors.Is(err, ErrNotFound) {
		return File{}, notFound(CodeFileNotFound, "No such File object: "+id)
	}
	return f, err
}

// OutputExpiresAt is when the output and error files of a batch are removed:
// the retention after the batch ended, nil while it runs or when it wrote
// neither file.
func (s *Service) OutputExpiresAt(b Batch) *time.Time {
	if b.OutputFileID == "" && b.ErrorFileID == "" {
		return nil
	}
	for _, ended := range []*time.Time{b.CompletedAt, b.CancelledAt, b.ExpiredAt, b.FailedAt} {
		if ended != nil {
			at := ended.Add(s.cfg.OutputRetention)
			return &at
		}
	}
	return nil
}
