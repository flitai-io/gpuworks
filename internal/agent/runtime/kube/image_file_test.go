package kube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestCheckImageFileRejectsNonRegularHostFilesAndInvalidStat(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "nsenter"), []byte("#!/bin/sh\nprintf '%s\\n' \"$KUBE_TEST_STAT_OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, output string
		nonRegular   bool
	}{
		{"directory", "41ed 4096", true},
		{"device", "21b6 0", true},
		{"pipe", "11a4 0", true},
		{"empty", "", false},
		{"extra fields", "81a4 42 unexpected", false},
		{"bad mode", "invalid 42", false},
		{"bad size", "81a4 invalid", false},
		{"negative size", "81a4 -1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBE_TEST_STAT_OUTPUT", tc.output)
			_, err := New(Options{}).CheckImageFile(context.Background(), "/data/runner.tar")
			if err == nil || (tc.nonRegular && !errors.Is(err, runtime.ErrImageFileNotRegular)) {
				t.Fatalf("stat output %q: %v", tc.output, err)
			}
		})
	}
	// Large image sizes must not overflow a 32-bit counter.
	t.Setenv("KUBE_TEST_STAT_OUTPUT", "81a4 53687091200")
	if size, err := New(Options{}).CheckImageFile(context.Background(), "/data/runner.tar"); err != nil || size != 50<<30 {
		t.Fatalf("large archive: size=%d err=%v", size, err)
	}
}

func TestCheckImageFileReportsHostErrorsAndCancellation(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "nsenter"), []byte("#!/bin/sh\necho 'stat: permission denied' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := New(Options{})
	if _, err := d.CheckImageFile(context.Background(), "/data/runner.tar"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("host stat failure = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.CheckImageFile(ctx, "/data/runner.tar"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled host stat = %v", err)
	}
}

func TestLoadImageFileUsesKubeletNamespaceAndOriginalHostPath(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("KUBE_TEST_LOAD_ARGS", argsFile)
	installNerdctl(t, `printf '%s\n' "$@" > "$KUBE_TEST_LOAD_ARGS"
echo 'unpacking registry.example/runner:v1 (sha256:aabbcc)...'`)
	d := New(Options{Images: newImageStore(t.TempDir())})
	path := "/data/node image.tar"
	ref, err := d.LoadImageFileRef(context.Background(), "registry.example/runner:v1", path)
	if err != nil || ref != "registry.example/runner:v1" {
		t.Fatalf("load = %q, %v", ref, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || string(args) != "-n\nk8s.io\nload\n-i\n"+path+"\n" {
		t.Fatalf("load args = %q, %v", args, err)
	}
}
