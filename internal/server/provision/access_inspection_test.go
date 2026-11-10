package provision

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// go test -race ./internal/server/provision -run TestAccessInspection -count=1 -timeout 40m
func TestAccessInspectionChecksCurrentStateOnEveryRequest(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		requests.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	dir, bin := connectionDiagnosticFixture(t, "owned", server.URL)
	writeExecutable(t, filepath.Join(dir, "gpuworks-agent"), "exit 0")
	line := dir + "/gpuworks-agent run --state " + dir
	writeExecutable(t, filepath.Join(bin, "ps"), "printf '%s\\n' "+shellQuote(line))
	// Neither an old log nor a stale snapshot may affect this request's verdict.
	for _, file := range []string{"agent.log", "agent-connection.json"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("old authentication failure token=secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
		calls.Add(1)
		token, _ := io.ReadAll(input)
		if string(token) != "owned\n" || strings.Contains(cmd, "owned") {
			t.Error("token must travel through stdin")
		}
		for _, forbidden := range []string{"agent.log", "agent-connection.json", "install.sh", "systemctl restart"} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("unexpected diagnostic operation: %s", forbidden)
			}
		}
		return executeConnectionDiagnostic(t, cmd, token, bin)
	})
	req := SSHProbeRequest{Host: host, Port: port, User: "root", InstallDir: dir, Token: "owned", ConnectMode: "agent_dial", ServerURL: server.URL, SSHAuth: SSHAuth{Password: "pw"}}
	installer := NewSSHInstaller(nil)
	got := installer.InspectAccess(context.Background(), req)
	if got.SSH != "passed" || got.Installation != "passed" || got.Process != "passed" || got.Connection.Outcome != "http_ok" || got.Connection.Target != server.URL+"/healthz" || got.Connection.ObservedAt.IsZero() {
		t.Fatalf("first inspection: %+v", got)
	}
	status.Store(503)
	writeExecutable(t, filepath.Join(bin, "ps"), "exit 0")
	got = installer.InspectAccess(context.Background(), req)
	if got.Process != "failed" || got.Connection.Outcome != "http_error" || requests.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("rerun reused history: %+v HTTP=%d SSH=%d", got, requests.Load(), calls.Load())
	}
}

func TestAccessInspectionServerDialSkipsNodeHTTPAndSupportsLegacyProcess(t *testing.T) {
	dir, bin := connectionDiagnosticFixture(t, "owned", "")
	if err := os.Remove(filepath.Join(dir, "gpuworks-agent.conf")); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dir, "gpuworks-agent"), "exit 0")
	line := dir + "/gpuworks-agent run --state " + dir + " --token owned --connect-mode server_dial"
	writeExecutable(t, filepath.Join(bin, "ps"), "printf '%s\\n' "+shellQuote(line))
	marker := filepath.Join(t.TempDir(), "http-called")
	writeExecutable(t, filepath.Join(bin, "curl"), "touch "+shellQuote(marker))
	stdout, stderr, exit := executeConnectionDiagnostic(t, accessInspectionCommand(dir, "", "server_dial"), []byte("owned\n"), bin)
	if exit != 0 || stderr != "" {
		t.Fatalf("%d %s", exit, stderr)
	}
	got := parseAccessInspection(stdout, AccessInspection{Reason: "inspection_unavailable"})
	if got.Process != "passed" || got.Connection.Outcome != "not_applicable" {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("server_dial ran a node HTTP probe")
	}
}

func TestAccessInspectionDoesNotConfuseMissingProcessWithFailedInspection(t *testing.T) {
	for _, tc := range []struct {
		name, ps, token, process, connection string
		installed                            bool
	}{
		{"not installed", "exit 0", "owned", "failed", "http_ok", false},
		{"stopped", "exit 0", "owned", "failed", "http_ok", true},
		{"process listing failed", "exit 1", "owned", "unavailable", "http_ok", true},
		{"foreign installation", "current", "other", "unavailable", "identity_unverified", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := connectionDiagnosticFixture(t, tc.token, "http://server")
			if tc.installed {
				writeExecutable(t, filepath.Join(dir, "gpuworks-agent"), "exit 0")
			}
			ps := tc.ps
			if ps == "current" {
				ps = "printf '%s\\n' " + shellQuote(dir+"/gpuworks-agent run --state "+dir)
			}
			writeExecutable(t, filepath.Join(bin, "ps"), ps)
			writeExecutable(t, filepath.Join(bin, "curl"), "printf 200")
			raw, _, exit := executeConnectionDiagnostic(t, accessInspectionCommand(dir, "http://server", "agent_dial"), []byte("owned\n"), bin)
			if exit != 0 {
				t.Fatalf("exit %d", exit)
			}
			got := parseAccessInspection(raw, AccessInspection{Reason: "inspection_unavailable"})
			if got.Process != tc.process || got.Connection.Outcome != tc.connection || (got.Installation == "passed") != tc.installed {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestAccessInspectionInvalidOutputIsUnavailableAndNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	for _, raw := range []string{"{}", "GW_INSTALL passed\nGW_PROCESS passed\nprivate-token\n", strings.Repeat("private-token", 10000)} {
		host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) { return raw, "private-token", 0 })
		got := NewSSHInstaller(nil).InspectAccess(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "private-token", ConnectMode: "server_dial", SSHAuth: SSHAuth{Password: "pw"}})
		if got.Process != "unavailable" || got.Installation != "unavailable" {
			t.Fatalf("%+v", got)
		}
	}
	if strings.Contains(logs.String(), "private-token") {
		t.Fatal("diagnostic output leaked")
	}
}

func TestAccessInspectionCancellationReturnsWithoutRecovery(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
		calls.Add(1)
		_, _ = io.ReadAll(input)
		close(started)
		<-release
		return "", "", 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan AccessInspection, 1)
	go func() {
		done <- NewSSHInstaller(nil).InspectAccess(ctx, SSHProbeRequest{Host: host, Port: port, User: "root", Token: "owned", ConnectMode: "server_dial", SSHAuth: SSHAuth{Password: "pw"}})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("inspection did not start")
	}
	cancel()
	select {
	case got := <-done:
		if got.Process != "unavailable" || got.Reason != "cancelled" || calls.Load() != 1 {
			t.Fatalf("%+v calls=%d", got, calls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled inspection did not return")
	}
}
