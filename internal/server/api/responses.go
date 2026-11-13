package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	if resp, ok := v.(*dto.EvaluateResponse); ok {
		prepareEvaluateResponse(w, resp)
	}
	// Encoding can fail for persisted RawMessage values, before any bytes
	// reach the writer. Do not commit a successful status until it succeeds.
	body, err := json.Marshal(v)
	if err != nil {
		code = http.StatusInternalServerError
		body = []byte(`{"code":"RESPONSE_ENCODING_FAILED","error":"response data could not be encoded as JSON"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

// WriteError answers err as the API does: a product error with its stable
// code and params, any other error with its text. It is for a handler in
// front of the router (the experience environment's guard), so a refusal
// there reads like one from the API.
func WriteError(w http.ResponseWriter, status int, err error) { writeErr(w, status, err) }

func writeErr(w http.ResponseWriter, code int, err error) {
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) && store.IsStorageFull(err) {
		// Which write it was matters less than where the room ran out: the
		// server's own disk, which no retry or input fixes.
		err = messages.WrapProductError(ErrorServerStorageFull, "the server's disk is full; free space on the server host and retry: "+err.Error(), serverStorageParams(w), err)
		code = http.StatusInsufficientStorage
	}
	response := map[string]any{"error": err.Error()}
	report := boundedDiagnosticReport(diagnosticsFromError(err))
	if report != nil {
		response["diagnostics"] = report
	}
	if errors.As(err, &productErr) {
		response["code"] = productErr.Code()
		w.Header().Set(usageErrorHeader, productErr.Code())
		if params := productErr.Params(); len(params) > 0 {
			response["params"] = params
		}
		params := productErr.Params()
		if summary := diagnosticLogSummary(report); summary != nil {
			params = make(map[string]any, len(productErr.Params())+1)
			for key, value := range productErr.Params() {
				params[key] = value
			}
			params["diagnostics"] = summary
		}
		noteProductError(w, productErr.Code(), params)
	}
	writeJSON(w, code, response)
}

// serverStorageParams are SERVER_STORAGE_FULL's params: the data directory
// and its filesystem's size and room, read with statfs, which writes nothing
// and so still answers on a full disk. Nil when no writer in w's chain knows
// the directory.
func serverStorageParams(w http.ResponseWriter) map[string]any {
	dir := ""
	for w != nil && dir == "" {
		if d, ok := w.(serverDataDirWriter); ok {
			dir = d.ServerDataDir()
			break
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = unwrapper.Unwrap()
	}
	if dir == "" {
		return nil
	}
	params := map[string]any{"path": dir}
	if total, avail, err := diagnostics.FilesystemUsage(dir); err == nil {
		params["total_bytes"], params["available_bytes"] = total, avail
	}
	return params
}

// serverDataDirWriter is a response writer that knows the answering server's
// data directory (the server's access log).
type serverDataDirWriter interface {
	ServerDataDir() string
}

// productErrorNoter is a response writer that records the product error a
// response carries: the server's access log, which writes the code and its
// parameters on the request's line, so a refusal's reason outlives the
// response the client may have dismissed.
type productErrorNoter interface {
	NoteProductError(code string, params map[string]any)
}

// noteProductError hands code and params to the first writer in w's wrapping
// chain that records them.
func noteProductError(w http.ResponseWriter, code string, params map[string]any) {
	for w != nil {
		if noter, ok := w.(productErrorNoter); ok {
			noter.NoteProductError(code, params)
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}
