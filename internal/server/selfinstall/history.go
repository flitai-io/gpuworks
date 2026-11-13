package selfinstall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// History actions.
const (
	ActionInstall  = "install"
	ActionUpgrade  = "upgrade"
	ActionRollback = "rollback"
)

// HistoryEntry is one line of <data>/install-history.jsonl: an append-only
// record of every install and upgrade, including the ones that failed.
type HistoryEntry struct {
	Action            string `json:"action"`
	Result            string `json:"result"` // ok | failed
	ReleaseID         string `json:"release_id"`
	PreviousReleaseID string `json:"previous_release_id,omitempty"`
	Backup            string `json:"backup,omitempty"`
	Mode              string `json:"mode,omitempty"`
	By                string `json:"by,omitempty"`
	At                string `json:"at"`
	Error             string `json:"error,omitempty"`
}

// stampNow formats an audit timestamp.
func stampNow(now time.Time) string { return now.UTC().Format(time.RFC3339) }

// historyLine renders one audit line.
func historyLine(entry HistoryEntry) ([]byte, error) {
	if entry.At == "" {
		entry.At = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// AppendHistory adds an entry. Audit logging must never fail an install that
// otherwise worked, so the caller treats the error as a warning.
func AppendHistory(path string, entry HistoryEntry, mode os.FileMode) error {
	line, err := historyLine(entry)
	if err != nil {
		return err
	}
	return appendHistoryBytes(path, line, mode)
}

func appendHistoryBytes(path string, line []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// appendInstalled appends one audit line, falling back to `sudo tee -a` the
// same way the other tree writes do.
func (i *Installer) appendInstalled(ctx context.Context, path string, entry HistoryEntry, mode os.FileMode) error {
	line, err := historyLine(entry)
	if err != nil {
		return err
	}
	return i.appendHistoryLine(ctx, path, line, mode)
}

// createHistory makes sure the audit log exists, without an entry. The
// install calls it before its last chownTree: a log first created after the
// handoff belongs to whoever ran the install (root), and the service account
// that confirms a console upgrade could not append to it.
func (i *Installer) createHistory(ctx context.Context, path string, mode os.FileMode) error {
	return i.appendHistoryLine(ctx, path, nil, mode)
}

func (i *Installer) appendHistoryLine(ctx context.Context, path string, line []byte, mode os.FileMode) error {
	if err := appendHistoryBytes(path, line, mode); err == nil {
		return nil
	} else if !i.canSudo() {
		return err
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "tee", Args: []string{"-a", path}, Sudo: true,
		Stdin: bytes.NewReader(line), Stdout: io.Discard}); err != nil {
		return err
	}
	return i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", mode), path}, Sudo: true})
}

// ReadHistory returns the recorded entries, oldest first. Unreadable lines are
// skipped: a truncated tail from a crash must not hide the rest of the record.
func ReadHistory(path string) ([]HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []HistoryEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry HistoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}
