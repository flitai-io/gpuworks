// Package diagnostics gathers what a remote supporter needs from an
// installation they cannot log in to: a debug session (the server's and the
// chosen nodes' debug logs, and the chosen deployments' raw requests, for a
// bounded time and size) and a diagnostics collection (logs, debug sessions
// and the related records, packed for download).
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Debug session bounds.
const (
	DefaultDebugDuration = 10 * time.Minute
	MaxDebugDuration     = time.Hour
	DefaultDebugBytes    = int64(10 << 30)
	MaxDebugBytes        = int64(50 << 30)
	// NodeDebugBytes caps one node's session file; the agent enforces it too.
	NodeDebugBytes = int64(200 << 20)
	// RequestCaptureBytes caps one request's recorded exchange.
	RequestCaptureBytes = int64(16 << 20)
	// SessionKeep is how long a session's files stay for collection.
	SessionKeep = 7 * 24 * time.Hour
)

// Nodes is what a debug session needs of the hub.
type Nodes interface {
	CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
	Online(uuid string) bool
}

// Scope resolves a session's scope against the store: the nodes running the
// deployments, and whether every named deployment and node exists.
type Scope interface {
	DeploymentNodes(ids []uint) ([]string, error)
	UnknownDeployment(ids []uint) (uint, bool)
	UnknownNode(uuids []string) (string, bool)
}

// DebugRequest starts a session. A zero Duration or MaxBytes takes the
// default.
type DebugRequest struct {
	Duration      time.Duration
	MaxBytes      int64
	DeploymentIDs []uint
	NodeUUIDs     []string
	StartedBy     string
}

// DebugSession is a session as it stands; it is also the session's
// manifest, written next to its files (session.json).
type DebugSession struct {
	ID            string      `json:"id"`
	StartedBy     string      `json:"started_by"`
	StartedAt     time.Time   `json:"started_at"`
	Until         time.Time   `json:"until"`
	MaxBytes      int64       `json:"max_bytes"`
	DeploymentIDs []uint      `json:"deployment_ids"`
	NodeUUIDs     []string    `json:"node_uuids"`
	Nodes         []NodeDebug `json:"nodes"`
	Open          bool        `json:"open"`
	// Ended is why a closed session ended: deadline, size_limit or stopped.
	Ended          string     `json:"ended,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	ServerLogBytes int64      `json:"server_log_bytes"`
	RequestBytes   int64      `json:"request_bytes"`
	Requests       int64      `json:"requests"`
}

// NodeDebug is one node's part in a session.
type NodeDebug struct {
	UUID string `json:"uuid"`
	// State is collecting, skipped (offline, or an agent without debug
	// sessions) or failed.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// Manager runs the installation's debug sessions, one at a time.
type Manager struct {
	dir   string
	tee   *debuglog.Tee
	nodes Nodes
	scope Scope
	log   *slog.Logger
	now   func() time.Time

	sources Sources
	// bundleDir, when set, holds the collections instead of <dir>/bundles.
	bundleDir string

	mu      sync.Mutex
	current *runningSession
	last    *DebugSession
	// bundleRunning is the collection being built, "" when none.
	bundleRunning string
	// unsaved are collections whose manifest could not be written (a full
	// disk): kept here so the list still shows them, and why they failed,
	// until a later write of the manifest succeeds.
	unsaved map[string]Bundle
}

type runningSession struct {
	session      DebugSession
	dir          string
	deployments  map[uint]bool
	requestBytes atomic.Int64
	requests     atomic.Int64
	closed       atomic.Bool
	timer        *time.Timer
}

// NewManager keeps sessions under dir (<data>/diagnostics).
func NewManager(dir string, tee *debuglog.Tee, nodes Nodes, scope Scope, log *slog.Logger) *Manager {
	return &Manager{dir: dir, tee: tee, nodes: nodes, scope: scope, log: log, now: time.Now}
}

// DebugDir is where the sessions' files live.
func (m *Manager) DebugDir() string { return filepath.Join(m.dir, "debug") }

// StartDebug opens a session.
func (m *Manager) StartDebug(ctx context.Context, req DebugRequest) (DebugSession, error) {
	if req.Duration == 0 {
		req.Duration = DefaultDebugDuration
	}
	if req.MaxBytes == 0 {
		req.MaxBytes = DefaultDebugBytes
	}
	if req.Duration < time.Minute || req.Duration > MaxDebugDuration {
		return DebugSession{}, messages.NewProductError(messages.ErrorDebugSessionInvalid, "duration must be 1 minute to 1 hour",
			map[string]any{"field": "duration", "max": int(MaxDebugDuration.Seconds())})
	}
	if req.MaxBytes < 1<<20 || req.MaxBytes > MaxDebugBytes {
		return DebugSession{}, messages.NewProductError(messages.ErrorDebugSessionInvalid, "max_bytes must be 1 MiB to 50 GiB",
			map[string]any{"field": "max_bytes", "max": MaxDebugBytes})
	}
	if id, unknown := m.scope.UnknownDeployment(req.DeploymentIDs); unknown {
		return DebugSession{}, messages.NewProductError(messages.ErrorDiagnosticsScopeUnknown, "no such deployment", map[string]any{"id": fmt.Sprint(id)})
	}
	if uuid, unknown := m.scope.UnknownNode(req.NodeUUIDs); unknown {
		return DebugSession{}, messages.NewProductError(messages.ErrorDiagnosticsScopeUnknown, "no such node", map[string]any{"id": uuid})
	}
	nodes := slices.Clone(req.NodeUUIDs)
	if len(req.DeploymentIDs) > 0 {
		running, err := m.scope.DeploymentNodes(req.DeploymentIDs)
		if err != nil {
			return DebugSession{}, err
		}
		nodes = append(nodes, running...)
	}
	slices.Sort(nodes)
	nodes = slices.Compact(nodes)

	m.mu.Lock()
	if m.current != nil {
		id := m.current.session.ID
		m.mu.Unlock()
		return DebugSession{}, messages.NewProductError(messages.ErrorDebugSessionOpen, "a debug session is already open", map[string]any{"session": id})
	}
	now := m.now()
	id := now.UTC().Format("20060102-150405") + "-" + auth.NewUUID()[:8]
	rs := &runningSession{
		session: DebugSession{ID: id, StartedBy: req.StartedBy, StartedAt: now, Until: now.Add(req.Duration), MaxBytes: req.MaxBytes,
			DeploymentIDs: slices.Clone(req.DeploymentIDs), NodeUUIDs: nodes, Open: true},
		dir:         filepath.Join(m.DebugDir(), id),
		deployments: map[uint]bool{},
	}
	for _, dep := range req.DeploymentIDs {
		rs.deployments[dep] = true
	}
	debuglog.Sweep(m.DebugDir(), SessionKeep, now)
	if err := os.MkdirAll(filepath.Join(rs.dir, "requests"), 0o700); err != nil {
		m.mu.Unlock()
		return DebugSession{}, err
	}
	if err := m.tee.Start(id, filepath.Join(rs.dir, "server.log"), rs.session.Until, req.MaxBytes); err != nil {
		m.mu.Unlock()
		return DebugSession{}, err
	}
	m.current = rs
	m.mu.Unlock()

	rs.session.Nodes = m.startNodes(ctx, id, rs.session.Until, nodes)
	m.log.Warn("debug session started", "session", id, "by", req.StartedBy, "until", rs.session.Until.UTC().Format(time.RFC3339),
		"max_bytes", req.MaxBytes, "deployments", fmt.Sprint(req.DeploymentIDs), "nodes", strings.Join(nodes, ","))
	go m.watchBudget(rs)
	m.mu.Lock()
	rs.timer = time.AfterFunc(req.Duration, func() { m.end(rs, "deadline") })
	out := m.snapshotLocked(rs)
	m.mu.Unlock()
	m.writeManifest(rs.dir, out)
	return out, nil
}

// startNodes asks each node to open its side, a few at a time.
func (m *Manager) startNodes(ctx context.Context, id string, until time.Time, uuids []string) []NodeDebug {
	out := make([]NodeDebug, len(uuids))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, uuid := range uuids {
		out[i] = NodeDebug{UUID: uuid}
		if !m.nodes.Online(uuid) {
			out[i].State, out[i].Reason = "skipped", "offline"
			continue
		}
		wg.Add(1)
		go func(i int, uuid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, err := m.nodes.CallNode(callCtx, uuid, proto.RPCDebugStart, proto.DebugStartParams{SessionID: id, UntilMS: until.UnixMilli(), MaxBytes: NodeDebugBytes})
			switch {
			case err == nil:
				out[i].State = "collecting"
			case errors.Is(err, proto.ErrRPCUnknownMethod):
				out[i].State, out[i].Reason = "skipped", "unsupported"
			default:
				out[i].State, out[i].Reason = "failed", err.Error()
			}
		}(i, uuid)
	}
	wg.Wait()
	return out
}

// StopDebug ends the open session; ok is false when none is open.
func (m *Manager) StopDebug() (DebugSession, bool) {
	m.mu.Lock()
	rs := m.current
	m.mu.Unlock()
	if rs == nil {
		return DebugSession{}, false
	}
	m.end(rs, "stopped")
	out, _ := m.DebugStatus()
	return out, true
}

// DebugStatus is the open session, or the last one.
func (m *Manager) DebugStatus() (DebugSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil {
		return m.snapshotLocked(m.current), true
	}
	if m.last != nil {
		return *m.last, true
	}
	return DebugSession{}, false
}

func (m *Manager) snapshotLocked(rs *runningSession) DebugSession {
	out := rs.session
	out.Nodes = slices.Clone(rs.session.Nodes)
	out.RequestBytes, out.Requests = rs.requestBytes.Load(), rs.requests.Load()
	if st, ok := m.tee.Status(); ok && st.ID == rs.session.ID {
		out.ServerLogBytes = st.Written
	}
	return out
}

// end closes rs once, whatever ended it.
func (m *Manager) end(rs *runningSession, reason string) {
	if !rs.closed.CompareAndSwap(false, true) {
		return
	}
	if rs.timer != nil {
		rs.timer.Stop()
	}
	m.tee.Stop(rs.session.ID)
	var wg sync.WaitGroup
	for _, node := range rs.session.Nodes {
		if node.State != "collecting" {
			continue
		}
		wg.Add(1)
		go func(uuid string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// A node that cannot be reached ends its side at the deadline.
			_, _ = m.nodes.CallNode(ctx, uuid, proto.RPCDebugStop, proto.DebugStopParams{SessionID: rs.session.ID})
		}(node.UUID)
	}
	wg.Wait()
	m.mu.Lock()
	out := m.snapshotLocked(rs)
	ended := m.now()
	out.Open, out.Ended, out.EndedAt = false, reason, &ended
	if m.current == rs {
		m.current = nil
	}
	m.last = &out
	m.mu.Unlock()
	m.writeManifest(rs.dir, out)
	m.log.Warn("debug session ended", "session", out.ID, "reason", reason, "server_log_bytes", out.ServerLogBytes,
		"request_bytes", out.RequestBytes, "requests", out.Requests)
}

func (m *Manager) writeManifest(dir string, s DebugSession) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, "session.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "session.json"))
	}
}

// overBudget ends the session once its server log and recorded requests
// together reach its size.
func (m *Manager) overBudget(rs *runningSession) bool {
	used := rs.requestBytes.Load()
	if st, ok := m.tee.Status(); ok && st.ID == rs.session.ID {
		used += st.Written
		if !st.Open && st.Ended == "size_limit" {
			return true
		}
	}
	return used >= rs.session.MaxBytes
}

// watchBudget ends rs when its server log alone fills it: request writes
// check the budget themselves, but a session may record no requests.
func (m *Manager) watchBudget(rs *runningSession) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if rs.closed.Load() {
			return
		}
		if m.overBudget(rs) {
			m.end(rs, "size_limit")
			return
		}
	}
}

// Begin is the gateway's request recorder (proxy.RequestRecorder): a
// session records the requests of the deployments it names.
func (m *Manager) Begin(deploymentID uint, requestID string) io.WriteCloser {
	m.mu.Lock()
	rs := m.current
	m.mu.Unlock()
	if rs == nil || rs.closed.Load() || !rs.deployments[deploymentID] {
		return nil
	}
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, requestID)
	f, err := os.OpenFile(filepath.Join(rs.dir, "requests", fmt.Sprintf("%d-%s.jsonl", deploymentID, name)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	rs.requests.Add(1)
	return &requestFile{m: m, rs: rs, f: f, left: RequestCaptureBytes}
}

// requestFile is one recorded exchange: bounded by RequestCaptureBytes and by
// the session's total, which it charges.
type requestFile struct {
	m         *Manager
	rs        *runningSession
	f         *os.File
	mu        sync.Mutex
	left      int64
	truncated bool
}

var errSessionOver = errors.New("debug session over")

func (w *requestFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rs.closed.Load() {
		return 0, errSessionOver
	}
	if int64(len(p)) > w.left {
		if !w.truncated {
			w.truncated = true
			marker := []byte(`{"t":"truncated","reason":"request_limit"}` + "\n")
			n, _ := w.f.Write(marker)
			w.rs.requestBytes.Add(int64(n))
		}
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.left -= int64(n)
	w.rs.requestBytes.Add(int64(n))
	if w.m.overBudget(w.rs) {
		go w.m.end(w.rs, "size_limit")
	}
	return n, err
}

func (w *requestFile) Close() error { return w.f.Close() }
