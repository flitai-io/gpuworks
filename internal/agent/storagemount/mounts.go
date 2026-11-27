package storagemount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// mountRecord is a mount this agent created, kept so it can be unmounted later, also after a
// restart. The export guards against unmounting something else that was mounted over the path.
type mountRecord struct {
	Path   string `json:"path"`
	Export string `json:"export"`
}

type stateFile struct {
	Mounts []mountRecord `json:"mounts"`
}

func (m *Manager) loadState() {
	if m.o.StateFile == "" {
		return
	}
	b, err := os.ReadFile(m.o.StateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.o.Logger.Warn("storagemount: cannot read state file", "path", m.o.StateFile, "err", err)
		}
		return
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		m.o.Logger.Warn("storagemount: ignoring malformed state file", "path", m.o.StateFile, "err", err)
		return
	}
	for _, r := range sf.Mounts {
		if r.Path != "" {
			m.mounted[filepath.Clean(r.Path)] = r
		}
	}
}

// saveStateLocked persists the mount set; m.mu must be held.
func (m *Manager) saveStateLocked() {
	if m.o.StateFile == "" {
		return
	}
	sf := stateFile{Mounts: make([]mountRecord, 0, len(m.mounted))}
	for _, r := range m.mounted {
		sf.Mounts = append(sf.Mounts, r)
	}
	sort.Slice(sf.Mounts, func(i, j int) bool { return sf.Mounts[i].Path < sf.Mounts[j].Path })
	b, _ := json.Marshal(sf)
	if err := writeFileAtomic(m.o.StateFile, b, 0o644); err != nil {
		m.o.Logger.Warn("storagemount: cannot save state file", "path", m.o.StateFile, "err", err)
	}
}

func (m *Manager) recordMount(path, export string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	m.mounted[path] = mountRecord{Path: path, Export: export}
	m.saveStateLocked()
}

func (m *Manager) forgetMount(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	if _, ok := m.mounted[path]; !ok {
		return
	}
	delete(m.mounted, path)
	m.saveStateLocked()
}

// writeFileAtomic writes via a temp file in the same directory and renames it into place.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, perm)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (m *Manager) readMounts() ([]MountEntry, error) { return readMountsAt(m.o.MountinfoPath) }

func readMountsAt(path string) ([]MountEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountinfo(f)
}

// call runs fn under the timed guard with CallTimeout.
func (m *Manager) call(ctx context.Context, key string, fn func() error) error {
	return m.g.run(ctx, key, m.o.CallTimeout, fn)
}

// mountResult is what a mount command produced.
type mountResult struct {
	out      []byte
	err      error
	timedOut bool
}

// runMount runs "mount -t nfs -o opts export path" under the guard. With record set, a successful
// mount is remembered from inside the guarded call, so a mount that finishes after the caller gave
// up is still tracked and can be unmounted later.
func (m *Manager) runMount(ctx context.Context, key, export, path, opts string, record bool) (mountResult, error) {
	var res mountResult
	err := m.g.run(ctx, key, m.o.MountTimeout+m.o.CallTimeout, func() error {
		cctx, cancel := context.WithTimeout(ctx, m.o.MountTimeout)
		defer cancel()
		clean := filepath.Clean(path)
		m.noteMounting(clean, 1)
		defer m.noteMounting(clean, -1)
		res.out, res.err = m.o.Runner(cctx, "mount", "-t", "nfs", "-o", opts, export, path)
		res.timedOut = cctx.Err() != nil && errors.Is(cctx.Err(), context.DeadlineExceeded)
		if res.err == nil && record {
			m.recordMount(path, export)
		}
		return nil
	})
	if err != nil {
		return mountResult{}, err // res may still be written by a stuck call
	}
	return res, nil
}

// runUmount runs a plain "umount path" (never -l or -f) under the guard. cmdErr is the command's
// own failure; err is a guard failure (time out), in which case out and cmdErr are meaningless.
func (m *Manager) runUmount(ctx context.Context, key, path string) (out []byte, cmdErr error, err error) {
	var o []byte
	var ce error
	err = m.g.run(ctx, key, m.o.MountTimeout+m.o.CallTimeout, func() error {
		cctx, cancel := context.WithTimeout(ctx, m.o.MountTimeout)
		defer cancel()
		o, ce = m.o.Runner(cctx, "umount", path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return o, ce, nil
}

// mountFailure turns a failed mount command into a reason and a message with truncated output.
func mountFailure(res mountResult, timeout time.Duration) (reason, message string) {
	text := string(res.out)
	if res.timedOut {
		return ReasonUnreachable, fmt.Sprintf("mount did not finish within %s", timeout)
	}
	reason = ClassifyMountError(text)
	if errors.Is(res.err, exec.ErrNotFound) {
		// No mount helper at all: exec could not find the program.
		reason = ReasonMissingClient
	}
	if res.err != nil && strings.TrimSpace(text) == "" {
		text = res.err.Error()
	}
	return reason, "mount failed: " + truncate(strings.TrimSpace(text), 400)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// unmountStale unmounts mounts this agent created whose paths are no longer wanted. Busy mounts
// and failed unmounts stay recorded and are retried by the next reconcile.
func (m *Manager) unmountStale(ctx context.Context, keep map[string]bool) {
	m.mu.Lock()
	var stale []mountRecord
	for p, r := range m.mounted {
		if !keep[p] {
			stale = append(stale, r)
		}
	}
	m.mu.Unlock()
	if len(stale) == 0 {
		return
	}
	entries, err := m.readMounts()
	if err != nil {
		m.o.Logger.Warn("storagemount: cannot read mountinfo, skipping unmount", "err", err)
		return
	}
	done := make(chan struct{}, len(stale))
	for _, r := range stale {
		go func() {
			defer func() { done <- struct{}{} }()
			m.unmountOne(ctx, entries, r)
		}()
	}
	for range stale {
		<-done
	}
}

func (m *Manager) unmountOne(ctx context.Context, entries []MountEntry, r mountRecord) {
	log := m.o.Logger.With("path", r.Path, "export", r.Export)
	e, ok := FindMount(entries, r.Path)
	if !ok || normalizeSource(e.Source) != normalizeSource(r.Export) {
		log.Info("storagemount: recorded mount is gone, forgetting it")
		m.forgetMount(r.Path)
		return
	}
	if m.o.Busy(r.Path) {
		log.Warn("storagemount: storage removed but still in use by a container, leaving it mounted")
		return
	}
	out, cmdErr, err := m.runUmount(ctx, "m:"+r.Path, r.Path)
	switch {
	case err != nil:
		log.Warn("storagemount: umount did not return", "err", err)
	case cmdErr != nil:
		log.Warn("storagemount: umount failed, leaving it mounted", "err", cmdErr, "output", truncate(string(out), 400))
	default:
		m.forgetMount(r.Path)
		log.Info("storagemount: unmounted")
	}
}
