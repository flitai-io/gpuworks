package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Authenticate names the caller of a /v1 request from its API key; false
// when the key is missing or invalid (the response has not been written).
type Authenticate func(r *http.Request) (Owner, bool)

// V1Routes is the route inventory of the OpenAI-compatible handler, for the
// test that pins it.
var V1Routes = []string{
	"DELETE /v1/files/{id}",
	"GET /v1/batches",
	"GET /v1/batches/{id}",
	"GET /v1/files",
	"GET /v1/files/{id}",
	"GET /v1/files/{id}/content",
	"POST /v1/batches",
	"POST /v1/batches/{id}/cancel",
	"POST /v1/files",
}

// V1Handler serves /v1/files and /v1/batches. Every route authenticates with
// auth first and answers in OpenAI's error shape.
func (s *Service) V1Handler(auth Authenticate) http.Handler {
	h := &v1{s: s, auth: auth}
	r := chi.NewRouter()
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeV1Error(w, notFound("not_found", "Unknown request URL"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeV1Error(w, &Error{Status: 405, Type: "invalid_request_error", Code: "method_not_allowed", Message: "Method not allowed"})
	})
	r.Use(h.authenticate)
	r.Post("/v1/files", h.uploadFile)
	r.Get("/v1/files", h.listFiles)
	r.Get("/v1/files/{id}", h.getFile)
	r.Get("/v1/files/{id}/content", h.fileContent)
	r.Delete("/v1/files/{id}", h.deleteFile)
	r.Post("/v1/batches", h.createBatch)
	r.Get("/v1/batches", h.listBatches)
	r.Get("/v1/batches/{id}", h.getBatch)
	r.Post("/v1/batches/{id}/cancel", h.cancelBatch)
	return r
}

type v1 struct {
	s    *Service
	auth Authenticate
}

type ownerKey struct{}

func (h *v1) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, ok := h.auth(r)
		if !ok {
			writeV1Error(w, &Error{Status: 401, Type: "invalid_request_error", Code: "invalid_api_key", Message: "invalid or missing API key"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner)))
	})
}

func (h *v1) owner(r *http.Request) Owner {
	owner, _ := r.Context().Value(ownerKey{}).(Owner)
	return owner
}

// v1Error is OpenAI's error object, plus the line of an input file a
// validation error is about.
type v1Error struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
	Line    int     `json:"line,omitempty"`
}

func writeV1Error(w http.ResponseWriter, e *Error) {
	writeJSON(w, e.Status, map[string]v1Error{"error": {
		Message: e.Message, Type: e.Type, Param: nonEmpty(e.Param), Code: nonEmpty(e.Code), Line: e.Line,
	}})
}

// fail answers err: a refusal as itself, anything else as a server error,
// logged with the request it failed, since the client sees no detail.
func (h *v1) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if errors.As(err, &e) {
		writeV1Error(w, e)
		return
	}
	h.s.cfg.Log.Error("batch: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeV1Error(w, &Error{Status: 500, Type: "server_error", Code: "internal_error", Message: "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// uploadFile serves POST /v1/files: a multipart body with the file and its
// purpose, streamed to disk while it is validated.
func (h *v1) uploadFile(w http.ResponseWriter, r *http.Request) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		writeV1Error(w, invalid("invalid_content_type", "", "the request must be multipart/form-data with the fields file and purpose"))
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeV1Error(w, invalid("invalid_multipart", "", "the multipart body cannot be read: "+err.Error()))
		return
	}
	file, err := h.s.UploadMultipart(h.owner(r), mr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWireFile(file))
}

// UploadMultipart reads a multipart upload with a file and a purpose of
// "batch", in either order, and stores the file.
func (s *Service) UploadMultipart(owner Owner, mr *multipart.Reader) (File, error) {
	var (
		purpose string
		saved   *File
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.discard(owner, saved)
			return File{}, invalid("invalid_multipart", "", "the multipart body cannot be read: "+err.Error())
		}
		switch part.FormName() {
		case "purpose":
			raw, _ := io.ReadAll(io.LimitReader(part, 64))
			purpose = strings.TrimSpace(string(raw))
			if purpose != PurposeBatch {
				s.discard(owner, saved)
				return File{}, invalid("invalid_purpose", "purpose", `purpose must be "batch"`)
			}
		case "file":
			if saved != nil {
				s.discard(owner, saved)
				return File{}, invalid("invalid_multipart", "file", "the request has more than one file")
			}
			f, err := s.Upload(owner, part.FileName(), part)
			if err != nil {
				return File{}, err
			}
			saved = &f
		}
	}
	if saved == nil {
		return File{}, invalid("missing_required_parameter", "file", "file is required")
	}
	if purpose != PurposeBatch {
		s.discard(owner, saved)
		return File{}, invalid("missing_required_parameter", "purpose", `purpose is required and must be "batch"`)
	}
	return *saved, nil
}

func (s *Service) discard(owner Owner, f *File) {
	if f != nil {
		_ = s.DeleteFile(owner, f.ID)
	}
}

func queryLimit(r *http.Request, def, max int) (int, *Error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, invalid("invalid_limit", "limit", "limit must be between 1 and "+strconv.Itoa(max))
	}
	return n, nil
}

func queryOrder(r *http.Request) *Error {
	if o := r.URL.Query().Get("order"); o != "" && o != "desc" {
		return invalid("invalid_order", "order", `only order "desc" (newest first) is supported`)
	}
	return nil
}

func (h *v1) listFiles(w http.ResponseWriter, r *http.Request) {
	limit, lerr := queryLimit(r, 10000, 10000)
	if lerr == nil {
		lerr = queryOrder(r)
	}
	purpose := r.URL.Query().Get("purpose")
	if lerr == nil && purpose != "" && purpose != PurposeBatch && purpose != PurposeOutput {
		lerr = invalid("invalid_purpose", "purpose", "purpose must be batch or batch_output")
	}
	if lerr != nil {
		writeV1Error(w, lerr)
		return
	}
	files, more, err := h.s.ListFiles(h.owner(r), purpose, r.URL.Query().Get("after"), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := wireFileList{Object: "list", Data: make([]wireFile, 0, len(files)), HasMore: more}
	for _, f := range files {
		out.Data = append(out.Data, toWireFile(f))
	}
	if len(files) > 0 {
		out.FirstID, out.LastID = &files[0].ID, &files[len(files)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *v1) getFile(w http.ResponseWriter, r *http.Request) {
	f, err := h.s.File(h.owner(r), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWireFile(f))
}

func (h *v1) fileContent(w http.ResponseWriter, r *http.Request) {
	f, fh, err := h.s.OpenFile(h.owner(r), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer fh.Close()
	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Bytes, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, fh)
}

func (h *v1) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.s.DeleteFile(h.owner(r), id); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wireFileDeleted{ID: id, Object: "file", Deleted: true})
}

type createBatchBody struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata"`
	// Model is a gpuworks extension: the served name every request is sent to.
	Model string `json:"model"`
}

func (h *v1) createBatch(w http.ResponseWriter, r *http.Request) {
	var body createBatchBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeV1Error(w, invalid("invalid_json", "", "the body is not valid JSON of the batch request: "+err.Error()))
		return
	}
	// A key's batch runs at the batch class: the key's binding says what its
	// own requests may ask for, and the OpenAI request names no class.
	b, err := h.s.CreateBatch(h.owner(r), CreateRequest{
		InputFileID: body.InputFileID, Endpoint: body.Endpoint, CompletionWindow: body.CompletionWindow, Metadata: body.Metadata, Model: body.Model,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWireBatch(b))
}

func (h *v1) getBatch(w http.ResponseWriter, r *http.Request) {
	b, err := h.s.Batch(h.owner(r), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWireBatch(b))
}

func (h *v1) listBatches(w http.ResponseWriter, r *http.Request) {
	limit, lerr := queryLimit(r, 20, 100)
	if lerr == nil {
		lerr = queryOrder(r)
	}
	if lerr != nil {
		writeV1Error(w, lerr)
		return
	}
	batches, more, err := h.s.ListBatches(h.owner(r), r.URL.Query().Get("after"), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := wireBatchList{Object: "list", Data: make([]wireBatch, 0, len(batches)), HasMore: more}
	for _, b := range batches {
		out.Data = append(out.Data, toWireBatch(b))
	}
	if len(batches) > 0 {
		out.FirstID, out.LastID = &batches[0].ID, &batches[len(batches)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *v1) cancelBatch(w http.ResponseWriter, r *http.Request) {
	b, err := h.s.Cancel(h.owner(r), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWireBatch(b))
}
