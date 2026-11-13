package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDoctorConnectivityProcess(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_DOCTOR_CLI") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"gpuworks-agent"}, os.Args[i+1:]...)
			break
		}
	}
	main()
	os.Exit(0)
}

func doctorCLI(t *testing.T, state string, args ...string) ([]byte, error) {
	return agentDiagnosticCLI(t, state, "doctor", args...)
}

func agentDiagnosticCLI(t *testing.T, state, subcommand string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestDoctorConnectivityProcess$", "--", subcommand}, args...)...)
	cmd.Env = append(os.Environ(), "GPUWORKS_TEST_DOCTOR_CLI=1", "GPUWORKS_STATE_DIR="+state, "GPUWORKS_SERVER=", "GPUWORKS_TOKEN=", "GPUWORKS_CONNECT_MODE=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	for _, secret := range []string{"proxy-password", "query-secret", "node-doctor-secret"} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("stderr leaked secret %q", secret)
		}
	}
	return stdout.Bytes(), err
}

func TestDoctorConnectivityServerDialJSON(t *testing.T) {
	state := t.TempDir()
	if err := agentconf.Write(state, agentconf.Config{Token: "node-doctor-secret", ConnectMode: agentconf.ServerDial}); err != nil {
		t.Fatal(err)
	}
	data, err := doctorCLI(t, state, "--connectivity", "--state", state, "--json")
	if err != nil {
		t.Fatalf("doctor connection mode: %v; %s", err, data)
	}
	var report proto.DoctorReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("stdout is not one report: %v: %s", err, data)
	}
	found := false
	for _, f := range report.Facts {
		if f.Code == "connection.outcome" && f.Value == "not_applicable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing server_dial N/A: %s", data)
	}
	if strings.Contains(string(data), "node-doctor-secret") {
		t.Fatal("token leaked")
	}
}

func TestDoctorConnectivityInvalidURLSafeJSON(t *testing.T) {
	state := t.TempDir()
	data, err := doctorCLI(t, state, "--connectivity", "--server", "http://user:proxy-password@example.invalid/?token=query-secret", "--token", "node-doctor-secret", "--json")
	if err == nil {
		t.Fatal("invalid URL succeeded")
	}
	for _, secret := range []string{"proxy-password", "query-secret", "node-doctor-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret %q leaked", secret)
		}
	}
	var report proto.DoctorReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("failure stdout is not one report: %v: %s", err, data)
	}
}

func TestDoctorConnectivityRejectsInvalidModeFromEverySource(t *testing.T) {
	for _, source := range []string{"state_file", "environment", "flag"} {
		t.Run(source, func(t *testing.T) {
			for _, entry := range launchEnv {
				t.Setenv(entry.name, "")
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			state := t.TempDir()
			mode := "agent_dial"
			if source == "state_file" {
				mode = "illegal-private-mode"
			}
			_ = os.WriteFile(agentconf.Path(state), []byte("SERVER="+server.URL+"\nTOKEN=private-mode-token\nCONNECT_MODE="+mode+"\n"), 0600)
			args := []string{"--connectivity", "--state", state, "--json"}
			if source == "environment" {
				t.Setenv("GPUWORKS_CONNECT_MODE", "illegal-private-mode")
			}
			if source == "flag" {
				args = append(args, "--connect-mode", "illegal-private-mode")
			}
			var out, stderr bytes.Buffer
			err := doctor(context.Background(), slog.New(slog.NewTextHandler(&stderr, nil)), args, &out, &stderr)
			var report proto.DoctorReport
			if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || proto.ConnectionValue(report, "connection.outcome") != "invalid_configuration" || requests.Load() != 0 {
				t.Fatalf("invalid %s mode bypassed: %+v %v requests=%d", source, report.Facts, err, requests.Load())
			}
			if strings.Contains(out.String()+stderr.String(), "illegal-private-mode") {
				t.Fatal("mode leaked")
			}
		})
	}
}

func TestDoctorConnectivityPlainDoctorDispatchAndSource(t *testing.T) {
	previous := doctorEnvironment
	defer func() { doctorEnvironment = previous }()
	calls := 0
	doctorEnvironment = func(_ context.Context, out io.Writer, state string) { calls++; fmt.Fprint(out, "legacy doctor") }
	var out, stderr bytes.Buffer
	if err := doctor(context.Background(), slog.Default(), []string{"--state", t.TempDir()}, &out, &stderr); err != nil || calls != 1 || out.String() != "legacy doctor" {
		t.Fatalf("legacy dispatch changed: %s %v %d", out.String(), err, calls)
	}
	if err := doctor(context.Background(), slog.Default(), []string{"--json"}, &out, &stderr); err == nil || calls != 1 {
		t.Fatal("JSON without selector ran hardware doctor")
	}
	for _, tc := range []struct{ env, flag, want string }{{"", "", "state_file"}, {"https://env.example", "", "environment"}, {"https://env.example", "https://flag.example", "flag"}} {
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		launch := newLaunchFlags(fs)
		args := []string{}
		if tc.flag != "" {
			args = append(args, "--server", tc.flag)
		}
		_ = fs.Parse(args)
		got := connectionServerSource(launch, "https://file.example", true, func(string) string { return tc.env })
		if got != tc.want {
			t.Fatalf("source=%s want %s", got, tc.want)
		}
	}
}

func TestDoctorConnectivityValidatesServerDialListen(t *testing.T) {
	for _, entry := range launchEnv {
		t.Setenv(entry.name, "")
	}
	state := t.TempDir()
	_ = os.WriteFile(agentconf.Path(state), []byte("TOKEN=private-mode-token\nCONNECT_MODE=server_dial\nLISTEN=:99999\n"), 0600)
	var out, stderr bytes.Buffer
	err := doctor(context.Background(), slog.Default(), []string{"--connectivity", "--state", state, "--json"}, &out, &stderr)
	var report proto.DoctorReport
	if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || proto.ConnectionValue(report, "connection.outcome") != "invalid_configuration" {
		t.Fatalf("invalid full configuration bypassed: %s %v", out.String(), err)
	}
}

func TestDoctorConnectivityTransportBudgetAndCLIRejectExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		raw := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		session, err := yamux.Server(raw, yamux.DefaultConfig(), nil)
		if err != nil {
			return
		}
		defer session.Close()
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		reader := bufio.NewReader(stream)
		_, _ = reader.ReadString('\n')
		var hello proto.Envelope
		_ = json.NewDecoder(reader).Decode(&hello)
		body, _ := json.Marshal(proto.Reject{Reason: "bad_token", Detail: "node-doctor-secret proxy-password query-secret"})
		_ = json.NewEncoder(stream).Encode(proto.Envelope{T: proto.TReject, D: body})
		_, _ = io.Copy(io.Discard, stream)
	}))
	defer server.Close()
	state := t.TempDir()
	if err := agentconf.Write(state, agentconf.Config{Server: server.URL, Token: "node-doctor-secret"}); err != nil {
		t.Fatal(err)
	}
	data, err := doctorCLI(t, state, "--connectivity", "--state", state, "--json")
	var exit *exec.ExitError
	var report proto.DoctorReport
	if !errors.As(err, &exit) || exit.ExitCode() != 4 || json.Unmarshal(data, &report) != nil || proto.ConnectionValue(report, "connection.reason") != "bad_token" {
		t.Fatalf("actual CLI reject exit/report: %s %v", data, err)
	}
}

func TestDoctorConnectivityTimeoutExitCodes(t *testing.T) {
	for _, subcommand := range []string{"doctor", "smoke"} {
		t.Run(subcommand, func(t *testing.T) {
			released := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(released) }))
			defer server.Close()
			state := t.TempDir()
			if err := agentconf.Write(state, agentconf.Config{Server: server.URL, Token: "node-doctor-secret"}); err != nil {
				t.Fatal(err)
			}
			args := []string{"--state", state}
			if subcommand == "doctor" {
				args = append(args, "--connectivity", "--json")
			}
			data, err := agentDiagnosticCLI(t, state, subcommand, args...)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("%s timeout exit: %v %s", subcommand, err, data)
			}
			if subcommand == "doctor" {
				var report proto.DoctorReport
				if json.Unmarshal(data, &report) != nil || proto.ConnectionValue(report, "connection.reason") != "timeout" {
					t.Fatalf("timeout report invalid: %s", data)
				}
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("timed out process left HTTP request alive")
			}
		})
	}
}
