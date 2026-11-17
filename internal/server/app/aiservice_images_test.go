package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rawLLM answers every call with finalTurn and keeps each request body as
// sent, so a test reads the content parts the way the endpoint does.
type rawLLM struct {
	mu     sync.Mutex
	bodies []json.RawMessage
}

func (l *rawLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	l.bodies = append(l.bodies, raw)
	l.mu.Unlock()
	_, _ = w.Write([]byte(finalTurn))
}

// messagesOf is a request's messages with content kept raw.
func messagesOf(t *testing.T, body json.RawMessage) []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
} {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	return req.Messages
}

func configureImageLLM(t *testing.T, f *aiFixture, imageInput bool) *rawLLM {
	t.Helper()
	llm := &rawLLM{}
	srv := httptest.NewServer(llm)
	t.Cleanup(srv.Close)
	box, err := auth.NewSecretbox("ai-image-test-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateAIConfig(&store.AIConfig{Name: "v", BaseURL: srv.URL, Model: "m", IsDefault: true, ProxyMode: "direct", ImageInput: imageInput}); err != nil {
		t.Fatal(err)
	}
	f.ai.box = box
	f.ai.outboundAllowPrivateHosts = []string{"127.0.0.1"}
	return llm
}

const askImage = "data:image/png;base64,iVBORw0KGgo="

// The question's images ride on the message that holds the question - the
// seed on a first turn - as content parts; an image alone is a question.
func TestAskSendsImagesWithTheQuestion(t *testing.T) {
	f := newAIFixture(t, "app_ai_images_first")
	llm := configureImageLLM(t, f, true)
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskAsk, Images: []string{askImage}, Locale: "en"}); err != nil {
		t.Fatal(err)
	}
	msgs := messagesOf(t, llm.bodies[0])
	last := msgs[len(msgs)-1]
	var parts []aihelper.ContentPart
	if last.Role != "user" || json.Unmarshal(last.Content, &parts) != nil || len(parts) != 2 ||
		parts[0].Type != "text" || parts[0].Text == "" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != askImage {
		t.Fatalf("first turn = %s", last.Content)
	}
	var system string
	if json.Unmarshal(msgs[0].Content, &system) != nil || system == "" {
		t.Fatalf("system stays a string: %s", msgs[0].Content)
	}
}

// A follow-up sends the earlier user turn's image again, on that turn, and
// the new image on the new question; the assistant turn stays text.
func TestAskFollowUpKeepsEachTurnsImages(t *testing.T) {
	f := newAIFixture(t, "app_ai_images_follow")
	llm := configureImageLLM(t, f, true)
	earlier := "data:image/png;base64,AAAA"
	_, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskAsk, Question: "and now?", Images: []string{askImage}, Locale: "en",
		History: []aihelper.Message{{Role: "user", Content: "what is this?", Images: []string{earlier}}, {Role: "assistant", Content: "a red square"}}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := messagesOf(t, llm.bodies[0])
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	var turn []aihelper.ContentPart
	if json.Unmarshal(msgs[2].Content, &turn) != nil || len(turn) != 2 || turn[1].ImageURL.URL != earlier {
		t.Fatalf("earlier user turn = %s", msgs[2].Content)
	}
	var answer string
	if json.Unmarshal(msgs[3].Content, &answer) != nil || answer != "a red square" {
		t.Fatalf("assistant turn = %s", msgs[3].Content)
	}
	if json.Unmarshal(msgs[4].Content, &turn) != nil || len(turn) != 2 || turn[0].Text != "and now?" || turn[1].ImageURL.URL != askImage {
		t.Fatalf("question = %s", msgs[4].Content)
	}
}

// A service not marked as taking images never receives them - from the
// question or from the thread.
func TestAskRefusesImagesTheServiceDoesNotTake(t *testing.T) {
	f := newAIFixture(t, "app_ai_images_off")
	llm := configureImageLLM(t, f, false)
	for _, req := range []aihelper.AskRequest{
		{Task: aihelper.TaskAsk, Question: "q", Images: []string{askImage}},
		{Task: aihelper.TaskAsk, Question: "q", History: []aihelper.Message{{Role: "user", Content: "x", Images: []string{askImage}}}},
	} {
		if _, err := f.ai.Ask(context.Background(), req); !errors.Is(err, aihelper.ErrImageInputOff) {
			t.Fatalf("err = %v", err)
		}
		if _, err := f.ai.AskStream(context.Background(), req); !errors.Is(err, aihelper.ErrImageInputOff) {
			t.Fatalf("stream err = %v", err)
		}
	}
	if len(llm.bodies) != 0 {
		t.Fatalf("the service was called %d times", len(llm.bodies))
	}
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskAsk, Question: "q"}); err != nil || len(llm.bodies) != 1 {
		t.Fatalf("text-only ask = %v calls=%d", err, len(llm.bodies))
	}
}
