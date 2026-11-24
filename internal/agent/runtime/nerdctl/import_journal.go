package nerdctl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// NeedsLocalImageCopy recovers interrupted attempts even when the last process
// already published the final name and a retry can register it directly.
func (d *Driver) NeedsLocalImageCopy(ctx context.Context, ref, configID, stateDir string) (bool, error) {
	journal, err := d.openImportJournal(stateDir)
	if err != nil {
		return false, err
	}
	defer journal.Close()
	if err := d.recoverImportTags(ctx, journal.Name()); err != nil {
		return false, err
	}
	present, err := d.sameLocalImage(ctx, ref, configID)
	return !present, err
}

// Lock the per-target journal until both private tags are gone. A process exit
// releases the lock, leaving the exact source and random tag for recovery.
func (d *Driver) openImportJournal(stateDir string) (*os.File, error) {
	if stateDir == "" {
		return nil, errors.New("missing agent state directory for local import")
	}
	dir := filepath.Join(stateDir, "local-image-imports", d.ns)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, runtime.ErrLocalImageBusy
		}
		return nil, fmt.Errorf("lock local import journal: %w", err)
	}
	return f, nil
}

func journalImportTag(dir, source string) (string, string, error) {
	id := strings.ToLower(rand.Text())
	marker := filepath.Join(dir, source+"+"+id)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", "", err
	}
	return localImportStagePrefix + id, marker, f.Close()
}

func (d *Driver) recoverImportTags(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		ns, id, ok := strings.Cut(e.Name(), "+")
		if !ok || ns == "" || !runtime.ValidImageNamespace(ns) || id == "" || !e.Type().IsRegular() {
			continue
		}
		if err := d.clearImportTag(filepath.Join(dir, e.Name()), ns, localImportStagePrefix+id); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) clearImportTag(marker, source, stage string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var cleanup error
	for _, ns := range []string{source, d.ns} {
		driver := NewInNamespace(ns)
		err := driver.RemoveImage(ctx, stage)
		if err != nil {
			if _, inspectErr := driver.InspectImage(ctx, stage); errors.Is(inspectErr, runtime.ErrImageNotFound) {
				continue
			}
		}
		cleanup = errors.Join(cleanup, err)
	}
	if cleanup != nil {
		return cleanup
	}
	return os.Remove(marker)
}
