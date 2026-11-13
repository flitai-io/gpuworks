package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestNegotiate(t *testing.T) {
	cases := []struct {
		lo, hi int
		want   int
	}{
		{1, 1, 1},
		{1, 5, 1}, // server max is 1
		{2, 3, 0}, // agent min above server max
		{0, 0, 0}, // agent below server min
	}
	for _, c := range cases {
		got := negotiate(proto.Hello{ProtoMin: c.lo, ProtoMax: c.hi})
		if got != c.want {
			t.Errorf("negotiate(min=%d,max=%d) = %d, want %d", c.lo, c.hi, got, c.want)
		}
	}
}

func TestAgentRemoteFollowsTrustedForwardedFor(t *testing.T) {
	listed, _ := auth.NewProxyTrust("10.0.0.0/8, 192.0.2.10", nil)
	exposed, _ := auth.NewProxyTrust("", func() bool { return true })
	for _, tc := range []struct {
		name    string
		proxies *auth.ProxyTrust
		peer    string
		local   string
		xff     string
		want    string
	}{
		{"no trust ignores XFF", nil, "10.1.2.3:443", "", "203.0.113.9", "10.1.2.3:443"},
		{"listed CIDR", listed, "10.1.2.3:443", "", "203.0.113.9", "203.0.113.9"},
		{"listed address", listed, "192.0.2.10:1234", "", "203.0.113.9", "203.0.113.9"},
		{"listed hops are skipped", listed, "10.1.2.3:443", "", "203.0.113.9, 10.9.9.9", "203.0.113.9"},
		{"unlisted peer ignores XFF", listed, "198.51.100.7:443", "", "203.0.113.9", "198.51.100.7:443"},
		{"empty XFF keeps the peer", listed, "10.1.2.3:443", "", "", "10.1.2.3:443"},
		{"entry point connector on this host", exposed, "192.0.2.156:40000", "192.0.2.156:8816", "203.0.113.9", "203.0.113.9"},
		{"remote peer while exposed", exposed, "198.51.100.7:443", "192.0.2.156:8816", "203.0.113.9", "198.51.100.7:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hub{log: slog.Default()}
			h.SetProxyTrust(tc.proxies)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/agent/connect", nil)
			r.RemoteAddr = tc.peer
			if tc.local != "" {
				r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.TCPAddrFromAddrPort(netip.MustParseAddrPort(tc.local))))
			}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := h.agentRemote(r); got != tc.want {
				t.Fatalf("agentRemote = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadLine(t *testing.T) {
	r := bytes.NewReader([]byte("hello world\nsecond"))
	line, err := readLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "hello world" {
		t.Errorf("line = %q", line)
	}
	// Remaining bytes must be intact (no over-read past the newline).
	rest, _ := io.ReadAll(r)
	if string(rest) != "second" {
		t.Errorf("rest = %q, want %q", rest, "second")
	}
}

func TestReadStreamHeader(t *testing.T) {
	r := bytes.NewReader([]byte(`{"kind":"ctrl"}` + "\n" + `{"t":"hello"}`))
	hdr, err := readStreamHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Kind != proto.StreamCtrl {
		t.Errorf("kind = %q", hdr.Kind)
	}
}

func TestAgentUpgradeFailureStatusIsLoggedAsAsyncEvent(t *testing.T) {
	st, err := store.Open("file:hub_upgrade_status_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := New(st, log)
	n, err := st.RegisterNode("upgrade-status-node", store.HashToken("upgrade-token"), "upgrade-status-node")
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(h, nil, nil)
	s.NodeUUID = n.UUID
	raw, err := json.Marshal(proto.AgentUpgradeStatus{State: "failed", Error: "smoke test failed"})
	if err != nil {
		t.Fatal(err)
	}
	s.handle(proto.Envelope{T: proto.TAgentUpgradeStatus, D: raw})
	if !bytes.Contains(logs.Bytes(), []byte("agent self-upgrade failed")) || !bytes.Contains(logs.Bytes(), []byte("smoke test failed")) {
		t.Fatalf("upgrade failure event was not logged: %s", logs.String())
	}
}

func TestAgentBenchmarkStatusPersistsProcessCurve(t *testing.T) {
	st, err := store.Open("file:hub_benchmark_curve_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("benchmark-curve-node", store.HashToken("benchmark-token"), "benchmark-curve-node")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "agent curve", LoadOrigin: node.UUID, State: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	sampleRaw, err := json.Marshal(metrics.ProcessSample{
		OutputTput: 11, TotalTput: 22, RequestTput: 3,
		TTFTP50: 4, TPOTP50: 5, LatencyP99: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	statusRaw, err := json.Marshal(proto.BenchmarkStatus{
		ID: run.ID, State: "running", PointIndex: 1, PointTotal: 1,
		CurrentPoint: &proto.BenchmarkCurrentPoint{Seq: 1, Concurrency: 7}, Sample: sampleRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session := newSession(h, nil, nil)
	session.NodeUUID, session.nodeID = node.UUID, node.ID
	h.sessions[node.UUID] = session
	session.handle(proto.Envelope{T: proto.TStatusBenchmark, D: statusRaw})
	session.drainReports()

	persistedRun, err := st.GetBenchmarkRun(run.ID)
	if err != nil || persistedRun.State != "running" || persistedRun.StartedAt == nil {
		t.Fatalf("run = %+v, err=%v", persistedRun, err)
	}
	point, err := st.GetBenchmarkPoint(run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var curve []store.BenchmarkCurveSample
	if err := json.Unmarshal([]byte(point.Curve), &curve); err != nil {
		t.Fatal(err)
	}
	if len(curve) != 1 || curve[0].OutputTput != 11 || curve[0].TotalTput != 22 || curve[0].RequestTput != 3 || curve[0].TTFTP50 != 4 || curve[0].TPOTP50 != 5 || curve[0].LatencyP99 != 6 {
		t.Fatalf("curve = %+v", curve)
	}
	if delta := time.Since(time.UnixMilli(curve[0].T)); delta < 0 || delta > time.Minute {
		t.Fatalf("curve timestamp is not server receipt time: %v", time.UnixMilli(curve[0].T))
	}
}

func TestAgentBenchmarkStatusFromWrongNodeCannotWriteCurve(t *testing.T) {
	st, err := store.Open("file:hub_benchmark_curve_wrong_node?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	owner, err := st.RegisterNode("benchmark-owner", store.HashToken("owner-token"), "benchmark-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.RegisterNode("benchmark-other", store.HashToken("other-token"), "benchmark-other")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "owned run", LoadOrigin: owner.UUID, State: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	sampleRaw, _ := json.Marshal(metrics.ProcessSample{OutputTput: 99})
	statusRaw, _ := json.Marshal(proto.BenchmarkStatus{ID: run.ID, State: "running", PointIndex: 1, Sample: sampleRaw})
	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	session := newSession(h, nil, nil)
	session.NodeUUID, session.nodeID = other.UUID, other.ID
	h.sessions[other.UUID] = session // current, so only the store's owner check refuses it
	session.handle(proto.Envelope{T: proto.TStatusBenchmark, D: statusRaw})
	session.drainReports()
	if _, err := st.GetBenchmarkPoint(run.ID, 1); err != store.ErrNotFound {
		t.Fatalf("wrong node wrote a curve point: err=%v", err)
	}
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil || persisted.State != "pending" {
		t.Fatalf("wrong node changed run: run=%+v err=%v", persisted, err)
	}
}
