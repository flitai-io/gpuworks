package nerdctl

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// nerdctl 1.7 prints "FATA[0000] 1 errors:\nno such image: org/model:v1" and
// exits 1 for an absent image.
func TestInspectImageReportsTheNerdctlAnswerAsErrImageNotFound(t *testing.T) {
	installFakeNerdctl(t, "printf 'FATA[0000] 1 errors:\\nno such image: org/model:v1\\n' >&2; exit 1")
	_, err := New().InspectImage(context.Background(), "org/model:v1")
	if !errors.Is(fmt.Errorf("collect: %w", err), runtime.ErrImageNotFound) {
		t.Fatalf("err = %v, want ErrImageNotFound", err)
	}
	installFakeNerdctl(t, "echo 'FATA[0000] cannot access containerd socket \"/run/containerd/containerd.sock\": permission denied' >&2; exit 1")
	if _, err := New().InspectImage(context.Background(), "org/model:v1"); err == nil || errors.Is(err, runtime.ErrImageNotFound) {
		t.Fatalf("err = %v, want a failure that is not ErrImageNotFound", err)
	}
}

// A nerdctl binary that cannot be executed fails with "executable file not
// found"; that text once matched the "not found" answer and made Remove
// report success.
func TestRemoveDoesNotMistakeAMissingBinaryForAMissingContainer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := New().Remove(context.Background(), "abc"); err == nil {
		t.Fatal("a missing nerdctl binary read as an absent container")
	}
	installFakeNerdctl(t, "echo 'FATA[0000] 1 errors:\nno such container: abc' >&2; exit 1")
	if err := New().Remove(context.Background(), "abc"); err != nil {
		t.Fatalf("remove of an absent container = %v", err)
	}
}

func TestInspectWorkloadReportsAMissingContainerAsErrContainerNotFound(t *testing.T) {
	installFakeNerdctl(t, "echo 'FATA[0000] 1 errors:\nno such container: abc' >&2; exit 1")
	if _, err := New().InspectWorkload(context.Background(), "abc"); !errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := New().InspectWorkload(context.Background(), "abc"); err == nil || errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("a missing binary read as a missing container: %v", err)
	}
}

// nerdctl that may not use the containerd socket prints this and exits 1.
func TestInspectionsReportASocketRefusalAsErrPermissionDenied(t *testing.T) {
	installFakeNerdctl(t, "echo 'FATA[0000] cannot access containerd socket \"/run/containerd/containerd.sock\": permission denied' >&2; exit 1")
	if _, err := New().InspectWorkload(context.Background(), "abc"); !runtime.IsPermissionDenied(fmt.Errorf("observe: %w", err)) || errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("workload err = %v, want ErrPermissionDenied only", err)
	}
	if _, err := New().InspectImage(context.Background(), "org/model:v1"); !runtime.IsPermissionDenied(err) || errors.Is(err, runtime.ErrImageNotFound) {
		t.Fatalf("image err = %v, want ErrPermissionDenied only", err)
	}
}
