package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fakeBatches is a BatchService that records what the routes pass it and
// answers from fields the test sets.
type fakeBatches struct {
	caller   BatchCaller
	states   []string
	page     int
	pageSize int
	id       string
	kind     string
	form     map[string]string
	fileBody string

	batch      dto.Batch
	dictionary dto.BatchDictionary
	err        error
}

func (f *fakeBatches) Dictionary() (dto.BatchDictionary, error) {
	if f.err != nil {
		return dto.BatchDictionary{}, f.err
	}
	return f.dictionary, nil
}

func (f *fakeBatches) List(c BatchCaller, states []string, page, pageSize int) (dto.BatchPage, error) {
	f.caller, f.states, f.page, f.pageSize = c, states, page, pageSize
	if f.err != nil {
		return dto.BatchPage{}, f.err
	}
	return dto.BatchPage{Items: []dto.Batch{f.batch}, Page: page, PageSize: pageSize, Total: 1}, nil
}

func (f *fakeBatches) Get(c BatchCaller, id string) (dto.Batch, error) {
	f.caller, f.id = c, id
	return f.batch, f.err
}

func (f *fakeBatches) Cancel(c BatchCaller, id string) (dto.Batch, error) {
	f.caller, f.id = c, id
	return f.batch, f.err
}

func (f *fakeBatches) CreateFromUpload(c BatchCaller, form *multipart.Reader) (dto.Batch, error) {
	f.caller = c
	f.form = map[string]string{}
	for {
		part, err := form.NextPart()
		if err != nil {
			break
		}
		raw, _ := io.ReadAll(part)
		f.form[part.FormName()] = string(raw)
	}
	return f.batch, f.err
}

func (f *fakeBatches) Download(c BatchCaller, id, kind string) (BatchDownload, error) {
	f.caller, f.id, f.kind = c, id, kind
	if f.err != nil {
		return BatchDownload{}, f.err
	}
	return BatchDownload{Filename: "batch_1_output.jsonl", Size: int64(len(f.fileBody)), Body: io.NopCloser(strings.NewReader(f.fileBody))}, nil
}

type batchRig struct {
	*tokenRig
	fake *fakeBatches
	user string
}

func newBatchRig(t *testing.T) *batchRig {
	t.Helper()
	base := newTokenRig(t)
	t.Cleanup(func() { _ = base.st.Close() })
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBatches{batch: sampleBatch()}
	base.h = Router(Deps{AuthSigner: signer, Store: base.st, Batches: fake})
	user, err := managementSessionTestToken(t, base.st, signer, auth.Claims{UserID: 5, Username: "bob", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	return &batchRig{tokenRig: base, fake: fake, user: user}
}

func sampleBatch() dto.Batch {
	created := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	return dto.Batch{
		ID: "batch_1", Endpoint: "/v1/chat/completions", Status: "validating", Cancellable: true, InputFileID: "file-in", CompletionWindow: "24h",
		RequestCounts: dto.BatchRequestCount{Total: 3}, Metadata: map[string]string{}, UserID: 1,
		CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
	}
}

const sampleBatchJSON = `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"validating","cancellable":true,"input_file_id":"file-in","completion_window":"24h","request_counts":{"total":3,"completed":0,"failed":0},"metadata":{},"user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z"}`

func (r *batchRig) upload(bearer string, fields map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if k == "file" {
			w, _ := mw.CreateFormFile("file", "in.jsonl")
			_, _ = io.WriteString(w, v)
		} else {
			_ = mw.WriteField(k, v)
		}
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/batches", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func TestBatchRoutesRequireAuthentication(t *testing.T) {
	r := newBatchRig(t)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/batches"}, {"POST", "/api/v1/batches"}, {"GET", "/api/v1/batches/batch_1"},
		{"POST", "/api/v1/batches/batch_1/cancel"}, {"GET", "/api/v1/batches/batch_1/files/output"},
		{"GET", "/api/v1/batches/dictionary"},
	} {
		rec := r.do("", route.method, route.path, "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"AUTHENTICATION_REQUIRED"`) {
			t.Errorf("%s %s without a token = %d %s", route.method, route.path, rec.Code, rec.Body)
		}
	}
	if r.fake.id != "" || r.fake.states != nil {
		t.Fatalf("the service was reached without authentication: %+v", r.fake)
	}
}

func TestBatchWritesNeedAnAdministratorAndReadTokensOnlyRead(t *testing.T) {
	r := newBatchRig(t)
	for _, tc := range []struct {
		name, method, path string
	}{{"cancel", "POST", "/api/v1/batches/batch_1/cancel"}} {
		rec := r.do(r.user, tc.method, tc.path, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"ADMINISTRATOR_REQUIRED"`) {
			t.Errorf("%s as a plain user = %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if rec := r.upload(r.user, map[string]string{"file": "x", "endpoint": "/v1/embeddings"}); rec.Code != http.StatusForbidden {
		t.Errorf("create as a plain user = %d", rec.Code)
	}
	read := r.mint("read")
	if rec := r.do(read, "GET", "/api/v1/batches", ""); rec.Code != http.StatusOK {
		t.Errorf("list with a read token = %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(read, "POST", "/api/v1/batches/batch_1/cancel", ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_READ_ONLY"`) {
		t.Errorf("cancel with a read token = %d %s", rec.Code, rec.Body)
	}
	write := r.mint("write")
	if rec := r.do(write, "POST", "/api/v1/batches/batch_1/cancel", ""); rec.Code != http.StatusOK {
		t.Errorf("cancel with a write token = %d %s", rec.Code, rec.Body)
	}
}

func TestBatchListWireQueryAndScope(t *testing.T) {
	r := newBatchRig(t)
	rec := r.do(r.session, "GET", "/api/v1/batches", "")
	want := `{"items":[` + sampleBatchJSON + `],"page":1,"page_size":50,"total":1}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("list = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}
	if !r.fake.caller.All || r.fake.caller.UserID == 0 || len(r.fake.states) != 0 {
		t.Fatalf("an administrator's list = %+v", r.fake)
	}
	rec = r.do(r.session, "GET", "/api/v1/batches?page=2&page_size=10&state=completed,failed", "")
	if rec.Code != http.StatusOK || r.fake.page != 2 || r.fake.pageSize != 10 || strings.Join(r.fake.states, ",") != "completed,failed" {
		t.Fatalf("filters: page=%d size=%d states=%v (%d)", r.fake.page, r.fake.pageSize, r.fake.states, rec.Code)
	}
	adminID := r.fake.caller.UserID
	if rec = r.do(r.user, "GET", "/api/v1/batches", ""); rec.Code != http.StatusOK || r.fake.caller.All || r.fake.caller.UserID == 0 || r.fake.caller.UserID == adminID {
		t.Fatalf("a plain user's list = %d %+v", rec.Code, r.fake.caller)
	}
	for path, code := range map[string]string{
		"/api/v1/batches?page=0":         "LIST_PAGE_INVALID",
		"/api/v1/batches?page_size=500":  "LIST_PAGE_INVALID",
		"/api/v1/batches?state=sleeping": "LIST_FILTER_INVALID",
		"/api/v1/batches?q=foo":          "LIST_FILTER_INVALID",
	} {
		rec := r.do(r.session, "GET", path, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestBatchGetCancelAndProductErrors(t *testing.T) {
	r := newBatchRig(t)
	rec := r.do(r.session, "GET", "/api/v1/batches/batch_1", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != sampleBatchJSON || r.fake.id != "batch_1" {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	r.fake.batch.Status = "cancelling"
	rec = r.do(r.session, "POST", "/api/v1/batches/batch_1/cancel", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"cancelling"`) {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"not found", &BatchError{Status: 404, Code: ErrorBatchNotFound, Fallback: "No such Batch object: batch_9"}, 404, `{"code":"BATCH_NOT_FOUND","error":"No such Batch object: batch_9"}`},
		{"not cancellable", &BatchError{Status: 409, Code: ErrorBatchNotCancellable, Fallback: "The batch is completed and cannot be cancelled"}, 409, `{"code":"BATCH_NOT_CANCELLABLE","error":"The batch is completed and cannot be cancelled"}`},
		{"bad file", &BatchError{Status: 400, Code: ErrorBatchFileInvalid, Fallback: "Line 3: custom_id must be a non-empty string", Params: map[string]any{"line": 3, "reason": "missing_custom_id"}}, 400,
			`{"code":"BATCH_FILE_INVALID","error":"Line 3: custom_id must be a non-empty string","params":{"line":3,"reason":"missing_custom_id"}}`},
	} {
		r.fake.err = tc.err
		for _, call := range []struct{ method, path string }{{"GET", "/api/v1/batches/batch_9"}, {"POST", "/api/v1/batches/batch_9/cancel"}, {"GET", "/api/v1/batches/batch_9/files/output"}, {"GET", "/api/v1/batches"}} {
			rec := r.do(r.session, call.method, call.path, "")
			if rec.Code != tc.status || strings.TrimSpace(rec.Body.String()) != tc.body {
				t.Errorf("%s %s %s = %d %s, want %d %s", tc.name, call.method, call.path, rec.Code, rec.Body, tc.status, tc.body)
			}
		}
	}
	r.fake.err = io.ErrUnexpectedEOF
	if rec := r.do(r.session, "GET", "/api/v1/batches/batch_1", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("an unexpected error = %d", rec.Code)
	}
}

func TestBatchCreateTakesAMultipartUpload(t *testing.T) {
	r := newBatchRig(t)
	rec := r.upload(r.session, map[string]string{"file": `{"custom_id":"a"}` + "\n", "endpoint": "/v1/chat/completions"})
	if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != sampleBatchJSON {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if r.fake.form["endpoint"] != "/v1/chat/completions" || r.fake.form["file"] != `{"custom_id":"a"}`+"\n" || !r.fake.caller.All {
		t.Fatalf("service got %+v for %+v", r.fake.form, r.fake.caller)
	}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "model": "qwen-chat"})
	if rec.Code != http.StatusCreated || r.fake.form["model"] != "qwen-chat" {
		t.Fatalf("create with a model = %d %s, service got %+v", rec.Code, rec.Body, r.fake.form)
	}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "completion_window": "72h"})
	if rec.Code != http.StatusCreated || r.fake.form["completion_window"] != "72h" {
		t.Fatalf("create with a window = %d %s, service got %+v", rec.Code, rec.Body, r.fake.form)
	}
	rec = r.do(r.session, "POST", "/api/v1/batches", `{"endpoint":"/v1/embeddings"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"BATCH_REQUEST_INVALID"`) {
		t.Fatalf("a JSON body = %d %s", rec.Code, rec.Body)
	}
	r.fake.err = &BatchError{Status: 400, Code: ErrorBatchRequestInvalid, Fallback: "completion_window must be a whole number of hours from 1h to 72h", Params: map[string]any{"field": "completion_window", "reason": "invalid_completion_window"}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/embeddings", "completion_window": "73h"})
	if want := `{"code":"BATCH_REQUEST_INVALID","error":"completion_window must be a whole number of hours from 1h to 72h","params":{"field":"completion_window","reason":"invalid_completion_window"}}`; rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("a bad window = %d %s", rec.Code, rec.Body)
	}
	r.fake.err = &BatchError{Status: 413, Code: ErrorBatchFileTooLarge, Fallback: "the file is larger than 200 MB", Params: map[string]any{"limit_mb": 200}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/embeddings"})
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), `"params":{"limit_mb":200}`) {
		t.Fatalf("too large = %d %s", rec.Code, rec.Body)
	}
}

func TestBatchFileDownload(t *testing.T) {
	r := newBatchRig(t)
	r.fake.fileBody = `{"id":"batch_req_1"}` + "\n"
	for _, kind := range []string{"input", "output", "error"} {
		rec := r.do(r.session, "GET", "/api/v1/batches/batch_1/files/"+kind, "")
		if rec.Code != http.StatusOK || rec.Body.String() != r.fake.fileBody || r.fake.kind != kind ||
			rec.Header().Get("Content-Type") != "application/x-ndjson" ||
			rec.Header().Get("Content-Disposition") != `attachment; filename=batch_1_output.jsonl` ||
			rec.Header().Get("Content-Length") != "21" {
			t.Fatalf("%s = %d %q headers %v", kind, rec.Code, rec.Body, rec.Header())
		}
	}
	rec := r.do(r.session, "GET", "/api/v1/batches/batch_1/files/secrets", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"BATCH_REQUEST_INVALID"`) {
		t.Fatalf("unknown kind = %d %s", rec.Code, rec.Body)
	}
}

func TestBatchRoutesAnswerNotFoundWithoutAService(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/batches"}, {"POST", "/api/v1/batches"}, {"GET", "/api/v1/batches/batch_1"},
		{"POST", "/api/v1/batches/batch_1/cancel"}, {"GET", "/api/v1/batches/batch_1/files/output"},
		{"GET", "/api/v1/batches/dictionary"},
	} {
		if rec := rig.do(rig.session, route.method, route.path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d", route.method, route.path, rec.Code)
		}
	}
}

// A batch given a model carries it, and the create response its warnings, in
// the wire shape a client reads; a refused model is a product error.
func TestBatchCreateWithAModelWireAndErrors(t *testing.T) {
	r := newBatchRig(t)
	r.fake.batch.Model = "held-model"
	r.fake.batch.Warnings = []messages.Warning{{Code: WarningBatchModelSleepHeld, Params: map[string]any{"model": "held-model"}}}
	rec := r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "model": "held-model"})
	want := `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"validating","cancellable":true,"input_file_id":"file-in","completion_window":"24h","model":"held-model","request_counts":{"total":3,"completed":0,"failed":0},"metadata":{},"warnings":[{"code":"BATCH_MODEL_SLEEP_HELD","params":{"model":"held-model"}}],"user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z"}`
	if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("create = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}

	r.fake.err = &BatchError{Status: 400, Code: ErrorBatchModelUnknown, Fallback: "No active deployment serves the model ghost", Params: map[string]any{"model": "ghost"}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "model": "ghost"})
	if wantErr := `{"code":"BATCH_MODEL_UNKNOWN","error":"No active deployment serves the model ghost","params":{"model":"ghost"}}`; rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != wantErr {
		t.Fatalf("unknown model = %d %s", rec.Code, rec.Body)
	}
}

// Without a model, the models of the input file go as a list with the number
// of further ones, in the refusal and in the held warning alike; a finished
// batch is not cancellable and says when its output goes.
func TestBatchFileModelsAndFinishedBatchWire(t *testing.T) {
	r := newBatchRig(t)
	r.fake.err = &BatchError{Status: 400, Code: messages.ErrorBatchFileModelsUnknown, Fallback: "The input file names models that no active deployment serves: a, b",
		Params: map[string]any{"models": []string{"a", "b"}, "more": 0}}
	rec := r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions"})
	if want := `{"code":"BATCH_FILE_MODELS_UNKNOWN","error":"The input file names models that no active deployment serves: a, b","params":{"models":["a","b"],"more":0}}`; rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("unknown file models = %d %s", rec.Code, rec.Body)
	}

	r.fake.err = nil
	r.fake.batch.Warnings = []messages.Warning{{Code: messages.WarningBatchFileModelsSleepHeld, Params: map[string]any{"models": []string{"m1", "m2", "m3", "m4", "m5"}, "more": 2}}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions"})
	if want := `"warnings":[{"code":"BATCH_FILE_MODELS_SLEEP_HELD","params":{"models":["m1","m2","m3","m4","m5"],"more":2}}]`; rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("held file models = %d %s", rec.Code, rec.Body)
	}

	done := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	kept := done.Add(30 * 24 * time.Hour)
	r.fake.batch = sampleBatch()
	r.fake.batch.Status, r.fake.batch.Cancellable, r.fake.batch.OutputFileID, r.fake.batch.CompletedAt, r.fake.batch.OutputExpiresAt = "completed", false, "file-out", &done, &kept
	rec = r.do(r.session, "GET", "/api/v1/batches/batch_1", "")
	want := `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"completed","cancellable":false,"input_file_id":"file-in","output_file_id":"file-out","completion_window":"24h","request_counts":{"total":3,"completed":0,"failed":0},"metadata":{},"output_expires_at":"2026-11-27T10:00:00Z","user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","completed_at":"2026-10-28T10:00:00Z"}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("finished batch = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}
}

// A running batch's detail says why its requests wait, and a model of another
// endpoint or one that waits is a product error or warning, each in its exact
// wire shape.
func TestBatchWaitingAndModelStateWire(t *testing.T) {
	r := newBatchRig(t)
	since := time.Date(2026, 11, 3, 3, 0, 40, 0, time.UTC)
	r.fake.batch.Status = "in_progress"
	r.fake.batch.Waiting = []dto.BatchModelWait{{Model: "BAAI/bge-m3", State: dto.BatchModelFailed, Status: 503, Detail: "deployment for BAAI/bge-m3 failed to start; see the deployment list", Since: since, Requests: 8}}
	rec := r.do(r.session, "GET", "/api/v1/batches/batch_1", "")
	want := `"waiting":[{"model":"BAAI/bge-m3","state":"failed","status":503,"detail":"deployment for BAAI/bge-m3 failed to start; see the deployment list","since":"2026-11-03T03:00:40Z","requests":8}]`
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("waiting = %d %s", rec.Code, rec.Body)
	}
	var round dto.Batch
	if err := jsonDecode(rec.Body.Bytes(), &round); err != nil || len(round.Waiting) != 1 || !round.Waiting[0].Since.Equal(since) || round.Waiting[0].Requests != 8 {
		t.Fatalf("round trip = %+v err=%v", round.Waiting, err)
	}
	// A model gone from the platform has no state; a transport failure no status detail.
	r.fake.batch.Waiting = []dto.BatchModelWait{{Model: "ghost", Since: since, Requests: 1}}
	rec = r.do(r.session, "GET", "/api/v1/batches/batch_1", "")
	if want := `"waiting":[{"model":"ghost","status":0,"since":"2026-11-03T03:00:40Z","requests":1}]`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("waiting without state = %s", rec.Body)
	}

	r.fake.batch.Waiting = nil
	r.fake.batch.Warnings = []messages.Warning{{Code: WarningBatchModelWaits, Params: map[string]any{"model": "parked", "state": dto.BatchModelStopped}}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "model": "parked"})
	if want := `"warnings":[{"code":"BATCH_MODEL_WAITS","params":{"model":"parked","state":"stopped"}}]`; rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("waits warning = %d %s", rec.Code, rec.Body)
	}
	r.fake.batch.Warnings = []messages.Warning{{Code: messages.WarningBatchFileModelsWait, Params: map[string]any{"models": []string{"a"}, "more": 0, "state": dto.BatchModelFailed}}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions"})
	if want := `"warnings":[{"code":"BATCH_FILE_MODELS_WAIT","params":{"models":["a"],"more":0,"state":"failed"}}]`; rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("file waits warning = %d %s", rec.Code, rec.Body)
	}

	r.fake.err = &BatchError{Status: 400, Code: ErrorBatchModelEndpoint, Fallback: "The deployments serving the model bge do not serve /v1/chat/completions", Params: map[string]any{"model": "bge", "endpoint": "/v1/chat/completions"}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions", "model": "bge"})
	if want := `{"code":"BATCH_MODEL_ENDPOINT","error":"The deployments serving the model bge do not serve /v1/chat/completions","params":{"endpoint":"/v1/chat/completions","model":"bge"}}`; rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("endpoint refusal = %d %s", rec.Code, rec.Body)
	}
	r.fake.err = &BatchError{Status: 400, Code: messages.ErrorBatchFileModelsEndpoint, Fallback: "The input file names models whose deployments do not serve /v1/chat/completions: bge",
		Params: map[string]any{"models": []string{"bge"}, "more": 0, "endpoint": "/v1/chat/completions"}}
	rec = r.upload(r.session, map[string]string{"file": "x", "endpoint": "/v1/chat/completions"})
	if want := `{"code":"BATCH_FILE_MODELS_ENDPOINT","error":"The input file names models whose deployments do not serve /v1/chat/completions: bge","params":{"endpoint":"/v1/chat/completions","models":["bge"],"more":0}}`; rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("file endpoint refusal = %d %s", rec.Code, rec.Body)
	}
}

// The dictionary a create dialog is built from, in its exact wire shape: it
// is the dictionary route, not a batch named "dictionary"; any signed-in user
// reads it; and it only reads.
func TestBatchDictionaryWire(t *testing.T) {
	r := newBatchRig(t)
	r.fake.dictionary = dto.BatchDictionary{
		Endpoints:             []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		CompletionWindowHours: dto.BatchWindowHours{Min: 1, Max: 72, Default: 24},
		MaxFileBytes:          200 << 20, MaxFileLines: 50000,
		Statuses:            []string{"validating", "in_progress", "finalizing", "cancelling", "completed", "failed", "expired", "cancelled"},
		OutputRetentionDays: 30,
		Models: []dto.BatchModel{
			{Name: "qwen", State: dto.BatchModelRunning, Ready: true, Endpoints: []string{"/v1/chat/completions", "/v1/completions"}},
			{Name: "napping", State: dto.BatchModelAsleep, Ready: true, Endpoints: []string{"/v1/chat/completions"}},
			{Name: "held", State: dto.BatchModelAsleepHeld, Endpoints: []string{"/v1/chat/completions"}},
			{Name: "parked", State: dto.BatchModelStopped, Endpoints: []string{"/v1/embeddings"}},
		},
	}
	want := `{"endpoints":["/v1/chat/completions","/v1/completions","/v1/embeddings"],"completion_window_hours":{"min":1,"max":72,"default":24},` +
		`"max_file_bytes":209715200,"max_file_lines":50000,"statuses":["validating","in_progress","finalizing","cancelling","completed","failed","expired","cancelled"],` +
		`"output_retention_days":30,"models":[{"name":"qwen","state":"running","ready":true,"endpoints":["/v1/chat/completions","/v1/completions"]},` +
		`{"name":"napping","state":"asleep","ready":true,"endpoints":["/v1/chat/completions"]},{"name":"held","state":"asleep_held","ready":false,"endpoints":["/v1/chat/completions"]},` +
		`{"name":"parked","state":"stopped","ready":false,"endpoints":["/v1/embeddings"]}]}`
	for _, bearer := range []string{r.session, r.user, r.mint("read")} {
		rec := r.do(bearer, "GET", "/api/v1/batches/dictionary", "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("dictionary = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
		}
	}
	if r.fake.id != "" {
		t.Fatalf("the dictionary was read as the batch %q", r.fake.id)
	}
	var round dto.BatchDictionary
	if err := jsonDecode([]byte(want), &round); err != nil || len(round.Models) != 4 || round.Models[2].State != dto.BatchModelAsleepHeld || round.Models[3].Endpoints[0] != "/v1/embeddings" || round.CompletionWindowHours.Default != 24 {
		t.Fatalf("round trip = %+v err=%v", round, err)
	}

	// No model served is an empty list, not null.
	r.fake.dictionary.Models = []dto.BatchModel{}
	if rec := r.do(r.session, "GET", "/api/v1/batches/dictionary", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"models":[]`) {
		t.Fatalf("no models = %d %s", rec.Code, rec.Body)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if rec := r.do(r.session, method, "/api/v1/batches/dictionary", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /batches/dictionary = %d %s", method, rec.Code, rec.Body)
		}
	}
	r.fake.err = io.ErrUnexpectedEOF
	if rec := r.do(r.session, "GET", "/api/v1/batches/dictionary", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed read = %d %s", rec.Code, rec.Body)
	}
}
