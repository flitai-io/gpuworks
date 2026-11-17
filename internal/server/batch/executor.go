package batch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Why a batch's dispatch stopped early.
const (
	causeNone int32 = iota
	causeCancel
	causeExpired
)

// execute runs one batch to its end, or to the service's shutdown, from
// which the next start resumes it.
func (s *Service) execute(ctx context.Context, b Batch, h *handle) {
	// A batch waits for an executor slot; a cancel while it waits skips the wait.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-h.cancel:
	case <-ctx.Done():
		return
	}
	fresh, err := s.repo.GetBatch(b.ID)
	if err != nil || Terminal(fresh.Status) {
		return
	}
	r := &run{svc: s, b: fresh, h: h}
	r.execute(ctx)
}

// run is the execution of one batch.
type run struct {
	svc *Service
	b   Batch
	h   *handle
	p   *partial

	cause atomic.Int32
}

func (r *run) log(msg string, args ...any) {
	r.svc.cfg.Log.Warn("batch: "+msg, append([]any{"batch", r.b.ID}, args...)...)
}

func (r *run) execute(ctx context.Context) {
	s := r.svc
	now := s.cfg.Clock.Now()
	if r.b.Status == StatusValidating {
		if _, err := s.repo.Start(r.b.ID, now); err != nil {
			r.log("start", "err", err)
			return
		}
		fresh, err := s.repo.GetBatch(r.b.ID)
		if err != nil || Terminal(fresh.Status) {
			return
		}
		r.b = fresh
	}
	p, err := openPartial(s.partialPath(r.b.ID), r.b.CheckpointOffset)
	if err != nil {
		r.fail(now, "results_unreadable", "the partial results cannot be read: "+err.Error())
		return
	}
	r.p = p

	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()
	dispCtx, cancelDisp := context.WithCancel(sendCtx)
	defer cancelDisp()
	finished := make(chan struct{})
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		r.watch(finished, cancelDisp, cancelSend)
	}()
	if r.b.Status == StatusCancelling {
		r.cause.CompareAndSwap(causeNone, causeCancel)
		cancelDisp()
	}
	// A batch resumed after its deadline (the server was down when it passed)
	// is stopped here, before the first request. The watch's timer would fire
	// at once too, but from another goroutine: the dispatch could send first.
	if !s.cfg.Clock.Now().Before(r.b.ExpiresAt) && r.cause.CompareAndSwap(causeNone, causeExpired) {
		cancelDisp()
		cancelSend()
	}

	stopCheckpoints := make(chan struct{})
	var checkpoints sync.WaitGroup
	checkpoints.Add(1)
	go func() {
		defer checkpoints.Done()
		r.checkpointLoop(stopCheckpoints)
	}()

	dispatchErr := r.dispatch(dispCtx, sendCtx)

	close(finished)
	watch.Wait()
	close(stopCheckpoints)
	checkpoints.Wait()
	if dispatchErr == nil && ctx.Err() == nil && r.cause.Load() == causeExpired {
		dispatchErr = r.expireRest()
	}
	r.checkpoint()
	_ = p.close()

	switch {
	case ctx.Err() != nil:
		return // the service is stopping; the checkpoint is where a restart resumes
	case dispatchErr != nil:
		r.fail(s.cfg.Clock.Now(), "input_file_unreadable", "the input file cannot be read: "+dispatchErr.Error())
	case r.cause.Load() == causeExpired:
		r.finish(StatusExpired)
	case r.cause.Load() == causeCancel:
		r.finish(StatusCancelled)
	default:
		r.finish(StatusCompleted)
	}
}

// watch turns a cancel and the batch's expiry into the stop of its requests.
func (r *run) watch(finished <-chan struct{}, stopDispatch, stopSend context.CancelFunc) {
	s := r.svc
	untilExpiry := r.b.ExpiresAt.Sub(s.cfg.Clock.Now())
	select {
	case <-finished:
	case <-r.h.cancel:
		if r.cause.CompareAndSwap(causeNone, causeCancel) {
			stopDispatch()
		}
		// Requests in flight finish, for a while.
		select {
		case <-finished:
		case <-s.cfg.Clock.After(s.cfg.CancelGrace):
			stopSend()
		}
	case <-s.cfg.Clock.After(max(untilExpiry, 0)):
		if r.cause.CompareAndSwap(causeNone, causeExpired) {
			stopDispatch()
			stopSend()
		}
	}
}

// checkpointLoop writes the batch's progress every CheckpointEvery while
// results come in.
func (r *run) checkpointLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-r.svc.cfg.Clock.After(r.svc.cfg.CheckpointEvery):
			r.checkpoint()
		}
	}
}

// checkpoint makes the results durable and records the progress: one update
// of the batch's row, and none when nothing arrived since the last.
func (r *run) checkpoint() {
	prog, changed, err := r.p.checkpoint()
	if err != nil {
		r.log("checkpoint", "err", err)
		return
	}
	if !changed {
		return
	}
	if err := r.svc.repo.SaveProgress(r.b.ID, prog); err != nil {
		r.log("save progress", "err", err)
	}
}

// requestLine is a request of the input file as the executor needs it.
type requestLine struct {
	CustomID string          `json:"custom_id"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// dispatch reads the input file in order and runs the requests that have no
// result yet, PerBatch at a time, until it ends or dispCtx is done; it returns
// when every request it started has finished.
func (r *run) dispatch(dispCtx, sendCtx context.Context) error {
	path, err := r.svc.filePath(r.b.InputFileID)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 256<<10)
	slots := make(chan struct{}, r.svc.cfg.PerBatch)
	var lines sync.WaitGroup
	defer lines.Wait()
	for dispCtx.Err() == nil {
		raw, err := rd.ReadBytes('\n')
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			var l requestLine
			if jerr := json.Unmarshal(raw, &l); jerr != nil {
				return jerr
			}
			if !r.p.has(l.CustomID) {
				select {
				case slots <- struct{}{}:
				case <-dispCtx.Done():
					return nil
				}
				lines.Add(1)
				go func() {
					defer lines.Done()
					defer func() { <-slots }()
					r.process(dispCtx, sendCtx, l)
				}()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

// process sends one request until it has a final answer. A request the
// platform could not serve is retried with a growing wait; it stops being
// retried when the batch is cancelled or expires, and is recorded then only
// if it got an answer. A request for a model served only by stopped
// deployments, which the gateway refuses as unknown, waits for a start as one
// for a failed deployment waits for a repair. While a request waits, the
// batch's wait board says why.
func (r *run) process(dispCtx, sendCtx context.Context, l requestLine) {
	s := r.svc
	body := withModel(l.Body, r.b.Model)
	model := requestModel(body)
	waiting := false
	defer func() {
		if waiting {
			r.h.waits.settled(model)
		}
	}()
	for attempt := 0; ; attempt++ {
		select {
		case s.global <- struct{}{}:
		case <-dispCtx.Done():
			return
		}
		// select picks at random when both are ready: a slot freed in the
		// same instant as a cancel or the expiry must not start an attempt.
		if dispCtx.Err() != nil {
			<-s.global
			return
		}
		attemptCtx, cancel := context.WithTimeout(sendCtx, s.cfg.AttemptTimeout)
		reply := s.send.Send(attemptCtx, r.b.Owner(), l.URL, body)
		cancel()
		<-s.global
		if reply.Err != nil && sendCtx.Err() != nil {
			return // aborted by the expiry, a cancel's grace or a shutdown
		}
		v, code, message := classify(reply)
		if v == verdictFail && reply.Status == http.StatusNotFound && s.modelStopped(model) {
			v = verdictRetry
		}
		switch v {
		case verdictDone:
			r.record(l.CustomID, encodeResult(r.b.ID, l.CustomID, reply, nil), true)
			return
		case verdictFail:
			r.record(l.CustomID, encodeResult(r.b.ID, l.CustomID, reply, &resultError{Code: code, Message: message}), false)
			return
		}
		r.h.waits.retrying(model, !waiting, reply, s.cfg.Clock.Now())
		waiting = true
		select {
		case <-dispCtx.Done():
			return
		case <-s.cfg.Clock.After(s.cfg.Jitter(backoff(attempt, s.cfg.RetryBase, s.cfg.RetryCap, reply.RetryAfter))):
		}
	}
}

// withModel is body with its "model" set to model, the batch's override; a
// batch without one, or a body that is not a JSON object, goes as it is.
func withModel(body []byte, model string) []byte {
	if model == "" {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body
	}
	name, _ := json.Marshal(model)
	fields["model"] = name
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

func (r *run) record(customID string, line []byte, succeeded bool) {
	if err := r.p.append(customID, line, succeeded); err != nil {
		r.log("write a result", "err", err)
	}
}

// finish ends the batch: it splits the results into the output and error
// files and records the end.
func (r *run) finish(status string) {
	s := r.svc
	if status != StatusCancelled {
		if err := s.repo.BeginFinalize(r.b.ID, s.cfg.Clock.Now()); err != nil {
			r.log("begin finalize", "err", err)
		}
	}
	outID, errID := outputFileID(r.b.ID, "output"), outputFileID(r.b.ID, "error")
	outPath, _ := s.filePath(outID)
	errPath, _ := s.filePath(errID)
	res, err := assemble(s.partialPath(r.b.ID), outPath, errPath)
	if err != nil {
		r.fail(s.cfg.Clock.Now(), "results_unreadable", "the results cannot be assembled: "+err.Error())
		return
	}
	now := s.cfg.Clock.Now()
	fin := Finish{Status: status, At: now, Completed: res.completed, Failed: res.failed}
	if res.completed > 0 {
		fin.Output = &File{ID: outID, UserID: r.b.UserID, APIKeyID: r.b.APIKeyID, Purpose: PurposeOutput, Filename: r.b.ID + "_output.jsonl", Bytes: res.outBytes, CreatedAt: now}
	}
	if res.failed > 0 {
		fin.Error = &File{ID: errID, UserID: r.b.UserID, APIKeyID: r.b.APIKeyID, Purpose: PurposeOutput, Filename: r.b.ID + "_error.jsonl", Bytes: res.errBytes, CreatedAt: now}
	}
	if err := s.repo.Finish(r.b.ID, fin); err != nil && !errors.Is(err, ErrTerminal) {
		r.log("finish", "err", err)
		return
	}
	_ = os.Remove(s.partialPath(r.b.ID))
}

// expireRest gives every request without a result the error batch_expired.
func (r *run) expireRest() error {
	path, err := r.svc.filePath(r.b.InputFileID)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 256<<10)
	for {
		raw, err := rd.ReadBytes('\n')
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			var l requestLine
			if jerr := json.Unmarshal(raw, &l); jerr != nil {
				return jerr
			}
			if !r.p.has(l.CustomID) {
				r.record(l.CustomID, encodeFailure(r.b.ID, l.CustomID, "batch_expired", "the batch expired before the request was completed"), false)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// fail ends the batch as failed, for a reason of the platform's own.
func (r *run) fail(at time.Time, code, message string) {
	r.log("failed", "code", code, "message", message)
	completed, failed := 0, 0
	if r.p != nil {
		completed, failed = r.p.counts()
	}
	err := r.svc.repo.Finish(r.b.ID, Finish{Status: StatusFailed, At: at, Completed: completed, Failed: failed, ErrorCode: code, ErrorMessage: message})
	if err != nil && !errors.Is(err, ErrTerminal) {
		r.log("finish", "err", err)
	}
}
