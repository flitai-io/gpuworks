package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestKnownInstanceFailureEndsWaitingRequest(t *testing.T) {
	for _, headers := range []bool{false, true} {
		name := "before_headers"
		if headers {
			name = "after_headers"
		}
		t.Run(name, func(t *testing.T) {
			const partialFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
			started := make(chan struct{})
			stop := make(chan struct{})
			var logs syncBuffer
			p, key, closeUpstream := newObservedProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if headers {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, partialFrame)
					w.(http.Flusher).Flush()
				}
				close(started)
				select {
				case <-r.Context().Done():
				case <-stop:
				}
			}), &logs)
			defer closeUpstream()
			records := make(chan uxrecord.Request, 1)
			p.SetRequestSink(func(r uxrecord.Request) { records <- r })
			server := httptest.NewServer(p.Handler())
			defer server.Close()
			defer close(stop)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni","messages":[{"role":"user","content":"long prompt"}],"stream":true}`))
			req.Header.Set("Authorization", "Bearer "+key)
			type result struct {
				status int
				retry  string
				body   string
				err    error
			}
			done := make(chan result, 1)
			partialReceived := make(chan struct{})
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- result{err: err}
					return
				}
				defer resp.Body.Close()
				var prefix []byte
				if headers {
					prefix = make([]byte, len(partialFrame))
					n, err := io.ReadFull(resp.Body, prefix)
					prefix = prefix[:n]
					if resp.StatusCode != http.StatusOK || err != nil || string(prefix) != partialFrame {
						done <- result{status: resp.StatusCode, body: string(prefix), err: err}
						return
					}
					close(partialReceived)
				}
				body, err := io.ReadAll(resp.Body)
				done <- result{status: resp.StatusCode, retry: resp.Header.Get("Retry-After"), body: string(append(prefix, body...)), err: err}
			}()
			ready := started
			if headers {
				ready = partialReceived
			}
			select {
			case <-ready:
			case got := <-done:
				t.Fatalf("request ended before failure trigger: %+v", got)
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach failure trigger phase")
			}
			if err := p.store.SetInstanceState("omni-instance", proto.InstFailed, "container exited (code -6)"); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if headers {
					if got.status != 200 || got.err == nil || !strings.Contains(got.body, "partial") {
						t.Fatalf("partial response=%+v", got)
					}
				} else if got.status != 503 || got.retry == "" || !strings.Contains(got.body, "instance") {
					t.Fatalf("waiting response=%+v", got)
				}
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("known failed instance left client waiting")
			}
			select {
			case rec := <-records:
				if rec.Outcome != uxrecord.OutcomeUpstreamError {
					t.Fatalf("outcome=%q, want upstream error", rec.Outcome)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request record missing")
			}
			p.guard.mu.Lock()
			registrations := len(p.guard.calls)
			p.guard.mu.Unlock()
			if registrations != 0 {
				t.Fatalf("guard registrations leaked: %d", registrations)
			}
			if live := p.LiveRequests(1, 10); len(live.Requests) != 0 {
				t.Fatalf("finished request leaked: %+v", live)
			}
		})
	}
}

func TestInstanceGuardPreservesHealthyDrainingAndSleep(t *testing.T) {
	var logs syncBuffer
	p, _, closeUpstream := newObservedProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &logs)
	defer closeUpstream()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	call := &guardedCall{port: 30000, cancel: cancel}
	p.guard.add("omni-instance", call)
	defer p.guard.remove("omni-instance", call)
	for _, state := range []string{proto.InstRunning, proto.InstDraining} {
		if err := p.store.SetInstanceState("omni-instance", state, "healthy"); err != nil {
			t.Fatal(err)
		}
		p.guard.checkMissing(map[string]bool{}) // Route removal alone is not failure.
		if ctx.Err() != nil {
			t.Fatalf("healthy %s request canceled: %v", state, context.Cause(ctx))
		}
	}
	if err := p.store.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Updates(map[string]any{"state": proto.InstRunning, "sleep_level": 1}).Error; err != nil {
		t.Fatal(err)
	}
	p.guard.check("omni-instance")
	if ctx.Err() != nil {
		t.Fatalf("sleep transition interrupted active call: %v", context.Cause(ctx))
	}
	p.Close()
	p.store.Close()
	p.guard.check("omni-instance")
	if ctx.Err() != nil {
		t.Fatal("database read failure canceled a healthy request")
	}
}

func TestInstanceGuardChecksDeletionAndPortChanges(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			var logs syncBuffer
			p, _, closeUpstream := newObservedProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &logs)
			defer closeUpstream()
			oldCtx, oldCancel := context.WithCancelCause(context.Background())
			defer oldCancel(nil)
			oldCall := &guardedCall{port: 30000, cancel: oldCancel}
			p.guard.add("omni-instance", oldCall)
			defer p.guard.remove("omni-instance", oldCall)
			if deleted {
				if err := p.store.DeleteInstance("omni-instance"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := p.store.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("api_port", 30001).Error; err != nil {
					t.Fatal(err)
				}
			}
			newCtx, newCancel := context.WithCancelCause(context.Background())
			defer newCancel(nil)
			newCall := &guardedCall{port: 30001, cancel: newCancel}
			p.guard.add("omni-instance", newCall)
			defer p.guard.remove("omni-instance", newCall)
			p.guard.check("omni-instance")
			if !errors.Is(context.Cause(oldCtx), errInstanceUnavailable) {
				t.Fatal("retired endpoint request was not canceled")
			}
			if !deleted && newCtx.Err() != nil {
				t.Fatal("new endpoint request was canceled with old generation")
			}
		})
	}
}

func TestInstanceGuardReleasesFailedAttemptsAndIsConcurrentSafe(t *testing.T) {
	var logs syncBuffer
	p, _, closeUpstream := newObservedProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &logs)
	defer closeUpstream()
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for count := 0; count < 10; count++ {
				req := httptest.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", nil)
				_, err := p.guard.roundTrip(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }), req, candidate{instanceUUID: "omni-instance", port: 30000})
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("transport error lost: %v", err)
				}
				p.guard.check("omni-instance")
			}
		}()
	}
	wg.Wait()
	p.guard.mu.Lock()
	defer p.guard.mu.Unlock()
	if len(p.guard.calls) != 0 {
		t.Fatalf("attempt registrations leaked: %d", len(p.guard.calls))
	}
}

func TestInstanceFailureAfterCompletedReplyKeepsCompletedOutcome(t *testing.T) {
	ob := &copyObservation{log: slog.New(slog.NewTextHandler(io.Discard, nil)), started: time.Now(), status: 200, declaredLen: -1}
	ob.response = newResponseObserver(chatPath, 200, sseHeader(), ob.started)
	ob.response.observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))
	var outcome uxrecord.Outcome
	ob.onFinish = func(_ int, _ time.Duration, _ responseStats, result uxrecord.Outcome) { outcome = result }
	ob.finish(errInstanceUnavailable)
	if outcome != uxrecord.OutcomeOK {
		t.Fatalf("completed reply classified as %q", outcome)
	}
}
