package app

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRuntimeImageErrorDiagnostics(t *testing.T) {
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	_, failure := exec.Command("sh", "-c", "echo 'permission denied API_TOKEN=secret-canary' >&2; exit 1").Output()
	err := localImageError(proto.RPCImageRuntimeList, proto.RuntimeImageSelection{Namespace: "default"}, fmt.Errorf("nerdctl namespace ls: %w", failure))
	var rpc *proto.RPCError
	if !errors.As(err, &rpc) || rpc.Code != proto.ErrorLocalImageUnavailable || strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected client error: %v", err)
	}
	if !strings.Contains(log.String(), "permission denied") || !strings.Contains(log.String(), "nerdctl namespace ls") || !strings.Contains(log.String(), "namespace=default") || strings.Contains(log.String(), "secret-canary") {
		t.Fatal("missing stage or unredacted diagnostic")
	}
	log.Reset()
	localImageError(proto.RPCImageRuntimeInspect, proto.RuntimeImageSelection{}, errors.New(strings.Repeat("x", 4096)))
	if log.Len() > 1600 {
		t.Fatalf("unbounded diagnostic: %d bytes", log.Len())
	}

	var lock sync.Mutex
	lock.Lock()
	defer lock.Unlock()
	raw := fmt.Sprintf(`{"ref":"example:v1","identity":"sha256:%s","runtime":"docker"}`, strings.Repeat("a", 64))
	images := runtimeImageRPC{driver: docker.New(), lock: &lock, stateDir: t.TempDir()}
	_, busyErr := images.selection(t.Context(), proto.RPCImageRuntimeImport, []byte(raw))
	if !errors.As(busyErr, &rpc) || rpc.Code != proto.ErrorLocalImageBusy {
		t.Fatalf("busy should have its own code: %v", busyErr)
	}
}
