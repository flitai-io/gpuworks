package docker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The fake CLI prints what docker 27 prints for an absent image:
// "Error: No such image: org/model:v1" on stderr, exit 1.
func TestInspectImageReportsTheDockerAnswerAsErrImageNotFound(t *testing.T) {
	installFakeDocker(t, "echo 'Error: No such image: org/model:v1' >&2; exit 1")
	_, err := New().InspectImage(context.Background(), "org/model:v1")
	if !errors.Is(err, runtime.ErrImageNotFound) || !errors.Is(fmt.Errorf("collect: %w", err), runtime.ErrImageNotFound) {
		t.Fatalf("err = %v, want ErrImageNotFound (also when wrapped)", err)
	}
	present, id, size, err := New().ImagePresence(context.Background(), "org/model:v1")
	if present || id != "" || size != nil || err != nil {
		t.Fatalf("presence = %v %q %v %v", present, id, size, err)
	}
}

func TestInspectImageKeepsADaemonFailureAnError(t *testing.T) {
	installFakeDocker(t, "echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?' >&2; exit 1")
	_, err := New().InspectImage(context.Background(), "org/model:v1")
	if err == nil || errors.Is(err, runtime.ErrImageNotFound) {
		t.Fatalf("err = %v, want a failure that is not ErrImageNotFound", err)
	}
	if _, _, _, err := New().ImagePresence(context.Background(), "org/model:v1"); err == nil {
		t.Fatal("presence swallowed a daemon failure")
	}
}

// Only the CLI's own answer proves a container is gone: a docker binary that
// cannot run at all must not read as "already removed".
func TestRemoveAndStatusTrustOnlyTheCLIAnswer(t *testing.T) {
	installFakeDocker(t, "echo 'Error response from daemon: No such container: abc' >&2; exit 1")
	if err := New().Remove(context.Background(), "abc"); err != nil {
		t.Fatalf("remove of an absent container = %v", err)
	}
	installFakeDocker(t, "echo 'Error: No such object: abc' >&2; exit 1")
	if st, err := New().Status(context.Background(), "abc"); err != nil || st.Exists {
		t.Fatalf("status = %+v %v", st, err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := New().Remove(context.Background(), "abc"); err == nil {
		t.Fatal("a missing docker binary read as an absent container")
	}
	if _, err := New().Status(context.Background(), "abc"); err == nil {
		t.Fatal("a missing docker binary read as an absent container")
	}
}

// docker 27 prints a missing tag this way; the driver marks the error where
// the CLI ran so the image puller never reads the text.
func TestPullImageMarksARegistryRefusalFromTheCLIOutput(t *testing.T) {
	installFakeDocker(t, "echo 'Error response from daemon: manifest for quay.io/x:1 not found: manifest unknown: manifest unknown' >&2; exit 1")
	err := New().PullImage(context.Background(), "quay.io/x:1", nil)
	if !errors.Is(fmt.Errorf("walk: %w", err), runtime.ErrRegistryRefused) || errors.Is(err, runtime.ErrNoSpace) {
		t.Fatalf("err = %v, want ErrRegistryRefused", err)
	}
	installFakeDocker(t, "echo 'failed to register layer: write /var/lib/docker/overlay2/x: no space left on device' >&2; exit 1")
	if err := New().PullImage(context.Background(), "quay.io/x:1", nil); !errors.Is(err, runtime.ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	installFakeDocker(t, "echo 'permission denied while trying to connect to the Docker daemon socket' >&2; exit 1")
	if err := New().PullImage(context.Background(), "quay.io/x:1", nil); err == nil || errors.Is(err, runtime.ErrRegistryRefused) {
		t.Fatalf("err = %v, want an unmarked local failure", err)
	}
}

func TestInspectWorkloadReportsAMissingContainerAsErrContainerNotFound(t *testing.T) {
	installFakeDocker(t, "echo 'Error: No such container: abc' >&2; exit 1")
	if _, err := New().InspectWorkload(context.Background(), "abc"); !errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("err = %v", err)
	}
	installFakeDocker(t, "echo 'permission denied while trying to connect to the Docker daemon socket' >&2; exit 1")
	if _, err := New().InspectWorkload(context.Background(), "abc"); err == nil || errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// docker 27 refused by the daemon socket prints this on stderr and exits 1.
// The refusal is runtime.ErrPermissionDenied for both inspections; a binary
// that cannot run is not a refusal.
func TestInspectionsReportASocketRefusalAsErrPermissionDenied(t *testing.T) {
	installFakeDocker(t, "echo 'permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/abc/json\": dial unix /var/run/docker.sock: connect: permission denied' >&2; exit 1")
	if _, err := New().InspectWorkload(context.Background(), "abc"); !runtime.IsPermissionDenied(fmt.Errorf("observe: %w", err)) {
		t.Fatalf("workload err = %v, want ErrPermissionDenied", err)
	}
	if _, err := New().InspectImage(context.Background(), "org/model:v1"); !runtime.IsPermissionDenied(err) || errors.Is(err, runtime.ErrImageNotFound) {
		t.Fatalf("image err = %v, want ErrPermissionDenied only", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := New().InspectWorkload(context.Background(), "abc"); err == nil || runtime.IsPermissionDenied(err) {
		t.Fatalf("a missing binary read as a refusal: %v", err)
	}
}
