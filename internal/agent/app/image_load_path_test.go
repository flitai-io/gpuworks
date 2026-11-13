package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
