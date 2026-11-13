package app

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type scriptedSender struct {
	mu    sync.Mutex
	sent  []string
	reply func(path string, body []byte) batch.Reply
}

func (s *scriptedSender) Send(_ context.Context, owner batch.Owner, path string, body []byte) batch.Reply {
	s.mu.Lock()
	s.sent = append(s.sent, string(body))
	s.mu.Unlock()
	return s.reply(path, body)
}

func batchStack(t *testing.T, send batch.Sender) (*batch.Service, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := batch.New(batchRepo{st}, send, batch.Config{Dir: t.TempDir(), RetryBase: 10 * time.Millisecond, RetryCap: 20 * time.Millisecond, CheckpointEvery: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Wait() })
	return svc, st
}

const twoLines = `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[]}}
{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[]}}
`

// The service over the real store: a batch runs to completion, the store holds
// its files, counts and metadata, and the rows stay consistent with what the
// service reports.
func TestBatchServiceOverTheStore(t *testing.T) {
	var calls int
	var mu sync.Mutex
	svc, st := batchStack(t, &scriptedSender{reply: func(path string, body []byte) batch.Reply {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return batch.Reply{Status: 503} // retried
		}
		return batch.Reply{Status: 200, RequestID: "r", Body: []byte(`{"ok":true}`)}
	}})
	owner := batch.Owner{UserID: 4, APIKeyID: 40}
	f, err := svc.Upload(owner, "in.jsonl", strings.NewReader(twoLines))
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateBatch(owner, batch.CreateRequest{InputFileID: f.ID, Endpoint: batch.EndpointChat, CompletionWindow: "24h", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	var done batch.Batch
	deadline := time.Now().Add(10 * time.Second)
	for {
		done, _ = svc.Batch(owner, b.ID)
		if done.Status == batch.StatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch stuck: %+v", done)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if done.Completed != 2 || done.Failed != 0 || done.Metadata["k"] != "v" || done.OutputFileID == "" || done.ErrorFileID != "" || done.InProgressAt == nil || done.CompletedAt == nil {
		t.Fatalf("batch = %+v", done)
	}
	row, err := st.GetBatch(b.ID)
	if err != nil || row.UserID != 4 || row.APIKeyID != 40 || row.Total != 2 || row.Status != store.BatchCompleted {
		t.Fatalf("row = %+v err=%v", row, err)
	}
	out, err := st.GetBatchFile(done.OutputFileID)
	if err != nil || out.Purpose != store.BatchFilePurposeOutput || out.UserID != 4 || out.Bytes == 0 {
		t.Fatalf("output row = %+v err=%v", out, err)
	}
	// A listing returns the metadata of every row in one go; another user's
	// listing is empty.
	rows, _, err := svc.ListBatchesPage(owner, []string{batch.StatusCompleted}, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Metadata["k"] != "v" {
		t.Fatalf("list = %+v err=%v", rows, err)
	}
	if rows, _, _ := svc.ListBatchesPage(batch.Owner{UserID: 99}, nil, 0, 10); len(rows) != 0 {
		t.Fatalf("another user lists %+v", rows)
	}
	// The count is scoped the same way, without reading the rows.
	if counts, err := svc.CountByStatus(owner, []string{batch.StatusCompleted, batch.StatusFailed}); err != nil || len(counts) != 1 || counts[batch.StatusCompleted] != 1 {
		t.Fatalf("owner counts = %v err=%v", counts, err)
	}
	if counts, err := svc.CountByStatus(batch.Owner{UserID: 99}, []string{batch.StatusCompleted}); err != nil || len(counts) != 0 {
		t.Fatalf("another user counts %v err=%v", counts, err)
	}
	// The input file is free to delete once the batch ended; before that the
	// store said ErrBatchFileInUse, which the service reports as 409.
	if err := svc.DeleteFile(owner, f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBatchRepoMapsStoreErrors(t *testing.T) {
	_, st := batchStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply { return batch.Reply{Status: 200} }})
	repo := batchRepo{st}
	if _, err := repo.GetFile("file-x"); err != batch.ErrNotFound {
		t.Errorf("missing file = %v", err)
	}
	if _, err := repo.GetBatch("batch_x"); err != batch.ErrNotFound {
		t.Errorf("missing batch = %v", err)
	}
	if err := repo.Finish("batch_x", batch.Finish{Status: batch.StatusCompleted}); err != batch.ErrNotFound {
		t.Errorf("finish missing = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.CreateFile(batch.File{ID: "file-1", Purpose: batch.PurposeBatch, Filename: "f", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBatch(batch.Batch{ID: "batch_1", InputFileID: "file-1", Endpoint: batch.EndpointChat, CompletionWindow: "24h", Status: batch.StatusInProgress, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteFile("file-1"); err != batch.ErrFileInUse {
		t.Errorf("delete in use = %v", err)
	}
	if b, err := repo.RequestCancel("batch_1", now); err != nil || b.Status != batch.StatusCancelling {
		t.Errorf("cancel = %+v %v", b, err)
	}
	if err := repo.Finish("batch_1", batch.Finish{Status: batch.StatusCancelled, At: now}); err != nil {
		t.Fatal(err)
	}
	if b, err := repo.RequestCancel("batch_1", now); err != batch.ErrTerminal || b.Status != batch.StatusCancelled {
		t.Errorf("cancel finished = %+v %v", b, err)
	}
	if _, err := repo.RequestCancel("batch_x", now); err != batch.ErrNotFound {
		t.Errorf("cancel missing = %v", err)
	}
}

func TestConsoleBatchErrorsBecomeProductErrors(t *testing.T) {
	for _, tc := range []struct {
		err    *batch.Error
		status int
		code   string
	}{
		{&batch.Error{Status: 404, Code: "batch_not_found"}, 404, api.ErrorBatchNotFound},
		{&batch.Error{Status: 404, Code: "file_not_found"}, 404, api.ErrorBatchNotFound},
		{&batch.Error{Status: 409, Code: "batch_not_cancellable"}, 409, api.ErrorBatchNotCancellable},
		{&batch.Error{Status: 413, Code: "file_too_large"}, 413, api.ErrorBatchFileTooLarge},
		{&batch.Error{Status: 400, Code: "duplicate_custom_id", Line: 4, Message: "Line 4: x"}, 400, api.ErrorBatchFileInvalid},
		{&batch.Error{Status: 400, Code: "empty_file"}, 400, api.ErrorBatchFileInvalid},
		{&batch.Error{Status: 400, Code: "endpoint_mismatch", Param: "endpoint"}, 400, api.ErrorBatchRequestInvalid},
		{&batch.Error{Status: 400, Code: "model_not_found", Param: "model", Model: "ghost"}, 400, api.ErrorBatchModelUnknown},
		{&batch.Error{Status: 400, Code: "model_not_found", Param: "input_file_id", Models: []string{"a", "b"}}, 400, messages.ErrorBatchFileModelsUnknown},
		{&batch.Error{Status: 400, Code: "too_many_requests", Line: 50001}, 400, api.ErrorBatchFileInvalid},
		{&batch.Error{Status: 400, Code: "invalid_file_extension", Param: "file"}, 400, api.ErrorBatchFileInvalid},
	} {
		got, ok := consoleBatchError(tc.err).(*api.BatchError)
		if !ok || got.Status != tc.status || got.Code != tc.code {
			t.Errorf("%s: %+v", tc.err.Code, got)
		}
	}
	if err := consoleBatchError(io.ErrUnexpectedEOF); err != io.ErrUnexpectedEOF {
		t.Errorf("an unexpected error = %v", err)
	}
}

// The console creates a batch from a form, sees it with its input file's name,
// downloads the output, and an invalid file is refused with its line.
func TestBatchConsoleCreateGetAndDownload(t *testing.T) {
	svc, _ := batchStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply {
		return batch.Reply{Status: 200, RequestID: "r", Body: []byte(`{"ok":1}`)}
	}})
	console := batchConsole{svc}
	caller := api.BatchCaller{UserID: 2, All: true}
	form := func(fields map[string]string) *multipart.Reader {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			if k == "file" {
				w, _ := mw.CreateFormFile("file", "my.jsonl")
				_, _ = io.WriteString(w, v)
			} else {
				_ = mw.WriteField(k, v)
			}
		}
		_ = mw.Close()
		return multipart.NewReader(&buf, mw.Boundary())
	}
	created, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions"}))
	if err != nil || created.InputFilename != "my.jsonl" || created.RequestCounts.Total != 2 || created.UserID != 2 || created.APIKeyID != 0 ||
		!created.Cancellable || created.OutputExpiresAt != nil {
		t.Fatalf("created = %+v err=%v", created, err)
	}
	var output string
	var completed int
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := console.Get(caller, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status == "completed" {
			// A finished batch is not cancellable, and its output is kept the
			// service's retention (30 days by default) after it ended.
			if b.Cancellable || b.CompletedAt == nil || b.OutputExpiresAt == nil || !b.OutputExpiresAt.Equal(b.CompletedAt.Add(30*24*time.Hour)) {
				t.Fatalf("completed batch = %+v", b)
			}
			output, completed = b.OutputFileID, b.RequestCounts.Completed
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck: %+v", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if output == "" || completed != 2 {
		t.Fatalf("done = %q %d", output, completed)
	}
	file, err := console.Download(caller, created.ID, "output")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(file.Body)
	file.Body.Close()
	if strings.Count(string(body), "\n") != 2 || file.Size != int64(len(body)) || !strings.HasSuffix(file.Filename, "_output.jsonl") {
		t.Fatalf("download = %q %+v", body, file)
	}
	if _, err := console.Download(caller, created.ID, "error"); err == nil || err.(*api.BatchError).Code != api.ErrorBatchNotFound {
		t.Fatalf("a batch with no error file = %v", err)
	}
	if _, err := console.Get(api.BatchCaller{UserID: 77}, created.ID); err == nil || err.(*api.BatchError).Status != 404 {
		t.Fatalf("another user's batch = %v", err)
	}
	_, err = console.CreateFromUpload(caller, form(map[string]string{"file": twoLines + `{"custom_id":"a"}` + "\n", "endpoint": "/v1/chat/completions"}))
	be, ok := err.(*api.BatchError)
	if !ok || be.Code != api.ErrorBatchFileInvalid || be.Params["line"] != 3 || be.Params["reason"] != "duplicate_custom_id" {
		t.Fatalf("invalid file = %+v", err)
	}
	if _, err = console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/embeddings"})); err == nil || err.(*api.BatchError).Code != api.ErrorBatchRequestInvalid {
		t.Fatalf("endpoint mismatch = %v", err)
	}
	mixed := twoLines + `{"custom_id":"e","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n"
	_, err = console.CreateFromUpload(caller, form(map[string]string{"file": mixed}))
	if be, ok := err.(*api.BatchError); !ok || be.Code != api.ErrorBatchFileInvalid || be.Params["line"] != 3 || be.Params["reason"] != "mixed_endpoints" {
		t.Fatalf("mixed endpoints = %+v", err)
	}
	page, err := console.List(caller, nil, 1, 50)
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("the refused uploads left batches behind: %+v %v", page, err)
	}
}

func TestDataPlaneRoutesBatchPathsAndModels(t *testing.T) {
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Handler", name) })
	}
	h := dataPlane(mark("models"), mark("batches"), mark("gateway"))
	for path, want := range map[string]string{
		"/v1/models": "models", "/v1/files": "batches", "/v1/files/file-1": "batches", "/v1/files/file-1/content": "batches",
		"/v1/batches": "batches", "/v1/batches/batch_1/cancel": "batches",
		"/v1/chat/completions": "gateway", "/v1/filesystem": "gateway", "/v1/batchesx": "gateway", "/v1/models/x": "gateway",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Handler"); got != want {
			t.Errorf("%s -> %s, want %s", path, got, want)
		}
	}
}
