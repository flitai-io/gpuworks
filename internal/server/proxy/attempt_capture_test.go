package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type attemptCaptureHub struct {
	dialTestHub
	traces chan *proto.TunnelTrace
}

func (h *attemptCaptureHub) OpenTunnel(ctx context.Context, node, dst string) (net.Conn, error) {
	h.traces <- proto.TunnelTraceFrom(ctx)
	return h.dialTestHub.OpenTunnel(ctx, node, dst)
}

func TestCapturedAttemptsJoinEngineHeadersAndTunnel(t *testing.T) {
	received := make(chan http.Header, 3)
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- r.Header.Clone()
		if r.Header.Get(debugAttemptHeader) == "1" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	p, _, key, depID, closeEngine := newAccountingProxy(t, engine)
	defer closeEngine()
	inst, err := p.store.InstanceByUUID("omni-instance")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.store.CreateInstance(&store.Instance{UUID: "backup", DeploymentID: depID, NodeID: inst.NodeID, State: proto.InstRunning, APIPort: 30001, Ports: "[30001]"})
	if err != nil {
		t.Fatal(err)
	}
	p.routes.rebuild()
	hub := &attemptCaptureHub{dialTestHub: p.hub.(dialTestHub), traces: make(chan *proto.TunnelTrace, 3)}
	p.hub = hub
	p.rp.Transport.(*retryTransport).hub = hub
	sendRequest := func() {
		req := httptest.NewRequest("POST", chatPath, strings.NewReader(`{"model":"omni"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "example-request")
		req.Header.Set(debugAttemptHeader, "999")
		out := httptest.NewRecorder()
		p.Handler()(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("status %d", out.Code)
		}
	}
	sendRequest()
	if (<-received).Get(debugAttemptHeader) != "" || <-hub.traces != nil {
		t.Fatal("uncaptured request enabled attempt diagnostics")
	}

	recorder := &oneCaptureRecorder{}
	p.SetRequestRecorder(recorder)
	sendRequest()
	if len(received) != 2 || len(hub.traces) != 2 {
		t.Fatal("expected two captured engine and tunnel attempts")
	}
	for i, number := range []string{"1", "2"} {
		headers, trace := <-received, <-hub.traces
		if headers.Get("X-Request-ID") != "example-request" || headers.Get(debugAttemptHeader) != number ||
			trace == nil || trace.RequestID != "example-request" || trace.Attempt != i+1 {
			t.Fatalf("unmatched attempt %d", i+1)
		}
	}
	attempts := capturedAttemptEvents(t, recorder)
	if len(attempts) != 2 {
		t.Fatalf("logged attempts: %v", attempts)
	}
	for i, e := range attempts {
		if e["request_id"] != "example-request" || e["attempt"] != float64(i+1) || e["client_canceled"] != false || e["body_replayable"] != true {
			t.Fatalf("invalid attempt %v", e)
		}
	}
	if attempts[0]["error"] != "EOF" || attempts[0]["received_first_byte"] != false || attempts[1]["received_first_byte"] != true || attempts[1]["status"] != float64(200) || attempts[0]["instance_uuid"] == attempts[1]["instance_uuid"] {
		t.Fatalf("attempts: %v", attempts)
	}
}

func capturedAttemptEvents(t *testing.T, recorder *oneCaptureRecorder) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(recorder.file.Bytes()))
	var attempts []map[string]any
	for scanner.Scan() {
		var e map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e["t"] == "attempt" {
			attempts = append(attempts, e)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return attempts
}

func TestCapturedAttemptRecordsPartialResponseHeader(t *testing.T) {
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nX-Incomplete:")
		if err != nil {
			t.Error(err)
		}
	})
	p, key, closeUpstream := newObservedProxy(t, engine, &syncBuffer{})
	defer closeUpstream()
	recorder := &oneCaptureRecorder{}
	p.SetRequestRecorder(recorder)
	req := httptest.NewRequest("POST", chatPath, strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	p.Handler()(out, req)
	if out.Code != http.StatusBadGateway {
		t.Fatalf("status %d", out.Code)
	}
	attempts := capturedAttemptEvents(t, recorder)
	if len(attempts) != 1 {
		t.Fatalf("expected one failed attempt, got %v", attempts)
	}
	e := attempts[0]
	if e["received_first_byte"] != true || e["status"] != nil || e["error"] == nil {
		t.Fatalf("partial header evidence: %v", e)
	}
}
