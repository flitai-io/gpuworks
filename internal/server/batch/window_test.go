package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "48h": 48 * time.Hour, "72h": 72 * time.Hour} {
		if got, ok := ParseWindow(in); !ok || got != want {
			t.Errorf("ParseWindow(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0h", "-1h", "73h", "100h", "1d", "24H", "024h", "+24h", "1.5h", "90m", "24", "h", " 24h", "24h "} {
		if got, ok := ParseWindow(in); ok {
			t.Errorf("ParseWindow(%q) = %v, want it refused", in, got)
		}
	}
}

// The window sets the batch's expiry and ends it there: a 2h batch expires
// after two hours, not after a day, and the stored window is what was asked.
func TestExpiryFollowsTheCompletionWindow(t *testing.T) {
	r := newRig(t, Config{PerBatch: 2}, func(context.Context, Owner, string, []byte) Reply {
		return Reply{Status: 503, Body: []byte(`{"error":{"message":"asleep"}}`)}
	})
	r.start()
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(2)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "2h"})
	if err != nil {
		t.Fatal(err)
	}
	if b.CompletionWindow != "2h" || b.ExpiresAt.Sub(b.CreatedAt) != 2*time.Hour {
		t.Fatalf("batch = window %q created %v expires %v", b.CompletionWindow, b.CreatedAt, b.ExpiresAt)
	}
	eventually(t, "retries waiting", func() bool { return retryDelays(r.clock) >= 2 })
	r.clock.Advance(time.Hour + 59*time.Minute)
	time.Sleep(50 * time.Millisecond)
	if st, _ := r.repo.GetBatch(b.ID); st.Status != StatusInProgress {
		t.Fatalf("before the window ended the batch is %q", st.Status)
	}
	r.clock.Advance(2 * time.Minute)
	done := r.await(b.ID, StatusExpired)
	if done.Failed != 2 || done.CompletionWindow != "2h" {
		t.Fatalf("expired batch = %+v", done)
	}
}

// Every whole hour from 1 to 72 is accepted, with the matching expiry.
func TestCreateBatchAcceptsTheWholeRangeOfWindows(t *testing.T) {
	r := newRig(t, Config{}, echo)
	r.start()
	f, _ := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(1)))
	for _, hours := range []int{1, 2, 24, 48, 71, 72} {
		window := fmt.Sprintf("%dh", hours)
		b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: window})
		if err != nil || b.CompletionWindow != window || b.ExpiresAt.Sub(b.CreatedAt) != time.Duration(hours)*time.Hour {
			t.Fatalf("%s: %+v err=%v", window, b, err)
		}
	}
	for _, window := range []string{"0h", "73h", "1d", ""} {
		_, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: window})
		if e, ok := err.(*Error); !ok || e.Code != "invalid_completion_window" || e.Param != "completion_window" {
			t.Fatalf("%q: err = %v", window, err)
		}
	}
}

// On /v1 the window is echoed as asked and the expiry follows it.
func TestV1WireJSONWithAnExtendedWindow(t *testing.T) {
	v := newV1Rig(t)
	file := v.uploadBatchFile("k1", inputLines(1))["id"].(string)
	rec := v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"72h"}`, file)))
	obj := decode(t, rec.Body.Bytes())
	if rec.Code != 200 || obj["completion_window"] != "72h" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if got := int64(obj["expires_at"].(float64)) - int64(obj["created_at"].(float64)); got != 72*3600 {
		t.Fatalf("expires_at - created_at = %d", got)
	}
	rec = v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"1h"}`, file)))
	obj = decode(t, rec.Body.Bytes())
	if rec.Code != 200 || obj["completion_window"] != "1h" || int64(obj["expires_at"].(float64))-int64(obj["created_at"].(float64)) != 3600 {
		t.Fatalf("1h create = %d %s", rec.Code, rec.Body)
	}
}

// The console form takes the window as a field; without one it is 24h, and a
// bad one leaves no file behind.
func TestConsoleFormWindow(t *testing.T) {
	r := newRig(t, Config{}, echo)
	// Form validation is synchronous; leave unrelated batch executors stopped.
	form := func(window string) (Batch, error) {
		return r.svc.CreateFromUpload(testOwner, multipartForm(t, map[string]string{"endpoint": EndpointChat, "completion_window": window}, inputLines(1)))
	}
	b, err := form("")
	if err != nil || b.CompletionWindow != "24h" {
		t.Fatalf("default: %+v err=%v", b, err)
	}
	b, err = form("12h")
	if err != nil || b.CompletionWindow != "12h" || b.ExpiresAt.Sub(b.CreatedAt) != 12*time.Hour {
		t.Fatalf("12h: %+v err=%v", b, err)
	}
	files := len(r.repo.files)
	for _, bad := range []string{"0h", "73h", "1d"} {
		if _, err := form(bad); err == nil || err.(*Error).Code != "invalid_completion_window" {
			t.Fatalf("%s: err = %v", bad, err)
		}
	}
	if len(r.repo.files) != files {
		t.Fatalf("refused windows left files behind: %d, was %d", len(r.repo.files), files)
	}
}

// Without an endpoint the console form takes the url the file's requests ask
// for; a file asking for two is refused at the first line that differs and
// leaves no file behind.
func TestConsoleFormInfersTheEndpoint(t *testing.T) {
	r := newRig(t, Config{}, echo)
	// Form validation is synchronous; leave unrelated batch executors stopped.
	embed := `{"custom_id":"e1","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n"
	b, err := r.svc.CreateFromUpload(testOwner, multipartForm(t, nil, embed))
	if err != nil || b.Endpoint != EndpointEmbedding {
		t.Fatalf("inferred: %+v err=%v", b, err)
	}
	b, err = r.svc.CreateFromUpload(testOwner, multipartForm(t, map[string]string{"endpoint": EndpointChat}, inputLines(2)))
	if err != nil || b.Endpoint != EndpointChat {
		t.Fatalf("given: %+v err=%v", b, err)
	}
	files := len(r.repo.files)
	mixed := inputLines(2) + `{"custom_id":"e1","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n" +
		`{"custom_id":"p1","method":"POST","url":"/v1/completions","body":{"model":"m","prompt":"x"}}` + "\n"
	_, err = r.svc.CreateFromUpload(testOwner, multipartForm(t, nil, mixed))
	be, ok := err.(*Error)
	if !ok || be.Code != "mixed_endpoints" || be.Line != 3 || be.Param != "url" || be.Status != 400 {
		t.Fatalf("mixed: %+v", err)
	}
	if len(r.repo.files) != files {
		t.Fatalf("the refused file was kept: %d, was %d", len(r.repo.files), files)
	}
	// A given endpoint is still checked against the file.
	_, err = r.svc.CreateFromUpload(testOwner, multipartForm(t, map[string]string{"endpoint": EndpointEmbedding}, inputLines(1)))
	if be, ok := err.(*Error); !ok || be.Code != "endpoint_mismatch" {
		t.Fatalf("mismatch: %+v", err)
	}
}

// The console form names the class the batch's requests run at: it is stored
// as the class's name and every request goes out at it. A class that does
// not exist is refused and leaves no file behind.
func TestConsoleFormPriorityClass(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	classes := func(name string) (bool, error) { return name == "high", nil }
	r := newRig(t, Config{PriorityClasses: classes}, func(_ context.Context, owner Owner, _ string, _ []byte) Reply {
		mu.Lock()
		sent = append(sent, owner.PriorityClass)
		mu.Unlock()
		return okReply(`{}`)
	})
	r.start()
	form := func(class string) (Batch, error) {
		return r.svc.CreateFromUpload(testOwner, multipartForm(t, map[string]string{"endpoint": EndpointChat, "priority_class": class}, inputLines(2)))
	}
	b, err := form(" High ")
	if err != nil || b.PriorityClass != "high" {
		t.Fatalf("high: %+v err=%v", b, err)
	}
	r.await(b.ID, StatusCompleted)
	mu.Lock()
	if len(sent) != 2 || sent[0] != "high" || sent[1] != "high" {
		t.Fatalf("sent at %q", sent)
	}
	mu.Unlock()
	if got, _ := r.repo.GetBatch(b.ID); got.PriorityClass != "high" {
		t.Fatalf("stored %q", got.PriorityClass)
	}
	// None named: the batch class, which the gateway applies.
	if b, err := form(""); err != nil || b.PriorityClass != "" {
		t.Fatalf("none: %+v err=%v", b, err)
	}
	files := len(r.repo.files)
	if _, err := form("platinum"); err == nil || err.(*Error).Code != CodePriorityClass || err.(*Error).Param != "priority_class" {
		t.Fatalf("unknown: err = %v", err)
	}
	if len(r.repo.files) != files {
		t.Fatalf("a refused class left a file behind: %d, was %d", len(r.repo.files), files)
	}
	// A class deleted between the check and the write is refused the same way.
	r.repo.mu.Lock()
	r.repo.createErr = ErrPriorityClassGone
	r.repo.mu.Unlock()
	if _, err := form("high"); err == nil || err.(*Error).Code != CodePriorityClass {
		t.Fatalf("deleted meanwhile: err = %v", err)
	}
	r.repo.mu.Lock()
	r.repo.createErr = nil
	r.repo.mu.Unlock()
	// A lookup that fails refuses the batch rather than guessing.
	r.cfg.PriorityClasses = func(string) (bool, error) { return false, errors.New("database locked") }
	r.svc = New(r.repo, r.send, r.cfg)
	if _, err := form("high"); err == nil || err.Error() != "database locked" {
		t.Fatalf("failed lookup: err = %v", err)
	}
}
