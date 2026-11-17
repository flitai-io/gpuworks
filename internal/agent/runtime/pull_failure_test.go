package runtime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// Output as docker 27 and nerdctl 1.7 print it for each failure.
func TestPullOutputFailureReadsTheCLIOutput(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   error
	}{
		{"failed to register layer: write /var/lib/docker/overlay2/x/diff/a: no space left on device", ErrNoSpace},
		{"Error response from daemon: manifest for quay.io/x:1 not found: manifest unknown: manifest unknown", ErrRegistryRefused},
		{"Error response from daemon: pull access denied for x, repository does not exist or may require 'docker login': denied: requested access to the resource is denied", ErrRegistryRefused},
		{`FATA[0001] failed to resolve reference "quay.io/x:1": quay.io/x:1: not found`, ErrRegistryRefused},
		{`FATA[0000] failed to resolve reference "r.example/x:1": pulling from host r.example failed with status code [manifests 1]: 401 Unauthorized`, ErrRegistryRefused},
		{"unknown: Tag 1 was deleted or has expired. To pull, revive via time machine", ErrRegistryRefused},
		{`Error response from daemon: Get "https://r.example/v2/": dial tcp 10.0.0.1:443: i/o timeout`, nil},
		{"permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock", nil},
		{"", nil},
	} {
		if got := PullOutputFailure(tc.output); got != tc.want {
			t.Errorf("PullOutputFailure(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

func TestMarkPullFailureKeepsTheTextAndBothChains(t *testing.T) {
	inner := &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}
	err := MarkPullFailure(fmt.Errorf("docker pull: %w", inner), ErrNoSpace)
	if err.Error() != "docker pull: write /x: no space left on device" {
		t.Fatalf("text changed: %q", err)
	}
	wrapped := fmt.Errorf("pull: %w", err)
	if !errors.Is(wrapped, ErrNoSpace) || !errors.Is(wrapped, syscall.ENOSPC) || errors.Is(wrapped, ErrRegistryRefused) {
		t.Fatalf("classification lost through wrapping: %v", wrapped)
	}
	plain := errors.New("x")
	if MarkPullFailure(plain, nil) != plain || MarkPullFailure(nil, ErrNoSpace) != nil {
		t.Fatal("nil kind or nil error must pass through")
	}
	if !DiskFull(wrapped) || !DiskFull(syscall.EDQUOT) || DiskFull(errors.New("no space left on device")) {
		t.Fatal("DiskFull must read the errno, not the text")
	}
}
