package aihelper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// visionEndpoint answers an image the way each kind of server does. The
// refusals are the bodies those servers send for an image to a text-only
// model: vLLM's BadRequestError and OpenAI's invalid content type.
func visionEndpoint(t *testing.T, answer func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"malformed request"}}`)
			return
		}
		var parts []ContentPart
		if json.Unmarshal(body.Messages[0].Content, &parts) != nil || len(parts) != 2 ||
			body.Messages[0].Role != "user" || parts[0].Type != "text" || parts[1].Type != "image_url" ||
			parts[1].ImageURL == nil || !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
			t.Errorf("image probe body = %s", body.Messages[0].Content)
		}
		answer(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeImageClassifiesEachAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   ImageCheck
	}{
		{"vision model", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Red"},"finish_reason":"stop"}]}`)
		}, ImageAccepted},
		{"vision model answering in Chinese", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"红色。"},"finish_reason":"stop"}]}`)
		}, ImageAccepted},
		// deepseek-v4-pro on 2026-11-13: a 200 whose prompt has no image in it.
		{"text-only model that drops the image", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Unknown"},"finish_reason":"stop"}],"usage":{"prompt_tokens":99,"completion_tokens":1}}`)
		}, ImageIgnored},
		{"an answer that only contains the letters", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The image is covered."},"finish_reason":"stop"}]}`)
		}, ImageIgnored},
		{"vllm text-only model", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"object":"error","message":"Qwen3-0.6B is not a multimodal model","type":"BadRequestError","param":null,"code":400}`)
		}, ImageRejected},
		{"openai text-only model", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Invalid content type. image_url is only supported by certain models.","type":"invalid_request_error","param":"messages.[0].content.[1].type","code":null}}`)
		}, ImageRejected},
		{"model gone since the text probe", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Model Not Exist","code":"model_not_found"}}`)
		}, ImageUnknown},
		{"rate limited", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limit"}}`)
		}, ImageUnknown},
		{"2xx without a completion", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"ok":true}`)
		}, ImageUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := visionEndpoint(t, tc.answer)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got := NewClient(Endpoint{BaseURL: srv.URL + "/v1", APIKey: "sk-secret", Model: "m"}, "127.0.0.1").ProbeImage(ctx)
			if got.Check != tc.want {
				t.Fatalf("check = %+v, want %s", got, tc.want)
			}
			if tc.want == ImageAccepted && got.Detail != "" {
				t.Fatalf("accepted carries detail %q", got.Detail)
			}
			if tc.want == ImageIgnored && got.Detail == "" {
				t.Fatalf("ignored lost the model's answer: %+v", got)
			}
			if tc.want == ImageRejected && (got.Status != http.StatusBadRequest || got.Detail == "") {
				t.Fatalf("rejected lost the endpoint's words: %+v", got)
			}
		})
	}
}

// An endpoint that does not answer leaves the check unknown, and the key never
// shows up in what an endpoint echoes back.
func TestProbeImageUnreachableAndRedacted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := NewClient(Endpoint{BaseURL: "http://127.0.0.1:1/v1", Model: "m"}, "127.0.0.1").ProbeImage(ctx); got.Check != ImageUnknown || got.Status != 0 {
		t.Fatalf("unreachable = %+v", got)
	}
	srv := visionEndpoint(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"image_url not supported for key sk-secret"}}`)
	})
	got := NewClient(Endpoint{BaseURL: srv.URL + "/v1", APIKey: "sk-secret", Model: "m"}, "127.0.0.1").ProbeImage(ctx)
	if got.Check != ImageRejected || strings.Contains(got.Detail, "sk-secret") {
		t.Fatalf("redaction = %+v", got)
	}
}

func TestProxyRoute(t *testing.T) {
	calls := 0
	management := func() (proto.NetworkConfig, error) {
		calls++
		return proto.NetworkConfig{HTTPSProxy: "http://mgmt:3128"}, nil
	}
	if r, _ := ProxyRoute(ProxyManagement, "", management)(); r.Direct || r.Proxy.HTTPSProxy != "http://mgmt:3128" {
		t.Fatalf("management = %+v", r)
	}
	if r, _ := ProxyRoute("", "", management)(); r.Proxy.HTTPSProxy != "http://mgmt:3128" || calls != 2 {
		t.Fatalf("empty mode = %+v calls=%d, want the management proxy read per call", r, calls)
	}
	if r, _ := ProxyRoute(ProxyDirect, "http://ignored:1", management)(); !r.Direct || calls != 2 {
		t.Fatalf("direct = %+v", r)
	}
	if r, _ := ProxyRoute(ProxyCustom, "socks5://u:p@own:1080", management)(); r.Direct || r.Proxy.HTTPProxy != "socks5://u:p@own:1080" || r.Proxy.HTTPSProxy != "socks5://u:p@own:1080" {
		t.Fatalf("custom = %+v", r)
	}
	if !KnownProxyMode("custom") || KnownProxyMode("platform") || KnownProxyMode("") {
		t.Fatal("KnownProxyMode")
	}
}
