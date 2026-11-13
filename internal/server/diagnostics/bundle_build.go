package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// agentLogBytes bounds a node's agent log in a collection (agent.log and its
// previous generation are 10 MiB each).
const agentLogBytes = int64(24 << 20)

// doctorTimeout is how long a node's diagnosis may take (the server's own
// wait for one, troubleshooting: agent caps it at 100 s).
const doctorTimeout = 2*time.Minute + 10*time.Second

// buildBundle writes the collection's archive. Parts are staged in a work
// directory (a tar entry needs its size up front), then packed; the archive
// appears under its name only when complete.
func (m *Manager) buildBundle(ctx context.Context, b Bundle) (Bundle, error) {
	work := filepath.Join(m.BundleDir(), b.ID+".work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return b, err
	}
	defer os.RemoveAll(work)
	var mu sync.Mutex
	var entries []stagedEntry
	add := func(item BundleItem, entry *stagedEntry) {
		mu.Lock()
		defer mu.Unlock()
		b.Items = append(b.Items, item)
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	stage := func(name string, write func(io.Writer) (int64, bool, error)) {
		local := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
			add(BundleItem{Name: name, State: "failed", Reason: err.Error()}, nil)
			return
		}
		f, err := os.Create(local)
		if err != nil {
			add(BundleItem{Name: name, State: "failed", Reason: err.Error()}, nil)
			return
		}
		n, truncated, err := write(f)
		_ = f.Close()
		if err != nil {
			add(BundleItem{Name: name, State: "failed", Reason: err.Error(), Bytes: n}, &stagedEntry{name: name, path: local})
			return
		}
		add(BundleItem{Name: name, State: "collected", Bytes: n, Truncated: truncated}, &stagedEntry{name: name, path: local})
	}

	// The server's disks first: when one is full, nothing later may fit.
	stage("server/disk.json", func(w io.Writer) (int64, bool, error) {
		return writeServerDisk(w, m.sources.ServerDirs())
	})

	// The server's log in the window.
	stage("server/gpuworks-server.log", func(w io.Writer) (int64, bool, error) {
		return m.serverLogWindow(w, b.From, b.To)
	})

	// Debug sessions that overlap the window: the server's files as they
	// are, and the session ids the nodes are asked for.
	sessions := m.sessionsIn(b.From, b.To)
	for _, s := range sessions {
		dir := filepath.Join(m.DebugDir(), s)
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // A diagnostics bundle skips unreadable files and retains the available evidence.
			}
			rel, _ := filepath.Rel(m.DebugDir(), p)
			info, _ := d.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			add(BundleItem{Name: "debug/" + filepath.ToSlash(rel), State: "collected", Bytes: size}, &stagedEntry{name: "debug/" + filepath.ToSlash(rel), path: p})
			return nil
		})
	}

	// Nodes: the agent log and session files, and a diagnosis.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, uuid := range b.Nodes {
		if !m.nodes.Online(uuid) {
			add(BundleItem{Name: "nodes/" + uuid, State: "skipped", Reason: "offline"}, nil)
			continue
		}
		wg.Add(1)
		go func(uuid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.collectNode(ctx, uuid, sessions, b.From, b.To, work, add)
			if b.Doctor {
				stage("nodes/"+uuid+"/doctor.json", func(w io.Writer) (int64, bool, error) {
					callCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
					defer cancel()
					raw, err := m.nodes.CallNode(callCtx, uuid, proto.RPCNodeDoctor, map[string]any{})
					if err != nil {
						return 0, false, err
					}
					n, err := w.Write(raw)
					return int64(n), false, err
				})
			}
		}(uuid)
	}

	// Instance logs.
	for _, uuid := range b.Instances {
		stage("instances/"+uuid+".log", func(w io.Writer) (int64, bool, error) {
			cw := &countingWriter{w: w}
			truncated, err := m.sources.InstanceLogs(ctx, uuid, b.From, b.To, cw, InstanceLogBytes)
			return cw.n, truncated, err
		})
	}
	wg.Wait()

	// The related records, credentials cut to their prefix.
	docs, err := m.sources.Snapshot(ctx, SnapshotScope{From: b.From, To: b.To, DeploymentIDs: b.Deployments, InstanceUUIDs: b.Instances, NodeUUIDs: b.Nodes})
	if err != nil {
		add(BundleItem{Name: "db", State: "failed", Reason: err.Error()}, nil)
	}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		doc := docs[name]
		stage("db/"+name+".json", func(w io.Writer) (int64, bool, error) {
			redacted, err := Redact(doc)
			if err != nil {
				return 0, false, err
			}
			raw, err := json.MarshalIndent(redacted, "", "  ")
			if err != nil {
				return 0, false, err
			}
			n, err := w.Write(raw)
			return int64(n), false, err
		})
	}

	sort.Slice(b.Items, func(i, j int) bool { return b.Items[i].Name < b.Items[j].Name })
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return b, m.pack(b, entries)
}

type stagedEntry struct{ name, path string }

// pack writes manifest.json and the staged entries as <id>.tar.gz.
func (m *Manager) pack(b Bundle, entries []stagedEntry) error {
	tmp := m.bundleArchive(b.ID) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	prefix := "gpuworks-diagnostics-" + b.ID + "/"
	manifest := b
	manifest.State = "ready"
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: prefix + "manifest.json", Mode: 0o600, Size: int64(len(raw)), ModTime: m.now()}); err != nil {
		return err
	}
	if _, err := tw.Write(raw); err != nil {
		return err
	}
	for _, e := range entries {
		if err := addFileToTar(tw, e.path, prefix+e.name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, m.bundleArchive(b.ID))
}

func addFileToTar(tw *tar.Writer, local, name string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, info.Size())
	return err
}

// serverLogWindow writes the server log's lines in the window, oldest
// generation first.
func (m *Manager) serverLogWindow(w io.Writer, from, to time.Time) (int64, bool, error) {
	var written int64
	files := m.sources.ServerLogFiles()
	for i := len(files) - 1; i >= 0; i-- {
		info, err := os.Stat(files[i])
		if err != nil || info.ModTime().Before(from) {
			continue // a generation last written before the window holds nothing in it
		}
		f, err := os.Open(files[i])
		if err != nil {
			continue
		}
		n, truncated, err := filterLogWindow(f, w, from, to, ServerLogBytes-written)
		_ = f.Close()
		written += n
		if err != nil || truncated {
			return written, truncated, err
		}
	}
	return written, false, nil
}

// sessionsIn are the debug sessions that overlap the window.
func (m *Manager) sessionsIn(from, to time.Time) []string {
	entries, err := os.ReadDir(m.DebugDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.DebugDir(), e.Name(), "session.json"))
		if err != nil {
			continue
		}
		var s DebugSession
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		end := s.Until
		if s.EndedAt != nil {
			end = *s.EndedAt
		}
		if !s.StartedAt.After(to) && !end.Before(from) {
			out = append(out, s.ID)
		}
	}
	sort.Strings(out)
	return out
}

// collectNode reads a node's part (diagnostics.read): its agent log, cut to
// the window, and the sessions' files as they are.
func (m *Manager) collectNode(ctx context.Context, uuid string, sessions []string, from, to time.Time, work string, add func(BundleItem, *stagedEntry)) {
	name := "nodes/" + uuid
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rc, err := m.sources.OpenNodeFiles(callCtx, uuid, sessions)
	if err != nil {
		add(BundleItem{Name: name, State: "failed", Reason: err.Error()}, nil)
		return
	}
	defer rc.Close()
	limit := agentLogBytes + int64(len(sessions))*NodeDebugBytes + (1 << 20)
	tr := tar.NewReader(io.LimitReader(rc, limit))
	var logs [2][]byte // agent.log.1, agent.log
	got := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			add(BundleItem{Name: name, State: "failed", Reason: "read: " + err.Error()}, nil)
			break
		}
		got = true
		switch clean := path.Clean(h.Name); {
		case clean == "agent.log.1" || clean == "agent.log":
			raw, _ := io.ReadAll(io.LimitReader(tr, agentLogBytes))
			if clean == "agent.log.1" {
				logs[0] = raw
			} else {
				logs[1] = raw
			}
		case strings.HasPrefix(clean, "debug/") && !strings.Contains(clean[len("debug/"):], "/"):
			local := filepath.Join(work, "nodes", uuid, filepath.FromSlash(clean))
			_ = os.MkdirAll(filepath.Dir(local), 0o700)
			f, err := os.Create(local)
			if err != nil {
				continue
			}
			n, err := io.Copy(f, io.LimitReader(tr, NodeDebugBytes))
			_ = f.Close()
			item := BundleItem{Name: name + "/" + clean, State: "collected", Bytes: n}
			if err != nil {
				item.State, item.Reason = "failed", err.Error()
			}
			add(item, &stagedEntry{name: name + "/" + clean, path: local})
		}
	}
	if !got {
		// An agent that predates the read closes the stream at once.
		add(BundleItem{Name: name + "/agent.log", State: "skipped", Reason: "unsupported"}, nil)
		return
	}
	local := filepath.Join(work, "nodes", uuid, "agent.log")
	_ = os.MkdirAll(filepath.Dir(local), 0o700)
	f, err := os.Create(local)
	if err != nil {
		add(BundleItem{Name: name + "/agent.log", State: "failed", Reason: err.Error()}, nil)
		return
	}
	var written int64
	truncated := false
	for _, raw := range logs {
		n, cut, _ := filterLogWindow(bytes.NewReader(raw), f, from, to, agentLogBytes-written)
		written += n
		truncated = truncated || cut
	}
	_ = f.Close()
	add(BundleItem{Name: name + "/agent.log", State: "collected", Bytes: written, Truncated: truncated}, &stagedEntry{name: name + "/agent.log", path: local})
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
