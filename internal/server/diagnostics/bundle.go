package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Collection bounds.
const (
	DefaultBundleWindow = time.Hour
	MaxBundleWindow     = 7 * 24 * time.Hour
	// BundleKeep is how long a finished collection stays for download.
	BundleKeep = 7 * 24 * time.Hour
	// MaxBundleInstances bounds the instances whose logs one collection
	// reads; MaxBundleNodes the nodes it asks.
	MaxBundleInstances = 64
	MaxBundleNodes     = 64
	// InstanceLogBytes and ServerLogBytes bound one instance's and the
	// server's log in a collection.
	InstanceLogBytes = int64(64 << 20)
	ServerLogBytes   = int64(256 << 20)
)

// Sources are the installation's records a collection reads (app wires them).
type Sources interface {
	// OpenNodeFiles opens a node's part: the agent log and the named debug
	// session files, as a tar (diagnostics.read).
	OpenNodeFiles(ctx context.Context, uuid string, sessions []string) (io.ReadCloser, error)
	// InstanceLogs writes an instance's stored log lines in the window,
	// oldest first, up to max bytes; truncated says it stopped there.
	InstanceLogs(ctx context.Context, uuid string, from, to time.Time, w io.Writer, max int64) (truncated bool, err error)
	// InstanceNodes are the nodes the instances are on.
	InstanceNodes(uuids []string) ([]string, error)
	// DeploymentInstances are the deployments' instances.
	DeploymentInstances(ids []uint) ([]string, error)
	// Snapshot is the related records, by document name.
	Snapshot(ctx context.Context, scope SnapshotScope) (map[string]any, error)
	// ServerLogFiles are the server's log file and its rotated generations.
	ServerLogFiles() []string
	// ServerDirs are the directories whose filesystems server/disk.json
	// reports.
	ServerDirs() []ServerDir
	// Version is the server's version.
	Version() string
}

// SnapshotScope is what a collection's records cover.
type SnapshotScope struct {
	From, To      time.Time
	DeploymentIDs []uint
	InstanceUUIDs []string
	NodeUUIDs     []string
}

// BundleRequest starts a collection. A zero window is the last hour.
type BundleRequest struct {
	From, To      time.Time
	DeploymentIDs []uint
	InstanceUUIDs []string
	NodeUUIDs     []string
	RequestedBy   string
	// Doctor runs each collected node's diagnosis (up to two minutes).
	Doctor bool
}

// Bundle is a collection as it stands; it is also the collection's
// manifest.json.
type Bundle struct {
	ID          string       `json:"id"`
	RequestedBy string       `json:"requested_by"`
	CreatedAt   time.Time    `json:"created_at"`
	FinishedAt  *time.Time   `json:"finished_at,omitempty"`
	From        time.Time    `json:"from"`
	To          time.Time    `json:"to"`
	Deployments []uint       `json:"deployment_ids"`
	Instances   []string     `json:"instance_uuids"`
	Nodes       []string     `json:"node_uuids"`
	Doctor      bool         `json:"doctor"`
	Version     string       `json:"server_version"`
	State       string       `json:"state"` // running, ready, failed
	Error       string       `json:"error,omitempty"`
	SizeBytes   int64        `json:"size_bytes"`
	Items       []BundleItem `json:"items"`
}

// BundleItem is one part of a collection, reported as a check (ADR 0067):
// pass (collected whole), warn (collected up to its size bound), skip (not
// read: the node is offline or its agent predates the read) or fail. Status
// is one of proto.Check*; Code (package messages) says why for every status
// but pass; Evidence is the error text of the read that failed, kept apart
// from the message. The manifest on disk and in the archive is this shape;
// manifests written before it are read by UnmarshalJSON.
type BundleItem struct {
	// Path is where the part is (or would be) in the archive.
	Path     string `json:"path"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Bytes    int64  `json:"bytes"`
}

// BundleDir is where collections are kept.
func (m *Manager) BundleDir() string {
	if m.bundleDir != "" {
		return m.bundleDir
	}
	return filepath.Join(m.dir, "bundles")
}

// SetBundleDir puts collections somewhere other than the data directory:
// the offline command builds next to its output, so it still works when
// the data directory's disk is the one that is full.
func (m *Manager) SetBundleDir(dir string) { m.bundleDir = dir }

// SetSources wires what collections read; until then they are unavailable.
func (m *Manager) SetSources(s Sources) { m.sources = s }

// StartBundle begins a collection in the background; one runs at a time.
func (m *Manager) StartBundle(ctx context.Context, req BundleRequest) (Bundle, error) {
	b, err := m.beginBundle(req)
	if err != nil {
		return Bundle{}, err
	}
	go func() {
		built, err := m.buildBundle(context.WithoutCancel(ctx), b)
		m.finishBundle(built, err)
	}()
	return b, nil
}

// CollectNow builds a collection in the caller's goroutine and answers it
// with its archive's path: the command line's way, for a server that is not
// running.
func (m *Manager) CollectNow(ctx context.Context, req BundleRequest) (Bundle, string, error) {
	b, err := m.beginBundle(req)
	if err != nil {
		return Bundle{}, "", err
	}
	built, err := m.buildBundle(ctx, b)
	m.finishBundle(built, err)
	done, _ := m.Bundle(b.ID)
	if err != nil {
		return done, "", err
	}
	return done, m.bundleArchive(b.ID), nil
}

// beginBundle checks a request and records the collection as running.
func (m *Manager) beginBundle(req BundleRequest) (Bundle, error) {
	if m.sources == nil {
		return Bundle{}, fmt.Errorf("diagnostics collection is not available")
	}
	now := m.now()
	if req.To.IsZero() {
		req.To = now
	}
	if req.From.IsZero() {
		req.From = req.To.Add(-DefaultBundleWindow)
	}
	if !req.From.Before(req.To) || req.To.Sub(req.From) > MaxBundleWindow {
		return Bundle{}, messages.NewProductError(messages.ErrorDiagnosticsBundleInvalid, "the window must end after it starts and span at most 7 days",
			map[string]any{"field": "window", "max": int(MaxBundleWindow.Seconds())})
	}
	if id, unknown := m.scope.UnknownDeployment(req.DeploymentIDs); unknown {
		return Bundle{}, messages.NewProductError(messages.ErrorDiagnosticsScopeUnknown, "no such deployment", map[string]any{"id": fmt.Sprint(id)})
	}
	if uuid, unknown := m.scope.UnknownNode(req.NodeUUIDs); unknown {
		return Bundle{}, messages.NewProductError(messages.ErrorDiagnosticsScopeUnknown, "no such node", map[string]any{"id": uuid})
	}
	instances := append([]string(nil), req.InstanceUUIDs...)
	if len(req.DeploymentIDs) > 0 {
		more, err := m.sources.DeploymentInstances(req.DeploymentIDs)
		if err != nil {
			return Bundle{}, err
		}
		instances = append(instances, more...)
	}
	instances = sortedUnique(instances)
	nodes := sortedUnique(req.NodeUUIDs)
	if len(nodes) == 0 {
		// No node named: the nodes the chosen instances are on.
		running, err := m.sources.InstanceNodes(instances)
		if err != nil {
			return Bundle{}, err
		}
		nodes = sortedUnique(running)
	}
	if len(instances) > MaxBundleInstances || len(nodes) > MaxBundleNodes {
		return Bundle{}, messages.NewProductError(messages.ErrorDiagnosticsBundleInvalid, "a collection reads at most 64 instances and 64 nodes",
			map[string]any{"field": "scope", "max": MaxBundleNodes})
	}

	m.mu.Lock()
	if m.bundleRunning != "" {
		id := m.bundleRunning
		m.mu.Unlock()
		return Bundle{}, messages.NewProductError(messages.ErrorDiagnosticsBundleRunning, "a diagnostics collection is already running", map[string]any{"bundle": id})
	}
	b := Bundle{ID: now.UTC().Format("20060102-150405") + "-" + auth.NewUUID()[:8], RequestedBy: req.RequestedBy, CreatedAt: now,
		From: req.From, To: req.To, Deployments: append([]uint{}, req.DeploymentIDs...), Instances: instances, Nodes: nodes,
		Doctor: req.Doctor, Version: m.sources.Version(), State: "running", Items: []BundleItem{}}
	m.bundleRunning = b.ID
	m.mu.Unlock()

	if err := os.MkdirAll(m.BundleDir(), 0o700); err != nil {
		m.finishBundle(b, err)
		return Bundle{}, err
	}
	sweepBundles(m.BundleDir(), BundleKeep, now)
	m.writeBundleManifest(b)
	m.log.Warn("diagnostics collection started", "bundle", b.ID, "by", req.RequestedBy, "from", req.From.UTC().Format(time.RFC3339),
		"to", req.To.UTC().Format(time.RFC3339), "deployments", fmt.Sprint(req.DeploymentIDs), "instances", len(instances), "nodes", len(nodes))
	return b, nil
}

func (m *Manager) finishBundle(b Bundle, err error) {
	finished := m.now()
	b.FinishedAt = &finished
	b.State = "ready"
	if err != nil {
		b.State, b.Error = "failed", err.Error()
		_ = os.Remove(m.bundleArchive(b.ID))
	} else if info, statErr := os.Stat(m.bundleArchive(b.ID)); statErr == nil {
		b.SizeBytes = info.Size()
	}
	m.writeBundleManifest(b)
	m.mu.Lock()
	if m.bundleRunning == b.ID {
		m.bundleRunning = ""
	}
	m.mu.Unlock()
	m.log.Warn("diagnostics collection finished", "bundle", b.ID, "state", b.State, "size_bytes", b.SizeBytes, "err", b.Error)
}

func (m *Manager) bundleArchive(id string) string {
	return filepath.Join(m.BundleDir(), id+".tar.gz")
}

func (m *Manager) bundleManifest(id string) string {
	return filepath.Join(m.BundleDir(), id+".json")
}

func (m *Manager) writeBundleManifest(b Bundle) {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return
	}
	tmp := m.bundleManifest(b.ID) + ".tmp"
	err = os.WriteFile(tmp, raw, 0o600)
	if err == nil {
		err = os.Rename(tmp, m.bundleManifest(b.ID))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.unsaved, b.ID)
		return
	}
	_ = os.Remove(tmp)
	if m.unsaved == nil {
		m.unsaved = map[string]Bundle{}
	}
	m.unsaved[b.ID] = b
	for len(m.unsaved) > maxUnsavedBundles {
		oldest := ""
		for id, kept := range m.unsaved {
			if oldest == "" || kept.CreatedAt.Before(m.unsaved[oldest].CreatedAt) {
				oldest = id
			}
		}
		delete(m.unsaved, oldest)
	}
}

// maxUnsavedBundles bounds the collections kept in memory without a manifest.
const maxUnsavedBundles = 16

// Bundles are the kept collections, newest first.
func (m *Manager) Bundles() []Bundle {
	out := []Bundle{}
	listed := map[string]bool{}
	entries, _ := os.ReadDir(m.BundleDir())
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if b, ok := m.Bundle(id); ok {
			out = append(out, b)
			listed[id] = true
		}
	}
	m.mu.Lock()
	for id, b := range m.unsaved {
		if !listed[id] {
			out = append(out, b)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Bundle reads one collection's manifest.
func (m *Manager) Bundle(id string) (Bundle, bool) {
	if !validID(id) {
		return Bundle{}, false
	}
	m.mu.Lock()
	unsaved, isUnsaved := m.unsaved[id]
	m.mu.Unlock()
	if isUnsaved {
		// Newer than any manifest on disk: that write is the one that failed.
		return unsaved, true
	}
	raw, err := os.ReadFile(m.bundleManifest(id))
	if err != nil {
		return Bundle{}, false
	}
	var b Bundle
	if json.Unmarshal(raw, &b) != nil {
		return Bundle{}, false
	}
	return b, true
}

// OpenBundle opens a ready collection's archive.
func (m *Manager) OpenBundle(id string) (*os.File, Bundle, error) {
	b, ok := m.Bundle(id)
	if !ok || b.State != "ready" {
		return nil, b, os.ErrNotExist
	}
	f, err := os.Open(m.bundleArchive(id))
	return f, b, err
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// sweepBundles removes collections older than keep.
func sweepBundles(dir string, keep time.Duration, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) > keep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
