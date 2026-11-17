package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

type fakeNodes struct {
	mu      sync.Mutex
	online  map[string]bool
	refuse  map[string]error
	calls   []string
	started map[string]proto.DebugStartParams
}

func (f *fakeNodes) Online(uuid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[uuid]
}

func (f *fakeNodes) CallNode(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, uuid+" "+method)
	if err := f.refuse[uuid]; err != nil {
		return nil, err
	}
	if p, ok := params.(proto.DebugStartParams); ok {
		f.started[uuid] = p
	}
	return json.RawMessage(`{}`), nil
}

type fakeScope struct{}

func (fakeScope) DeploymentNodes(ids []uint) ([]string, error) {
	if len(ids) == 1 && ids[0] == 7 {
		return []string{"n-run", "n-old"}, nil
	}
	return nil, nil
}
func (fakeScope) UnknownDeployment(ids []uint) (uint, bool) {
	for _, id := range ids {
		if id != 7 && id != 8 {
			return id, true
		}
	}
	return 0, false
}
func (fakeScope) UnknownNode(uuids []string) (string, bool) {
	for _, u := range uuids {
		if u == "ghost" {
			return u, true
		}
	}
	return "", false
}

func newManager(t *testing.T) (*Manager, *fakeNodes, *strings.Builder) {
	var log strings.Builder
	tee := debuglog.New(slog.NewTextHandler(io.Discard, nil))
	nodes := &fakeNodes{
		online:  map[string]bool{"n-run": true, "n-old": true, "n-pick": true},
		refuse:  map[string]error{"n-old": fmt.Errorf("rpc error: %w", proto.UnknownRPCMethod(proto.RPCDebugStart))},
		started: map[string]proto.DebugStartParams{},
	}
	return NewManager(t.TempDir(), tee, nodes, fakeScope{}, slog.New(slog.NewTextHandler(&log, nil))), nodes, &log
}

func productCode(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	return ""
}

func TestStartDebugRefusesOutOfRangeAndUnknownScope(t *testing.T) {
	m, _, _ := newManager(t)
	for _, req := range []DebugRequest{
		{Duration: 2 * time.Hour}, {Duration: 30 * time.Second}, {MaxBytes: 51 << 30}, {MaxBytes: 1},
	} {
		if _, err := m.StartDebug(context.Background(), req); productCode(err) != messages.ErrorDebugSessionInvalid {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if _, err := m.StartDebug(context.Background(), DebugRequest{DeploymentIDs: []uint{9}}); productCode(err) != messages.ErrorDiagnosticsScopeUnknown {
		t.Fatal(err)
	}
	if _, err := m.StartDebug(context.Background(), DebugRequest{NodeUUIDs: []string{"ghost"}}); productCode(err) != messages.ErrorDiagnosticsScopeUnknown {
		t.Fatal(err)
	}
	if _, ok := m.DebugStatus(); ok {
		t.Fatal("a refused request left a session")
	}
}

func TestDebugSessionLifecycle(t *testing.T) {
	m, nodes, log := newManager(t)
	nodes.online["n-off"] = false
	s, err := m.StartDebug(context.Background(), DebugRequest{DeploymentIDs: []uint{7}, NodeUUIDs: []string{"n-pick", "n-off"}, StartedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Open || s.MaxBytes != DefaultDebugBytes || s.Until.Sub(s.StartedAt) != DefaultDebugDuration {
		t.Fatalf("defaults: %+v", s)
	}
	states := map[string]string{}
	for _, n := range s.Nodes {
		states[n.UUID] = n.State + "/" + n.Reason
	}
	if states["n-run"] != "collecting/" || states["n-pick"] != "collecting/" || states["n-off"] != "skipped/offline" || states["n-old"] != "skipped/unsupported" {
		t.Fatalf("nodes = %v", states)
	}
	if p := nodes.started["n-run"]; p.SessionID != s.ID || p.MaxBytes != NodeDebugBytes || p.UntilMS != s.Until.UnixMilli() {
		t.Fatalf("debug.start params = %+v", p)
	}
	if _, err := m.StartDebug(context.Background(), DebugRequest{}); productCode(err) != messages.ErrorDebugSessionOpen {
		t.Fatalf("second start: %v", err)
	}

	// Only the named deployment's requests are recorded.
	if m.Begin(8, "r1") != nil {
		t.Fatal("recorded a deployment outside the session")
	}
	w := m.Begin(7, "r/2")
	if w == nil {
		t.Fatal("no recorder for the session's deployment")
	}
	_, _ = w.Write([]byte(`{"t":"request"}` + "\n"))
	_ = w.Close()
	dir := filepath.Join(m.DebugDir(), s.ID)
	if b, err := os.ReadFile(filepath.Join(dir, "requests", "7-r_2.jsonl")); err != nil || !strings.Contains(string(b), "request") {
		t.Fatalf("request file: %q %v", b, err)
	}

	out, ok := m.StopDebug()
	if !ok || out.Open || out.Ended != "stopped" || out.Requests != 1 || out.RequestBytes == 0 {
		t.Fatalf("stopped = %+v", out)
	}
	if strings.Count(strings.Join(nodes.calls, "\n"), proto.RPCDebugStop) != 2 {
		t.Fatalf("debug.stop goes to the collecting nodes only: %v", nodes.calls)
	}
	var manifest DebugSession
	b, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	if json.Unmarshal(b, &manifest) != nil || manifest.Ended != "stopped" || manifest.StartedBy != "admin" || len(manifest.Nodes) != 4 {
		t.Fatalf("manifest = %s", b)
	}
	if !strings.Contains(log.String(), `msg="debug session started"`) || !strings.Contains(log.String(), "by=admin") || !strings.Contains(log.String(), `msg="debug session ended"`) {
		t.Fatalf("the start and end are logged as the audit trail:\n%s", log.String())
	}
	if m.Begin(7, "r3") != nil {
		t.Fatal("recording after the session ended")
	}
	if _, ok := m.StopDebug(); ok {
		t.Fatal("stopped twice")
	}
	if last, ok := m.DebugStatus(); !ok || last.ID != s.ID || last.Open {
		t.Fatalf("last = %+v", last)
	}
}

func TestDebugSessionEndsAtItsDeadline(t *testing.T) {
	m, _, _ := newManager(t)
	s, err := m.StartDebug(context.Background(), DebugRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.current.timer.Reset(time.Millisecond)
	m.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := m.DebugStatus(); !st.Open {
			if st.ID != s.ID || st.Ended != "deadline" {
				t.Fatalf("ended = %+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the deadline did not end the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A request's exchange keeps 16 MiB; the session ends when its recorded
// bytes reach its size.
func TestDebugSessionBounds(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.StartDebug(context.Background(), DebugRequest{DeploymentIDs: []uint{7}, MaxBytes: 20 << 20}); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 1<<20)
	w := m.Begin(7, "big")
	for i := 0; i < 20; i++ {
		_, _ = w.Write(big)
	}
	_ = w.Close()
	st, _ := m.DebugStatus()
	if st.RequestBytes > RequestCaptureBytes+100 {
		t.Fatalf("one request kept %d bytes", st.RequestBytes)
	}
	w = m.Begin(7, "next")
	for i := 0; i < 8; i++ {
		_, _ = w.Write(big)
	}
	_ = w.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := m.DebugStatus(); !st.Open {
			if st.Ended != "size_limit" {
				t.Fatalf("ended = %+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the size did not end the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
