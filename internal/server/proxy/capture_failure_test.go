package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type oneCaptureRecorder struct{ file memFile }

func (r *oneCaptureRecorder) Begin(uint, string) io.WriteCloser { return &r.file }

func capturedEnd(t *testing.T, r *oneCaptureRecorder) map[string]any {
	t.Helper()
	if !r.file.closed {
		t.Fatal("capture not closed")
	}
	var end map[string]any
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(r.file.Bytes()))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["t"] == "end" {
			end = event
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 1 || end["observation_scope"] != "gateway" {
		t.Fatalf("terminal capture count=%d end=%v", count, end)
	}
	return end
}

func TestDebugSessionUpstreamEOFBeforeHeadersKeepsReadFailure(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			// Accept the whole request, then close without any response headers.
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			})
			logs := &syncBuffer{}
			p, key, closeUpstream := newObservedProxy(t, upstream, logs)
			t.Cleanup(closeUpstream)
			if wrapped {
				// Exercise the same ReverseProxy ErrorHandler with a wrapped EOF.
				p.rp.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("read upstream response: %w", io.EOF)
				})
			}
			recorder := &oneCaptureRecorder{}
			p.SetRequestRecorder(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", "session=private-cookie")
			out := httptest.NewRecorder()
			p.Handler().ServeHTTP(out, req)
			end := capturedEnd(t, recorder)
			wantErr := "EOF"
			if wrapped {
				wantErr = "read upstream response: EOF"
			}
			if out.Code != http.StatusBadGateway || end["status"] != float64(502) ||
				end["outcome"] != "upstream_error" || end["read_error"] != wantErr ||
				end["write_error"] != nil || end["copied_bytes"] != float64(0) {
				t.Fatalf("status=%d terminal upstream evidence=%v", out.Code, end)
			}
			if strings.Contains(recorder.file.String(), key) || strings.Contains(recorder.file.String(), "private-cookie") {
				t.Fatal("request capture leaked authentication headers")
			}
			if !hasLogMsg(logs, "proxy upstream error") || hasLogMsg(logs, "proxy response body truncated") || hasLogMsg(logs, "proxy response body copied") {
				t.Fatalf("pre-response error was misclassified as a body copy: %s", logs.String())
			}
		})
	}
}

func TestDebugSessionUpstreamTruncationKeepsReadFailure(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = fmt.Fprint(brw, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\nConnection: close\r\n\r\nshort")
		_ = brw.Flush()
		_ = conn.Close()
	})
	logs := &syncBuffer{}
	p, key, closeUpstream := newObservedProxy(t, upstream, logs)
	defer closeUpstream()
	recorder := &oneCaptureRecorder{}
	p.SetRequestRecorder(recorder)
	front := httptest.NewServer(p.Handler())
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("truncated upstream looked complete")
	}
	waitForLog(t, logs, "proxy response body truncated")
	end := capturedEnd(t, recorder)
	if end["outcome"] != "truncated" || end["read_error"] == nil || end["write_error"] != nil || end["finish_reason"] != "" {
		t.Fatalf("upstream evidence=%v", end)
	}
}

type captureFailWriter struct{ header http.Header }

func (w *captureFailWriter) Header() http.Header       { return w.header }
func (w *captureFailWriter) WriteHeader(int)           {}
func (w *captureFailWriter) Write([]byte) (int, error) { return 0, errors.New("downstream reset") }
func (w *captureFailWriter) Flush()                    {}

func TestDebugSessionClientWriteFailureKeepsObservedBoundary(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`)
	})
	logs := &syncBuffer{}
	p, key, closeUpstream := newObservedProxy(t, upstream, logs)
	defer closeUpstream()
	recorder := &oneCaptureRecorder{}
	p.SetRequestRecorder(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	func() {
		defer func() {
			if got := recover(); got != nil && got != http.ErrAbortHandler {
				t.Errorf("unexpected panic=%v", got)
			}
		}()
		p.Handler().ServeHTTP(&captureFailWriter{header: make(http.Header)}, req)
	}()
	end := capturedEnd(t, recorder)
	if end["outcome"] != "client_closed" || end["write_error"] != "downstream reset" || end["copied_bytes"] != float64(0) {
		t.Fatalf("client evidence=%v", end)
	}
}
