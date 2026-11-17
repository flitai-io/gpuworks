//go:build linux || darwin

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestModelInstallPermissionLog(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "model")
	cause := fmt.Errorf("install %s: %w", target, &os.LinkError{Op: "rename", Old: "staging", New: target, Err: syscall.EACCES})
	for _, tc := range []struct {
		name, path string
		cause      error
	}{
		{"permission", target, cause},
		{"missing directory", filepath.Join(parent, "missing", "model"), cause},
		{"other error", target, syscall.ENOSPC},
		{"permission text is not an errno", target, errors.New("permission denied")},
		{"success", target, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			r := newStorageTestRuntimeLog(t, slog.New(slog.NewJSONHandler(&output, nil)))
			output.Reset()
			task := proto.ModelFileTask{Key: "model-file", TransferID: "transfer", StorageID: 7}
			logModelInstallPermission(r.NodeRuntime, task, tc.path, tc.cause)
			if !errors.Is(tc.cause, os.ErrPermission) {
				if output.Len() != 0 {
					t.Fatalf("non-permission result produced a diagnostic: %s", &output)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatalf("invalid diagnostic: %s: %v", &output, err)
			}
			for key, want := range map[string]any{"err": cause.Error(), "key": task.Key, "transfer_id": task.TransferID, "storage_id": float64(task.StorageID), "target_parent": filepath.Dir(tc.path)} {
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
			if tc.path != target {
				if got["permissions"] != "unknown" || got["permissions_error"] == nil || got["writable"] != nil {
					t.Fatalf("failed stat guessed permissions: %v", got)
				}
				return
			}
			for key, want := range map[string]any{"mode": "drwxr-x---", "mode_octal": "0750", "uid": float64(os.Geteuid()), "gid": float64(os.Getegid()), "readable": true, "writable": true, "searchable": true} {
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

func TestModelInstallPermissionLogBoundsUnresponsiveStorage(t *testing.T) {
	var output bytes.Buffer
	r := newStorageTestRuntimeLog(t, slog.New(slog.NewJSONHandler(&output, nil)))
	r.storage.timeouts.marker = 20 * time.Millisecond
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	original := modelInstallPermissions
	modelInstallPermissions = func(string) ([]slog.Attr, error) {
		calls.Add(1)
		defer close(finished)
		<-release
		return []slog.Attr{slog.Bool("writable", true)}, nil
	}
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("diagnostic reader did not finish after release")
		}
		modelInstallPermissions = original
	})
	target := filepath.Join(t.TempDir(), "model")
	for range 2 {
		output.Reset()
		logModelInstallPermission(r.NodeRuntime, proto.ModelFileTask{StorageID: 1}, target, syscall.EACCES)
		var got map[string]any
		if err := json.Unmarshal(output.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["err"] != syscall.EACCES.Error() || got["permissions"] != "unknown" || got["permissions_error"] != storagemount.ErrUnresponsive.Error() || got["writable"] != nil {
			t.Fatalf("timeout replaced the failure or exposed unfinished results: %v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("a stuck directory accumulated %d diagnostic reads", calls.Load())
	}
}

func TestModelInstallFailureLogsPermissionsWithoutChangingStatus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as an unprivileged user to exercise real directory permission denial")
	}
	for _, acquisition := range []string{"download", "download existing staging", "copy"} {
		t.Run(acquisition, func(t *testing.T) {
			var output bytes.Buffer
			r := newStorageTestRuntimeLog(t, slog.New(slog.NewJSONHandler(&output, nil)))
			parent, work, source := t.TempDir(), filepath.Join(t.TempDir(), "work"), t.TempDir()
			final := filepath.Join(parent, "model")
			t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
			task := copyTask("file", source)
			task.TransferID, task.Staging = "transfer", work
			if acquisition == "copy" {
				writeModel(t, source, "weights")
				r.Bind(func(kind string, value any) error {
					if kind == proto.TStatusModelFile {
						st := value.(proto.ModelFileStatus)
						r.mu.Lock()
						r.files = append(r.files, st)
						r.mu.Unlock()
						if st.Preparation != nil && st.Preparation.Phase == proto.ModelPhaseInstalling {
							return os.Chmod(parent, 0o555)
						}
					}
					return nil
				})
				r.copyModelDir(context.Background(), task, work, final)
			} else {
				files := urlModelFiles()
				_, endpoint, _ := newAutoindexFixture(t, "/org/model", files)
				task = urlTask(endpoint)
				task.TransferID, task.Staging = "transfer", work
				if acquisition == "download existing staging" {
					if err := os.MkdirAll(work, 0o755); err != nil {
						t.Fatal(err)
					}
					for name, file := range files {
						if err := os.WriteFile(filepath.Join(work, name), []byte(file.body), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := os.Chmod(parent, 0o555); err != nil {
					t.Fatal(err)
				}
				r.downloadRepo(context.Background(), task, work, final)
			}
			status := r.lastFile(t, task.Key)
			want := fmt.Sprintf("install %s: %s", final, (&os.LinkError{Op: "rename", Old: work, New: final, Err: syscall.EACCES}).Error())
			if status.State != proto.MFFailed || status.Message != want || status.TransferID != task.TransferID {
				t.Fatalf("original status changed: %+v; want message %q", status, want)
			}
			var got map[string]any
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var entry map[string]any
				if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "model installation permission denied" {
					got = entry
				}
			}
			if got["err"] != want || got["target_parent"] != parent || got["mode_octal"] != "0555" || got["writable"] != false || got["readable"] != true || got["searchable"] != true {
				t.Fatalf("missing permission evidence: %s", &output)
			}
			if _, err := os.Stat(filepath.Join(work, "model.safetensors")); err != nil {
				t.Fatalf("diagnostics removed staged weights: %v", err)
			}
			if err := os.Chmod(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if acquisition == "copy" {
				r.Bind(nil)
				if err := installStagedModel(work, final, task); err != nil {
					t.Fatal(err)
				}
			} else {
				r.downloadRepo(context.Background(), task, work, final)
				if got := r.lastFile(t, task.Key); got.State != proto.MFPresent {
					t.Fatalf("retry failed: %+v", got)
				}
			}
			if strings.Contains(output.String(), "model installation permission denied") {
				t.Fatalf("successful installation logged a permission failure: %s", &output)
			}
		})
	}
}
