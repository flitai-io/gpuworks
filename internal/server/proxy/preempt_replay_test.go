package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

const (
	abortedReply  = `{"choices":[{"index":0,"finish_reason":"abort","message":{"role":"assistant","content":"half an ans"}}]}`
	finishedReply = `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the whole answer"}}]}`
)

// newPreemptibleProxy is a gateway in front of one awake sleepable deployment
// of level, whose engine answers with the replies in turn (the last one for
// every call after).
func newPreemptibleProxy(t *testing.T, level string, replies ...string) (*Proxy, *store.Store, string, uint, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var p *Proxy
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		reply := replies[min(n, len(replies))-1]
		if reply == abortedReply && p != nil {
			// The arbiter put the deployment to sleep for another while the
			// request ran.
			_ = updateInstanceRow(p.store, "omni-instance", map[string]any{"sleep_level": 1})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	})
	p, _, key, depID, closeUpstream := newAccountingProxyFor(t, upstream, presetDeployment(level))
	t.Cleanup(closeUpstream)
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)
	return p, p.store, key, depID, front, &calls
}

// presetDeployment is a sleepable deployment of the yield policy, with its
// defaults.
func presetDeployment(yield string) *store.Deployment {
	dep := &store.Deployment{Engine: "vllm", Sleepable: true}
	p, _ := yieldpolicy.Parse(yield, yieldpolicy.Overrides{})
	dep.SetPolicy(p)
	return dep
}

func chat(t *testing.T, front *httptest.Server, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// The engine wakes the deployment again a little after the client's request
// is held.
func wakeAfter(p *Proxy, st *store.Store, delay time.Duration) {
	p.SetWakeDemand(func(uint) {
		go func() {
			time.Sleep(delay)
			_ = updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 0})
		}()
	})
}

func TestACutOffReplyIsReplayedOnceTheDeploymentServesAgain(t *testing.T) {
	for _, level := range []string{yieldpolicy.Anytime, yieldpolicy.TimeSlice} {
		t.Run(level, func(t *testing.T) {
			p, st, key, _, front, calls := newPreemptibleProxy(t, level, abortedReply, finishedReply)
			wakeAfter(p, st, 150*time.Millisecond)
			status, body := chat(t, front, key, `{"model":"omni","messages":[{"role":"user","content":"hi"}]}`)
			if status != http.StatusOK || body != finishedReply {
				t.Fatalf("reply = %d %s, want the replayed whole answer", status, body)
			}
			if calls.Load() != 2 {
				t.Fatalf("engine calls = %d, want 2 (cut off, then replayed)", calls.Load())
			}
		})
	}
}

func TestReplayIsOnlyForPreemptibleNonStreamingRequests(t *testing.T) {
	for _, tc := range []struct {
		name, level, body string
	}{
		{"a stream", yieldpolicy.Anytime, `{"model":"omni","stream":true,"messages":[]}`},
		{"an online deployment", yieldpolicy.Idle, `{"model":"omni","messages":[]}`},
		{"a resident deployment", yieldpolicy.Never, `{"model":"omni","messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, st, key, _, front, calls := newPreemptibleProxy(t, tc.level, abortedReply, finishedReply)
			wakeAfter(p, st, 50*time.Millisecond)
			status, body := chat(t, front, key, tc.body)
			if status != http.StatusOK || body != abortedReply || calls.Load() != 1 {
				t.Fatalf("reply = %d %s after %d calls, want the cut-off reply as it came", status, body, calls.Load())
			}
		})
	}
}

func TestReplayStopsAfterItsLimit(t *testing.T) {
	// The engine cuts the request off every time without the deployment
	// sleeping: after replayMax replays the client gets what came.
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, abortedReply)
	})
	p, _, key, _, closeUpstream := newAccountingProxyFor(t, upstream, presetDeployment(yieldpolicy.Anytime))
	defer closeUpstream()
	front := httptest.NewServer(p.Handler())
	defer front.Close()
	status, body := chat(t, front, key, `{"model":"omni","messages":[]}`)
	if status != http.StatusOK || body != abortedReply || int(calls.Load()) != replayMax+1 {
		t.Fatalf("reply = %d %s after %d calls, want the last cut-off reply after %d", status, body, calls.Load(), replayMax+1)
	}
}

func TestReplyAborted(t *testing.T) {
	for body, want := range map[string]bool{
		abortedReply:  true,
		finishedReply: false,
		`{"choices":[{"finish_reason":"stop"},{"finish_reason":"abort"}]}`: true,
		`{"data":[{"embedding":[0.1]}]}`:                                   false,
		`not json`:                                                         false,
	} {
		if got := replyAborted([]byte(body)); got != want {
			t.Errorf("replyAborted(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestAHeldReplyPassesALargeReplyThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	h := newHeldReply(rec)
	h.Header().Set("Content-Type", "application/json")
	h.WriteHeader(http.StatusOK)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < replayBufferMax/len(chunk)+2; i++ {
		if _, err := h.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !h.passedThrough || rec.Body.Len() != (replayBufferMax/len(chunk)+2)*len(chunk) || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("passed through=%v wrote %d bytes, content type %q", h.passedThrough, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
	h.release()
	if rec.Body.Len() != (replayBufferMax/len(chunk)+2)*len(chunk) {
		t.Fatal("release after a pass-through wrote the reply twice")
	}

	small := httptest.NewRecorder()
	held := newHeldReply(small)
	held.WriteHeader(http.StatusTeapot)
	_, _ = held.Write([]byte("short"))
	if small.Body.Len() != 0 {
		t.Fatal("a held reply reached the client before its release")
	}
	held.release()
	if small.Code != http.StatusTeapot || small.Body.String() != "short" {
		t.Fatalf("released %d %q", small.Code, small.Body.String())
	}
}
