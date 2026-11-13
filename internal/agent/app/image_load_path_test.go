package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

func localArchive(t *testing.T) (string, []byte) {
	t.Helper()
	data, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{loadURLCanonical}, LayerBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image with spaces.tar")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func loadPathParams(path string) json.RawMessage {
	raw, _ := json.Marshal(proto.ImageLoadPathParams{CanonicalRef: loadURLCanonical, ArchivePath: path, TransferID: "local-import"})
	return raw
}

func TestImageLoadPathUsesNodeFileAndPreservesIt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "runtime failure"}[fail], func(t *testing.T) {
			h := newArchiveLoadHarness(t)
			path, original := localArchive(t)
			if fail {
				h.driver.InjectLoadFailure(errors.New("invalid image archive"))
			}
			result, err := h.node.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path))
			if err != nil || result.(proto.ImageLoadPathResult).State != "syncing" {
				t.Fatalf("start = %+v, %v", result, err)
			}
			status := h.awaitTerminal(t)
			want := "present"
			if fail {
				want = "failed"
			}
			if status.State != want || status.TransferID != "local-import" || (!fail && status.ImageID != "sha256:loaded") {
				t.Fatalf("terminal status = %+v", status)
			}
			paths := h.driver.LoadedPaths()
			if len(paths) != 1 || paths[0] != path {
				t.Fatalf("loaded paths = %v, want original node file", paths)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(original) {
				t.Fatalf("original archive changed or removed: %v", err)
			}
			if len(h.fixture.Requests()) != 0 {
				t.Fatal("local import contacted an archive server")
			}
		})
	}
}

func TestImageLoadPathRejectsMissingFilesAndDirectories(t *testing.T) {
	h := newArchiveLoadHarness(t)
	for _, path := range []string{"", "relative.tar", t.TempDir(), filepath.Join(t.TempDir(), "missing.tar"), "/dev/null"} {
		if _, err := h.node.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path)); err == nil {
			t.Errorf("accepted non-file path %q", path)
		}
	}
	if len(h.driver.Calls()) != 0 {
		t.Fatal("an invalid path reached the runtime")
	}
}

func TestImageLoadPathCancelledInQueueDoesNotLoad(t *testing.T) {
	h := newArchiveLoadHarness(t)
	path, _ := localArchive(t)
	h.node.imagePullMu.Lock()
	_, err := h.node.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path))
	if err != nil {
		h.node.imagePullMu.Unlock()
		t.Fatal(err)
	}
	cancelled := h.node.cancelImageOperation(loadURLCanonical, "local-import")
	h.node.imagePullMu.Unlock()
	if !cancelled {
		t.Fatal("accepted operation was not cancellable")
	}
	status := h.awaitTerminal(t)
	if status.State != "failed" || !strings.Contains(status.Message, "cancel") || len(h.driver.LoadedPaths()) != 0 {
		t.Fatalf("cancelled load = %+v, paths = %v", status, h.driver.LoadedPaths())
	}
}

type kubeArchiveImages struct{ *recording.Driver }

func (d *kubeArchiveImages) ExecPTY(context.Context, string, []string) ([]string, error) {
	return nil, errors.New("not used by image import")
}

func TestImageLoadPathUsesKubernetesHostFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name, hostKind string
		podFile        bool
	}{
		{name: "archive outside pod mounts", hostKind: "file"},
		{name: "same path holds different pod file", hostKind: "file", podFile: true},
		{name: "pod file cannot substitute for missing host file", podFile: true},
		{name: "pod file cannot substitute for host directory", hostKind: "directory", podFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newArchiveLoadHarness(t)
			driver := kube.New(kube.Options{Images: &kubeArchiveImages{h.driver}})
			node := NewNodeRuntime(h.node.log, driver, h.node.ModelsRoot())
			node.send = h.node.send
			h.node = node
			path := filepath.Join(t.TempDir(), "node image.tar")
			if tc.podFile {
				if err := os.WriteFile(path, []byte("pod file"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hostRoot := t.TempDir()
			hostPath := filepath.Join(hostRoot, path)
			original := []byte("archive on the host, outside the pod mounts")
			if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
				t.Fatal(err)
			}
			switch tc.hostKind {
			case "file":
				if err := os.WriteFile(hostPath, original, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(hostPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate the host mount namespace while retaining real stat results.
			bin := t.TempDir()
			t.Setenv("KUBE_TEST_HOST_ROOT", hostRoot)
			script := `#!/bin/sh
set -eu
[ "$#" -eq 10 ] && [ "$1" = -t ] && [ "$2" = 1 ] && [ "$3" = -m ] && [ "$4" = -- ]
[ "$5" = stat ] && [ "$6" = -L ] && [ "$7" = -c ] && [ "$8" = '%f %s' ] && [ "$9" = -- ]
exec stat -L -c '%f %s' -- "$KUBE_TEST_HOST_ROOT${10}"
`
			if err := os.WriteFile(filepath.Join(bin, "nsenter"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			_, err := node.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path))
			if tc.hostKind != "file" {
				if err == nil || len(h.driver.LoadedPaths()) != 0 {
					t.Fatalf("invalid host file accepted: err=%v paths=%v", err, h.driver.LoadedPaths())
				}
				return
			}
			if err != nil {
				t.Fatalf("host archive rejected: %v", err)
			}
			status := h.awaitTerminal(t)
			if status.State != "present" || status.SizeBytes != int64(len(original)) || status.TransferredBytes != status.SizeBytes {
				t.Fatalf("host import status = %+v", status)
			}
			if paths := h.driver.LoadedPaths(); len(paths) != 1 || paths[0] != path {
				t.Fatalf("runtime must receive the original host path: %v", paths)
			}
			if data, err := os.ReadFile(hostPath); err != nil || string(data) != string(original) {
				t.Fatalf("host archive changed or removed: %v", err)
			}
		})
	}
}
