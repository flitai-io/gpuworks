package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestChatRejectsUnsupportedInputBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	p, key, closeUpstream := newProxyE2E(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, `{"choices":[]}`) }))
	defer closeUpstream()
	var model store.Model
	if err := p.store.DB().First(&model).Error; err != nil {
		t.Fatal(err)
	}
	if err := p.store.DB().Model(&model).Updates(map[string]any{"input_modalities": `["text"]`, "operations": `[{"type":"openai.chat"}]`}).Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/chat/completions", "/api/v1/playground/v1/chat/completions"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"omni","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRPRIVATE","format":"wav"}}]},{"role":"assistant","content":"previous reply"},{"role":"user","content":"next"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		if strings.HasPrefix(path, "/api/") {
			p.PlaygroundHandler(nil)(rec, req)
		} else {
			p.Handler()(rec, req)
		}
		if rec.Code != 400 {
			t.Errorf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "MODEL_INPUT_MODALITY_UNSUPPORTED" || !strings.Contains(body.Error.Message, "audio") || strings.Contains(rec.Body.String(), "PRIVATE") {
			t.Fatalf("unsafe/missing modality error: %s", rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported history reached upstream %d times", calls.Load())
	}
}

func TestUpstreamValidationErrorIsBounded(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "25 validation errors input_value={'input_audio':{'data':'" + strings.Repeat("A", 6_492_508) + "'}}", "type": "BadRequestError", "code": 400}})
	p, key, closeUpstream := newProxyE2E(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write(payload)
	}))
	defer closeUpstream()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != 400 || rec.Body.Len() > 4096 || strings.Contains(rec.Body.String(), strings.Repeat("A", 128)) {
		t.Fatalf("status=%d error bytes=%d", rec.Code, rec.Body.Len())
	}
}

func TestChatInputDeclaredSupportedAndUnknownRemainCompatible(t *testing.T) {
	for _, modalities := range []string{`["text","audio","image"]`, `[]`, ``} {
		t.Run("modalities="+modalities, func(t *testing.T) {
			var got string
			p, key, closeUpstream := newProxyE2E(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				got = string(raw)
				_, _ = io.WriteString(w, `{"choices":[]}`)
			}))
			defer closeUpstream()
			if err := p.store.DB().Model(&store.Model{}).Where("source_id = ?", "org/omni").Update("input_modalities", modalities).Error; err != nil {
				t.Fatal(err)
			}
			body := `{"model":"omni","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRSUPPORTED","format":"wav"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,IMAGE"}}]}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			p.Handler()(rec, req)
			if rec.Code != 200 || got != body {
				t.Fatalf("status=%d forwarded=%s", rec.Code, got)
			}
			got = ""
			rec = httptest.NewRecorder()
			p.Handler()(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != 401 || got != "" {
				t.Fatalf("unauthenticated status=%d upstream=%s", rec.Code, got)
			}
		})
	}
}

func TestChatModalityScannerOnlyReadsContentPartTypes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"messages":[{"content":"quoted input_audio image_url"}],"tools":[{"type":"input_audio"}]}`, ""},
		{`{"messages":[{"content":[{"text":"image_url","type":"text"}]}]}`, ""},
		{`{"messages":[{"content":[{"input_audio":{"data":"AAAA"},"type":"input_audio"}]}]}`, "audio"},
		{`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "image"},
		{`{"messages":[{"content":[{"type":"video_url","video_url":{"url":"x"}}]}]}`, "video"},
	} {
		got := ""
		err := scanChatInput(bufio.NewReader(strings.NewReader(tc.body)), nil, func(modality string) error { got = modality; return nil })
		if err != nil || got != tc.want {
			t.Fatalf("got=%s err=%v body=%s", got, err, tc.body)
		}
	}
}
