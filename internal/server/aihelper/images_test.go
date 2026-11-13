package aihelper

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A message with images is written as OpenAI content parts, text first; one
// without stays a plain string; a response's string content reads back.
func TestMessageWireForm(t *testing.T) {
	plain, _ := json.Marshal(Message{Role: "user", Content: "hi"})
	if string(plain) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain = %s", plain)
	}
	withImages, _ := json.Marshal(Message{Role: "user", Content: "what is wrong?", Images: []string{"data:image/png;base64,AAAA", "data:image/jpeg;base64,BBBB"}})
	want := `{"role":"user","content":[{"type":"text","text":"what is wrong?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,BBBB"}}]}`
	if string(withImages) != want {
		t.Fatalf("parts\n got %s\nwant %s", withImages, want)
	}
	onlyImage, _ := json.Marshal(Message{Role: "user", Images: []string{"data:image/png;base64,AAAA"}})
	if string(onlyImage) != `{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}` {
		t.Fatalf("image only = %s", onlyImage)
	}
	var back ChatResponse
	if err := json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), &back); err != nil || back.Choices[0].Message.Content != "ok" {
		t.Fatalf("response = %+v err=%v", back, err)
	}
	// A request body carries the parts, and no "Images" key leaks.
	body, _ := json.Marshal(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x", Images: []string{"data:image/png;base64,AAAA"}}}})
	if !strings.Contains(string(body), `"image_url"`) || strings.Contains(string(body), "Images") {
		t.Fatalf("request = %s", body)
	}
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func TestValidateImages(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 32)...)
	if err := ValidateImages([]string{dataURL("image/png", png), dataURL("image/jpeg", jpeg), probeImageDataURL}); err != nil {
		t.Fatalf("valid images: %v", err)
	}
	for _, tc := range []struct {
		name   string
		images []string
		code   string
		index  int
	}{
		{"too many", []string{probeImageDataURL, probeImageDataURL, probeImageDataURL, probeImageDataURL, probeImageDataURL}, "too_many", ImagesPerMessage},
		{"not a data URL", []string{"https://example.com/a.png"}, "format", 0},
		{"unsupported type", []string{probeImageDataURL, dataURL("image/svg+xml", []byte("<svg/>"))}, "format", 1},
		{"type lies about the bytes", []string{dataURL("image/png", jpeg)}, "format", 0},
		{"not base64", []string{"data:image/png;base64,***"}, "format", 0},
		{"too large", []string{dataURL("image/png", append(png, make([]byte, ImageMaxBytes)...))}, "too_large", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImages(tc.images)
			var ie *ImageError
			if !errors.As(err, &ie) || ie.Code != tc.code || ie.Index != tc.index {
				t.Fatalf("err = %v, want %s at %d", err, tc.code, tc.index)
			}
			if strings.Contains(err.Error(), "base64") {
				t.Fatalf("error repeats the image: %v", err)
			}
		})
	}
}

func TestImageRefused(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&UpstreamError{Status: 400, Body: `{"object":"error","message":"Qwen3-0.6B is not a multimodal model","type":"BadRequestError"}`}, true},
		{&UpstreamError{Status: 400, Body: `{"error":{"message":"Invalid content type. image_url is only supported by certain models."}}`}, true},
		{fmt.Errorf("ask: %w", &UpstreamError{Status: 422, Body: `unsupported content`}), true},
		{&UpstreamError{Status: 400, Body: `{"error":{"code":"model_not_found"}}`}, false},
		{&UpstreamError{Status: 401, Body: `bad key`}, false},
		{&UpstreamError{Status: 500, Body: `boom`}, false},
		{errors.New("dial tcp: refused"), false},
	} {
		if got := ImageRefused(tc.err); got != tc.want {
			t.Errorf("ImageRefused(%v) = %v", tc.err, got)
		}
	}
}

// The thread keeps each user turn's images and never puts one on an
// assistant turn; a user turn with only an image is kept.
func TestConversationCarriesUserImages(t *testing.T) {
	msgs := Conversation("sys", "seed", []Message{
		{Role: "user", Content: "", Images: []string{"data:image/png;base64,A"}},
		{Role: "assistant", Content: "I see a red square", Images: []string{"data:image/png;base64,B"}},
		{Role: "user", Content: "and this?", Images: []string{"data:image/png;base64,C"}},
	}, "next")
	if len(msgs) != 6 {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(msgs[2].Images) != 1 || msgs[2].Images[0] != "data:image/png;base64,A" {
		t.Fatalf("image-only user turn = %+v", msgs[2])
	}
	if len(msgs[3].Images) != 0 {
		t.Fatalf("assistant turn kept images: %+v", msgs[3])
	}
	if len(msgs[4].Images) != 1 || msgs[5].Content != "next" {
		t.Fatalf("thread = %+v", msgs)
	}
}
