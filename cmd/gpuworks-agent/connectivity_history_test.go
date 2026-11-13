package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"

	"gpuworks.ai/gpuworks/internal/proto"
)

func historyEvent(at time.Time, extra string) string {
	return fmt.Sprintf("time=%s level=INFO msg=connection_event_v1 source=agent_runtime stage=session outcome=failed reason=timeout attempt=%s lane=ctrl observed_at=%s server_url=https://node.example/prefix %s\n", at.Format(time.RFC3339Nano), strings.Repeat("a", 32), at.Format(time.RFC3339Nano), extra)
}
func TestConnectivityHistoryKeepsDaemonFailureSeparate(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour).UTC()
	if err := os.WriteFile(filepath.Join(dir, "agent.log.1"), []byte(historyEvent(at, "suppressed_retries=3")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte("old raw token=private-secret\n"+strings.ReplaceAll(historyEvent(time.Now(), ""), "source=agent_runtime", "source=cli")), 0600); err != nil {
		t.Fatal(err)
	}
	report := proto.DoctorReport{Facts: []proto.DoctorFact{{Code: "connection.source", Value: "cli"}, {Code: "connection.outcome", Value: "ok"}}}
	addConnectionHistory(&report, dir, "private-secret")
	if proto.ConnectionValue(report, "connection.history_state") != "found" || proto.ConnectionValue(report, "connection.last_failure.at") != at.Format(time.RFC3339Nano) || proto.ConnectionValue(report, "connection.source") != "cli" || proto.ConnectionValue(report, "connection.outcome") != "ok" {
		t.Fatalf("history changed current result: %+v", report.Facts)
	}
}
func TestConnectivityHistoryRejectsHostileMalformedAndPartial(t *testing.T) {
	base := historyEvent(time.Now().UTC(), "")
	cases := []string{
		strings.TrimSuffix(base, "\n"),
		strings.Replace(base, "reason=timeout", "reason=private-secret", 1),
		strings.Replace(base, "stage=session", "stage=private-secret", 1),
		strings.Replace(base, "server_url=https://node.example/prefix", "server_url=https://user:private-secret@node.example/", 1),
		strings.Replace(base, "server_url=https://node.example/prefix", "server_url=https://node.example/?token=private-secret", 1),
		strings.Replace(base, "attempt="+strings.Repeat("a", 32), "attempt=private-secret", 1),
		strings.TrimSuffix(base, "\n") + " raw_detail=private-secret\n",
		strings.TrimSuffix(base, "\n") + " suppressed_retries=-1\n",
		strings.TrimSuffix(base, "\n") + " source=agent_runtime\n",
	}
	for i, line := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte(line), 0600); err != nil {
				t.Fatal(err)
			}
			failure, status := readConnectionHistory(dir, []string{"private-secret"})
			if failure != nil || status != "unknown" {
				t.Fatalf("unsafe event accepted: %v %v", failure, status)
			}
		})
	}
}
func TestConnectivityHistoryByteAndEventLimits(t *testing.T) {
	t.Run("bytebudget", func(t *testing.T) {
		dir := t.TempDir()
		at := time.Now().UTC()
		data := strings.Repeat("x", connectionHistoryBytes+10) + "\n" + historyEvent(at, "")
		_ = os.WriteFile(filepath.Join(dir, "agent.log"), []byte(data), 0600)
		_ = os.WriteFile(filepath.Join(dir, "agent.log.1"), []byte(historyEvent(at.Add(time.Hour), "")), 0600)
		failure, status := readConnectionHistory(dir, nil)
		if status != "found" || failure["at"] != at.Format(time.RFC3339Nano) {
			t.Fatalf("tail budget bypassed: %v %s", failure, status)
		}
	})
	t.Run("events", func(t *testing.T) {
		dir := t.TempDir()
		line := historyEvent(time.Now(), "")
		success := strings.Replace(line, "outcome=failed", "outcome=ok", 1)
		data := line + strings.Repeat(success, connectionHistoryEvents)
		_ = os.WriteFile(filepath.Join(dir, "agent.log"), []byte(data), 0600)
		failure, status := readConnectionHistory(dir, nil)
		if failure != nil || status != "unknown" {
			t.Fatalf("event cap bypassed: %v %v", failure, status)
		}
	})
}
func TestConnectivityHistoryMissingUnsafeAndPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, status := readConnectionHistory(dir, nil); status != "unknown" {
		t.Fatal(status)
	}
	target := filepath.Join(t.TempDir(), "private-log")
	_ = os.WriteFile(target, []byte(historyEvent(time.Now(), "")), 0600)
	if err := os.Symlink(target, filepath.Join(dir, "agent.log")); err != nil {
		t.Fatal(err)
	}
	if failure, status := readConnectionHistory(dir, nil); failure != nil || status != "unavailable" {
		t.Fatalf("followed unrelated symlink: %v %s", failure, status)
	}
	_ = os.Remove(filepath.Join(dir, "agent.log"))
	_ = os.WriteFile(filepath.Join(dir, "agent.log"), []byte(historyEvent(time.Now(), "")), 0000)
	_, status := readConnectionHistory(dir, nil)
	if os.Geteuid() != 0 && status != "unavailable" {
		t.Fatalf("permission error not represented: %s", status)
	}
	if os.Geteuid() == 0 && status != "found" {
		t.Fatalf("root-readable log was falsely unavailable: %s", status)
	}
}

func TestConnectivityHistoryConsumesRealRuntimeEvent(t *testing.T) {
	var log bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	runtime := agentconn.New(agentconn.Credentials{ServerURL: server.URL + "/route=prefix", NodeToken: "private-node-token"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(&log, nil)))
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("expected actual WS refusal")
	}
	runtime.RecordReconnect(time.Minute)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.log"), log.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	failure, state := readConnectionHistory(dir, []string{"private-node-token"})
	if state != "found" || failure["stage"] != "ws_upgrade" || failure["reason"] != "ws_upgrade" || failure["lane"] != "single" {
		t.Fatalf("real producer event not consumed: %v %s; %s", failure, state, log.String())
	}
}

func TestConnectivityR10DecodedHistorySecretsAreRejected(t *testing.T) {
	const secret = "deadbeefdeadbeef"
	hexEscaped, unicodeEscaped, percentEscaped := "", "", ""
	for _, b := range []byte(secret) {
		hexEscaped += fmt.Sprintf("\\x%02x", b)
		unicodeEscaped += fmt.Sprintf("\\u%04x", b)
		percentEscaped += fmt.Sprintf("%%%02x", b)
	}
	for _, tc := range []struct{ name, line string }{
		{"hex", strings.Replace(historyEvent(time.Now(), ""), "attempt="+strings.Repeat("a", 32), "attempt=\""+hexEscaped+"\"", 1)},
		{"unicode", strings.Replace(historyEvent(time.Now(), ""), "attempt="+strings.Repeat("a", 32), "attempt=\""+unicodeEscaped+"\"", 1)},
		{"url", strings.Replace(historyEvent(time.Now(), ""), "server_url=https://node.example/prefix", "server_url=https://node.example/"+percentEscaped, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte(tc.line), 0600); err != nil {
				t.Fatal(err)
			}
			report := proto.DoctorReport{}
			addConnectionHistory(&report, dir, secret)
			if proto.ConnectionValue(report, "connection.history_state") != "unknown" || proto.ConnectionValue(report, "connection.last_failure.attempt") != "" {
				t.Fatal("decoded credential-bearing event was trusted")
			}
			data, _ := json.Marshal(report)
			if strings.Contains(string(data), secret) {
				t.Fatal("decoded credential leaked into history")
			}
		})
	}
	t.Run("proxy-password", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://user:"+secret+"@proxy.invalid")
		line := strings.Replace(historyEvent(time.Now(), ""), "attempt="+strings.Repeat("a", 32), "attempt=\""+hexEscaped+"\"", 1)
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "agent.log"), []byte(line), 0600)
		report := proto.DoctorReport{}
		addConnectionHistory(&report, dir, "different-node-token")
		if proto.ConnectionValue(report, "connection.history_state") != "unknown" {
			t.Fatal("decoded proxy credential was trusted")
		}
	})
}

func TestConnectivityHistorySelectedEventTargetAndRetryStayHistorical(t *testing.T) {
	t.Run("history-A-current-B", func(t *testing.T) {
		var log bytes.Buffer
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
		defer server.Close()
		targetA := server.URL + "/history=prefix"
		runtime := agentconn.New(agentconn.Credentials{ServerURL: targetA, NodeToken: "private-history-token"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(&log, nil)))
		if err := runtime.Run(context.Background()); err == nil {
			t.Fatal("expected actual historical WS refusal")
		}
		runtime.RecordReconnect(0)
		historicalRetry := proto.ConnectionValue(runtime.ConnectionReport(), "connection.retry_at")
		at, err := time.Parse(time.RFC3339Nano, historicalRetry)
		if err != nil || at.After(time.Now()) {
			t.Fatalf("expected already planned retry: %s %v", historicalRetry, err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agent.log"), log.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		report := proto.DoctorReport{Facts: []proto.DoctorFact{{Code: "connection.source", Value: "cli"}, {Code: "connection.outcome", Value: "ok"}, {Code: "connection.server_url", Value: "https://current-B.example/current"}}}
		addConnectionHistory(&report, dir, "private-history-token")
		if proto.ConnectionValue(report, "connection.last_failure.server_url") != targetA || proto.ConnectionValue(report, "connection.last_failure.retry_at") != historicalRetry || proto.ConnectionValue(report, "connection.server_url") != "https://current-B.example/current" || proto.ConnectionValue(report, "connection.retry_at") != "" {
			t.Fatal("historical selected event target/retry was lost or replaced by current state")
		}
	})
	t.Run("missing-is-not-borrowed", func(t *testing.T) {
		var log bytes.Buffer
		peer, accepted := net.Pipe()
		_ = peer.Close()
		runtime := agentconn.New(agentconn.Credentials{NodeToken: "private-history-token", ConnectMode: "server_dial"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(&log, nil)))
		if err := runtime.RunAccepted(context.Background(), accepted); err == nil {
			t.Fatal("expected own inbound pre-auth close")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agent.log"), log.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		report := proto.DoctorReport{Facts: []proto.DoctorFact{{Code: "connection.source", Value: "cli"}, {Code: "connection.server_url", Value: "https://current-B.example"}, {Code: "connection.retry_at", Value: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}}}
		addConnectionHistory(&report, dir, "private-history-token")
		if proto.ConnectionValue(report, "connection.history_state") != "found" {
			t.Fatal("missing optional fields made valid history unavailable")
		}
		for _, fact := range report.Facts {
			if fact.Code == "connection.last_failure.server_url" || fact.Code == "connection.last_failure.retry_at" {
				t.Fatal("history borrowed current optional fields")
			}
		}
	})
}
