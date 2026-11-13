package app

import (
	"errors"
	"mime/multipart"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// batchConsole is the console's side of the Batch API (api.BatchService) over
// the batch service.
type batchConsole struct {
	svc *batch.Service
}

func consoleOwner(c api.BatchCaller) batch.Owner {
	return batch.Owner{UserID: c.UserID, All: c.All}
}

func (c batchConsole) List(caller api.BatchCaller, states []string, page, pageSize int) (dto.BatchPage, error) {
	rows, total, err := c.svc.ListBatchesPage(consoleOwner(caller), states, (page-1)*pageSize, pageSize)
	if err != nil {
		return dto.BatchPage{}, consoleBatchError(err)
	}
	items := make([]dto.Batch, 0, len(rows))
	for _, b := range rows {
		items = append(items, c.batchDTO(b, ""))
	}
	return dto.BatchPage{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

var _ api.BatchCounter = batchConsole{}

func (c batchConsole) CountByStatus(caller api.BatchCaller, states []string) (map[string]int, error) {
	counts, err := c.svc.CountByStatus(consoleOwner(caller), states)
	if err != nil {
		return nil, consoleBatchError(err)
	}
	return counts, nil
}

func (c batchConsole) Get(caller api.BatchCaller, id string) (dto.Batch, error) {
	b, err := c.svc.Batch(consoleOwner(caller), id)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	out := c.detail(b)
	out.Waiting = c.waiting(b.ID)
	return out, nil
}

// waiting is why the requests of a running batch are being retried, each
// model with its state now: a few models at most, each looked up once.
func (c batchConsole) waiting(id string) []dto.BatchModelWait {
	waits := c.svc.Waits(id)
	if len(waits) == 0 {
		return nil
	}
	out := make([]dto.BatchModelWait, 0, len(waits))
	for _, w := range waits {
		item := dto.BatchModelWait{Model: w.Model, Status: w.Status, Detail: w.Message, Since: w.Since.UTC(), Requests: w.Requests}
		if entry, err := c.svc.LookupModel(w.Model); err == nil {
			item.State = batchModelStates[entry.State]
		}
		out = append(out, item)
	}
	return out
}

func (c batchConsole) Cancel(caller api.BatchCaller, id string) (dto.Batch, error) {
	b, err := c.svc.Cancel(consoleOwner(caller), id)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	return c.detail(b), nil
}

func (c batchConsole) CreateFromUpload(caller api.BatchCaller, form *multipart.Reader) (dto.Batch, error) {
	b, err := c.svc.CreateFromUpload(consoleOwner(caller), form)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	return c.detail(b), nil
}

func (c batchConsole) Download(caller api.BatchCaller, id, kind string) (api.BatchDownload, error) {
	f, fh, err := c.svc.OpenBatchFile(consoleOwner(caller), id, kind)
	if err != nil {
		return api.BatchDownload{}, consoleBatchError(err)
	}
	return api.BatchDownload{Filename: f.Filename, Size: f.Bytes, Body: fh}, nil
}

// Dictionary is the batch service's dictionary as the console reads it.
func (c batchConsole) Dictionary() (dto.BatchDictionary, error) {
	d, err := c.svc.Dictionary()
	if err != nil {
		return dto.BatchDictionary{}, err
	}
	out := dto.BatchDictionary{
		Endpoints:             d.Endpoints,
		CompletionWindowHours: dto.BatchWindowHours{Min: d.MinWindowHours, Max: d.MaxWindowHours, Default: d.DefaultWindowHours},
		MaxFileBytes:          d.MaxFileBytes, MaxFileLines: d.MaxFileLines, Statuses: d.Statuses,
		OutputRetentionDays: int(d.OutputRetention / (24 * time.Hour)),
		Models:              make([]dto.BatchModel, 0, len(d.Models)),
	}
	for _, m := range d.Models {
		if state, ok := batchModelStates[m.State]; ok {
			endpoints := append([]string{}, m.Endpoints...)
			out.Models = append(out.Models, dto.BatchModel{Name: m.Name, State: state, Ready: m.State.Ready(), Endpoints: endpoints})
		}
	}
	return out, nil
}

// batchModelStates are the wire names of the states a batch can be sent to.
var batchModelStates = map[batch.ModelState]string{
	batch.ModelRunning:   dto.BatchModelRunning,
	batch.ModelAsleep:    dto.BatchModelAsleep,
	batch.ModelStarting:  dto.BatchModelStarting,
	batch.ModelSleepHeld: dto.BatchModelAsleepHeld,
	batch.ModelOffline:   dto.BatchModelOffline,
	batch.ModelFailed:    dto.BatchModelFailed,
	batch.ModelStopped:   dto.BatchModelStopped,
}

// detail is a batch with the name of its input file, which a list row leaves out.
func (c batchConsole) detail(b batch.Batch) dto.Batch {
	name := ""
	if f, err := c.svc.FileOf(b.InputFileID); err == nil {
		name = f.Filename
	}
	return c.batchDTO(b, name)
}

func (c batchConsole) batchDTO(b batch.Batch, inputName string) dto.Batch {
	out := dto.Batch{
		ID: b.ID, Endpoint: b.Endpoint, Status: b.Status, Cancellable: batch.Cancellable(b.Status),
		InputFileID: b.InputFileID, InputFilename: inputName, OutputExpiresAt: c.svc.OutputExpiresAt(b),
		OutputFileID: b.OutputFileID, ErrorFileID: b.ErrorFileID, CompletionWindow: b.CompletionWindow,
		RequestCounts: dto.BatchRequestCount{Total: b.Total, Completed: b.Completed, Failed: b.Failed},
		Model:         b.Model, Metadata: map[string]string{}, UserID: b.UserID, APIKeyID: b.APIKeyID,
		CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt, InProgressAt: b.InProgressAt, FinalizingAt: b.FinalizingAt,
		CompletedAt: b.CompletedAt, FailedAt: b.FailedAt, ExpiredAt: b.ExpiredAt, CancellingAt: b.CancellingAt, CancelledAt: b.CancelledAt,
	}
	for k, v := range b.Metadata {
		out.Metadata[k] = v
	}
	for _, w := range b.Warnings {
		if len(w.Models) == 0 {
			continue
		}
		switch {
		case w.Code == batch.WarningModelSleepHeld && w.Param == batch.ParamInputFileID:
			out.Warnings = append(out.Warnings, messages.Warning{Code: messages.WarningBatchFileModelsSleepHeld, Params: namedModelsParams(w.Models)})
		case w.Code == batch.WarningModelSleepHeld:
			out.Warnings = append(out.Warnings, messages.Warning{Code: api.WarningBatchModelSleepHeld, Params: map[string]any{"model": w.Models[0]}})
		case w.Code == batch.WarningModelWaits && w.Param == batch.ParamInputFileID:
			params := namedModelsParams(w.Models)
			params["state"] = batchModelStates[w.State]
			out.Warnings = append(out.Warnings, messages.Warning{Code: messages.WarningBatchFileModelsWait, Params: params})
		case w.Code == batch.WarningModelWaits:
			out.Warnings = append(out.Warnings, messages.Warning{Code: api.WarningBatchModelWaits, Params: map[string]any{"model": w.Models[0], "state": batchModelStates[w.State]}})
		}
	}
	if b.ErrorCode != "" {
		out.Failure = &dto.BatchFailure{Code: b.ErrorCode, Message: b.ErrorMessage}
	}
	return out
}

// consoleBatchError turns a batch refusal into the console's product error:
// not found, not cancellable, a bad file (with its line), a file over the
// limit, or a bad request field.
func consoleBatchError(err error) error {
	var be *batch.Error
	if !errors.As(err, &be) {
		return err
	}
	out := &api.BatchError{Status: be.Status, Fallback: be.Message}
	switch {
	case be.Status == http.StatusNotFound:
		out.Code = api.ErrorBatchNotFound
	case be.Code == batch.CodeModelNotFound && len(be.Models) > 0:
		out.Code = messages.ErrorBatchFileModelsUnknown
		out.Params = namedModelsParams(be.Models)
	case be.Code == batch.CodeModelNotFound:
		out.Code = api.ErrorBatchModelUnknown
		out.Params = map[string]any{"model": be.Model}
	case be.Code == batch.CodeModelEndpoint && len(be.Models) > 0:
		out.Code = messages.ErrorBatchFileModelsEndpoint
		out.Params = namedModelsParams(be.Models)
		out.Params["endpoint"] = be.Endpoint
	case be.Code == batch.CodeModelEndpoint:
		out.Code = api.ErrorBatchModelEndpoint
		out.Params = map[string]any{"model": be.Model, "endpoint": be.Endpoint}
	case be.Code == batch.CodeBatchNotCancellable:
		out.Code = api.ErrorBatchNotCancellable
	case be.Status == http.StatusRequestEntityTooLarge:
		out.Code = api.ErrorBatchFileTooLarge
		out.Params = map[string]any{"limit_mb": batch.MaxFileBytes >> 20}
	case be.Line > 0 || be.Code == batch.CodeEmptyFile || be.Code == batch.CodeTooManyRequests || be.Code == batch.CodeInvalidFileExtension:
		out.Code = api.ErrorBatchFileInvalid
		out.Params = map[string]any{"line": be.Line, "reason": be.Code, "detail": be.Message}
	default:
		out.Code = api.ErrorBatchRequestInvalid
		out.Params = map[string]any{"field": be.Param, "reason": be.Code}
	}
	return out
}

// namedModelsParams are the params of a product message about the models of an
// input file: the first few names as a list and how many more follow, for the
// client to join in its own language.
func namedModelsParams(models []string) map[string]any {
	named, more := batch.NamedModels(models)
	return map[string]any{"models": named, "more": more}
}
