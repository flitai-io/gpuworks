package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiImageRouter is aiRouter over a store of its own, handed back: the shared
// router store would carry an AI service into every other test using it.
func aiImageRouter(t *testing.T, ai AIService) (http.Handler, string, *store.Store) {
	t.Helper()
	signer, err := auth.NewTokenSigner("ai-image-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return Router(Deps{AuthSigner: signer, Store: st, AI: ai}), token, st
}

// pngURL is a real PNG as a data: URL, n bytes of payload after the header.
func pngURL(n int) string {
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, n)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// The console learns from /ai/available whether to offer pasting, and the
// limits to keep, from the service in use.
func TestAIAvailableAnswersImageInputAndLimits(t *testing.T) {
	h, token, st := aiImageRouter(t, &fakeAI{available: true})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/available", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"available":true,"image_input":false}` {
		t.Fatalf("no service = %d %s", rec.Code, rec.Body)
	}
	if _, err := st.CreateAIConfig(&store.AIConfig{Name: "v", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", ImageInput: true}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"available":true,"image_input":true,"images":{"per_message":4,"per_request":8,"max_bytes":%d,"max_side":1568}}`, 2<<20)
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/available", ""); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("vision service = %s", rec.Body)
	}
	h, token, _ = aiImageRouter(t, &fakeAI{available: false})
	if rec := aiDo(t, h, token, "GET", "/api/v1/ai/available", ""); strings.TrimSpace(rec.Body.String()) != `{"available":false,"image_input":false}` {
		t.Fatalf("unavailable = %s", rec.Body)
	}
}

func TestAIAskForwardsImages(t *testing.T) {
	ai := &fakeAI{available: true, result: aihelper.Result{Answer: "ok"}}
	h, token, _ := aiImageRouter(t, ai)
	img, earlier := pngURL(16), pngURL(24)
	body := fmt.Sprintf(`{"task":"ask","images":[%q],"history":[{"role":"user","content":"look","images":[%q]},{"role":"assistant","content":"seen"}]}`, img, earlier)
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", body); rec.Code != 200 {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	// An image without a question is a question.
	if len(ai.req.Images) != 1 || ai.req.Images[0] != img || len(ai.req.History) != 2 || len(ai.req.History[0].Images) != 1 || ai.req.History[0].Images[0] != earlier {
		t.Fatalf("forwarded %+v", ai.req)
	}
}

func TestAIAskRejectsUnusableImages(t *testing.T) {
	h, token, _ := aiImageRouter(t, &fakeAI{available: true, result: aihelper.Result{Answer: "ok"}})
	five := strings.TrimSuffix(strings.Repeat(fmt.Sprintf("%q,", pngURL(8)), 5), ",")
	four := strings.TrimSuffix(strings.Repeat(fmt.Sprintf("%q,", pngURL(8)), 4), ",")
	for _, tc := range []struct {
		name, body, reason, where string
	}{
		{"too many in the question", `{"task":"ask","question":"q","images":[` + five + `]}`, "too_many", "question"},
		{"not an image", `{"task":"ask","question":"q","images":["data:text/plain;base64,aGk="]}`, "format", "question"},
		{"too large", fmt.Sprintf(`{"task":"ask","question":"q","images":[%q]}`, pngURL(aihelper.ImageMaxBytes)), "too_large", "question"},
		{"on an assistant turn", fmt.Sprintf(`{"task":"ask","question":"q","history":[{"role":"assistant","content":"a","images":[%q]}]}`, pngURL(8)), "role", "history"},
		{"too many in the request", `{"task":"ask","question":"q","images":[` + four + `],"history":[{"role":"user","content":"a","images":[` + four + `]},{"role":"user","content":"b","images":[` + fmt.Sprintf("%q", pngURL(8)) + `]}]}`, "too_many", "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", tc.body)
			var out struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			if rec.Code != 400 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Code != "AI_IMAGE_INVALID" || out.Params["reason"] != tc.reason || out.Params["where"] != tc.where {
				t.Fatalf("= %d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "base64") {
				t.Fatalf("answer repeats the image: %s", rec.Body)
			}
		})
	}
	// A body past every image the request may carry is refused before it is read whole.
	huge := `{"task":"ask","question":"` + strings.Repeat("x", aiAskMaxBytes+1) + `"}`
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", rec.Code)
	}
}

func TestAIAskImageErrors(t *testing.T) {
	img := fmt.Sprintf(`{"task":"ask","question":"q","images":[%q]}`, pngURL(8))
	// The service is not marked as taking images: a 409 the console explains.
	h, token, _ := aiImageRouter(t, &fakeAI{available: true, err: aihelper.ErrImageInputOff})
	for _, rec := range []*httptest.ResponseRecorder{aiDo(t, h, token, "POST", "/api/v1/ai/ask", img), aiStream(t, h, token, img)} {
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"AI_IMAGE_INPUT_OFF"`) {
			t.Fatalf("image input off = %d %s", rec.Code, rec.Body)
		}
	}
	// The service refused them (vLLM's answer for a text-only model).
	refused := &aihelper.UpstreamError{Status: 400, Body: `{"object":"error","message":"Qwen3-0.6B is not a multimodal model","type":"BadRequestError"}`}
	h, token, _ = aiImageRouter(t, &fakeAI{available: true, err: refused})
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", img); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"AI_IMAGE_REJECTED"`) {
		t.Fatalf("refused = %d %s", rec.Code, rec.Body)
	}
	// The same 400 without images is the service failing, not the images.
	if rec := aiDo(t, h, token, "POST", "/api/v1/ai/ask", `{"task":"ask","question":"q"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("400 without images = %d %s", rec.Code, rec.Body)
	}
	// Mid-stream, the error event carries the code.
	h, token, _ = aiImageRouter(t, &fakeAI{available: true, events: []aihelper.Event{{Kind: aihelper.EventError, Err: fmt.Errorf("ask: %w", refused)}}})
	rec := aiStream(t, h, token, img)
	if !strings.Contains(rec.Body.String(), `"code":"AI_IMAGE_REJECTED"`) {
		t.Fatalf("stream = %s", rec.Body)
	}
	rec = aiStream(t, h, token, `{"task":"ask","question":"q"}`)
	if strings.Contains(rec.Body.String(), `"code"`) {
		t.Fatalf("stream without images got a code: %s", rec.Body)
	}
	h, token, _ = aiImageRouter(t, &fakeAI{available: true, events: []aihelper.Event{{Kind: aihelper.EventError, Err: errors.New("boom")}}})
	if rec := aiStream(t, h, token, img); !strings.HasSuffix(rec.Body.String(), "event: error\ndata: {\"error\":\"boom\"}\n\n") {
		t.Fatalf("plain error = %s", rec.Body)
	}
}
