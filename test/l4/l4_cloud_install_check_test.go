//go:build cloudgpu

package l4

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Offline: go test -tags=cloudgpu -run 'TestJudgeProviderInstallCheck|TestWaitQueuedDialNodeOnline' ./test/l4/

func TestJudgeProviderInstallCheck(t *testing.T) {
	const queued = "queued for reconnection: read server_dial challenge: read tcp 100.96.0.20:41800->213.173.108.41:22256: i/o timeout"
	cases := []struct {
		name       string
		want       string
		check      provision.CheckResult
		accepted   bool
		queuedDial bool
	}{
		{"server_dial pass", "server_dial", provision.CheckResult{ID: "server_dial", Status: "pass", Code: messages.CheckServerDialConnected}, true, false},
		{"server_dial queued for reconnection", "server_dial", provision.CheckResult{ID: "server_dial", Status: "warn", Code: messages.CheckServerDialNotConnected, Evidence: queued}, true, true},
		{"server_dial no address", "server_dial", provision.CheckResult{ID: "server_dial", Status: "skip", Code: messages.CheckServerDialNoAddress}, false, false},
		{"server_dial not configured", "server_dial", provision.CheckResult{ID: "server_dial", Status: "skip", Code: messages.CheckServerDialUnconfigured}, false, false},
		{"server_dial warn for another code", "server_dial", provision.CheckResult{ID: "server_dial", Status: "warn", Code: messages.CheckAgentSessionWaiting}, false, false},
		{"server_dial fail even with the queued code", "server_dial", provision.CheckResult{ID: "server_dial", Status: "fail", Code: messages.CheckServerDialNotConnected, Evidence: queued}, false, false},
		{"queued code on the wrong check", "agent_dial", provision.CheckResult{ID: "agent_dial", Status: "warn", Code: messages.CheckServerDialNotConnected}, false, false},
		{"queued code under another id", "server_dial", provision.CheckResult{ID: "agent_dial", Status: "warn", Code: messages.CheckServerDialNotConnected}, false, false},
		{"agent_dial pass", "agent_dial", provision.CheckResult{ID: "agent_dial", Status: "pass"}, true, false},
		{"agent_dial waiting for first session", "agent_dial", provision.CheckResult{ID: "agent_dial", Status: "warn", Code: messages.CheckAgentSessionWaiting}, true, false},
		{"agent_dial session not observable", "agent_dial", provision.CheckResult{ID: "agent_dial", Status: "skip", Code: messages.CheckAgentSessionUnobserved}, false, false},
		{"precheck warn", "precheck", provision.CheckResult{ID: "precheck", Status: "warn", Code: messages.CheckPrecheckAttention}, true, false},
		{"precheck fail", "precheck", provision.CheckResult{ID: "precheck", Status: "fail"}, false, false},
		{"precheck skip is not a pass", "precheck", provision.CheckResult{ID: "precheck", Status: "skip"}, false, false},
		{"install agent warn", "install_agent", provision.CheckResult{ID: "install_agent", Status: "warn"}, false, false},
		{"wrong check at the position", "install_agent", provision.CheckResult{ID: "precheck", Status: "pass"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeProviderInstallCheck(tc.want, tc.check)
			if got.Accepted != tc.accepted || got.QueuedDial != tc.queuedDial {
				t.Fatalf("judgeProviderInstallCheck(%q, %+v) = accepted %v queued %v, want accepted %v queued %v", tc.want, tc.check, got.Accepted, got.QueuedDial, tc.accepted, tc.queuedDial)
			}
		})
	}
}

func TestWaitQueuedDialNodeOnline(t *testing.T) {
	const queued = "queued for reconnection: read server_dial challenge: i/o timeout"
	t.Run("comes online after some redials", func(t *testing.T) {
		polls := 0
		state := func() (string, error) {
			polls++
			if polls < 3 {
				return store.NodeStateOffline, nil
			}
			return store.NodeStateOnline, nil
		}
		probe := func() string { t.Fatal("probe must not run when the node comes online"); return "" }
		if err := waitQueuedDialNodeOnline(context.Background(), time.Second, time.Millisecond, state, probe, queued); err != nil {
			t.Fatal(err)
		}
		if polls != 3 {
			t.Fatalf("polls = %d, want 3", polls)
		}
	})
	t.Run("never online reports the dial error, state and probe", func(t *testing.T) {
		state := func() (string, error) { return store.NodeStateOffline, nil }
		probe := func() string { return "dial 1.2.3.4:5: i/o timeout" }
		err := waitQueuedDialNodeOnline(context.Background(), 30*time.Millisecond, time.Millisecond, state, probe, queued)
		if err == nil {
			t.Fatal("a node that never comes online passed")
		}
		for _, want := range []string{queued, `node state "offline"`, "dial 1.2.3.4:5: i/o timeout", "within 30ms"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
	})
	t.Run("unreadable node state is reported", func(t *testing.T) {
		state := func() (string, error) { return "", errors.New("record not found") }
		err := waitQueuedDialNodeOnline(context.Background(), 20*time.Millisecond, time.Millisecond, state, func() string { return "p" }, queued)
		if err == nil || !strings.Contains(err.Error(), "record not found") {
			t.Fatalf("err = %v, want the state read error", err)
		}
	})
	t.Run("canceled context stops the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		state := func() (string, error) { return store.NodeStateOffline, nil }
		err := waitQueuedDialNodeOnline(ctx, time.Minute, time.Millisecond, state, func() string { return "p" }, queued)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), queued) {
			t.Fatalf("err = %v, want context.Canceled carrying the install dial error", err)
		}
	})
}
