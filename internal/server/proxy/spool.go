package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

const DefaultMaxBodyBytes = 256 << 20
const maxMultipartModelBytes = 1 << 20

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }
func statusForExtractError(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return http.StatusBadRequest
}
func tooLarge(maxBody int64) error {
	return &httpError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("request body exceeds %d bytes", maxBody)}
}

func extractModel(r *http.Request, maxBody int64) (string, func(), error) {
	if modelName := strings.TrimSpace(r.URL.Query().Get("model")); modelName != "" {
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
			q := r.URL.Query()
			q.Del("model")
			r.URL.RawQuery = q.Encode()
		}
		return modelName, func() {}, nil
	}
	if modelName := strings.TrimSpace(r.Header.Get("X-GPUWorks-Model")); modelName != "" {
		r.Header.Del("X-GPUWorks-Model")
		return modelName, func() {}, nil
	}
	if maxBody > 0 && r.ContentLength > maxBody {
		return "", func() {}, tooLarge(maxBody)
	}
	b, err := newReplayBodyFromReader(r.Body, maxBody)
	if err != nil {
		return "", func() {}, err
	}
	_ = r.Body.Close()
	cleanup := func() { _ = b.Close() }
	installReplayBody(r, b)
	contentType := r.Header.Get("Content-Type")
	mediaType, params, mediaErr := mime.ParseMediaType(contentType)
	if mediaErr != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/") {
		return "", cleanup, fmt.Errorf("invalid multipart content type: %w", mediaErr)
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		if params["boundary"] == "" {
			return "", cleanup, errors.New("multipart boundary is required")
		}
		mr := multipart.NewReader(b.NewReader(), params["boundary"])
		for {
			part, nextErr := mr.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return "", cleanup, fmt.Errorf("read multipart body: %w", nextErr)
			}
			if part.FormName() != "model" {
				continue
			}
			value, readErr := io.ReadAll(io.LimitReader(part, maxMultipartModelBytes+1))
			if readErr != nil {
				return "", cleanup, readErr
			}
			if len(value) > maxMultipartModelBytes {
				return "", cleanup, fmt.Errorf("multipart model field exceeds %d bytes", maxMultipartModelBytes)
			}
			return strings.TrimSpace(string(value)), cleanup, nil
		}
		return "", cleanup, nil
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(b.NewReader()).Decode(&probe); err != nil {
		return "", cleanup, nil //nolint:nilerr // Model extraction is best effort; the caller rejects requests without a model.
	}
	return probe.Model, cleanup, nil
}

func installReplayBody(r *http.Request, b *replayBody) {
	if b == nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	if b.file != nil {
		_, _ = b.file.Seek(0, io.SeekStart)
		r.Body = b.file
	} else {
		r.Body = b.NewReader()
	}
	r.GetBody = func() (io.ReadCloser, error) { return b.NewReader(), nil }
	r.ContentLength = b.size
	r.TransferEncoding = nil
}
