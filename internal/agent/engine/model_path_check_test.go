package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCheckModelPathsAcceptsDirectories(t *testing.T) {
	dir := t.TempDir()
	component := filepath.Join(dir, "component")
	if err := os.Mkdir(component, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := proto.InstanceDesiredState{ModelPath: dir}
	spec.Mounts = []proto.MountSpec{{Source: component, Purpose: "model_component:vision"}, {Source: "/nonexistent-other", Purpose: "kv_disk"}}
	if err := checkModelPaths(spec); err != nil {
		t.Fatalf("check = %v", err)
	}
}

func TestCheckModelPathsRejectsMissingFileAndComponent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "weights.bin")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	component := proto.InstanceDesiredState{ModelPath: dir}
	component.Mounts = []proto.MountSpec{{Source: filepath.Join(dir, "absent"), Purpose: "model_component:vision"}}
	// A directory that is not there is the node's finding that the copy is
	// gone; one that is there and unusable is not.
	for name, tc := range map[string]struct {
		spec proto.InstanceDesiredState
		code string
		path string
	}{
		"missing":   {proto.InstanceDesiredState{ModelPath: filepath.Join(dir, "absent")}, proto.ErrorModelPathMissing, filepath.Join(dir, "absent")},
		"file":      {proto.InstanceDesiredState{ModelPath: file}, proto.ErrorModelPathUnavailable, file},
		"component": {component, proto.ErrorModelPathMissing, filepath.Join(dir, "absent")},
	} {
		err := checkModelPaths(tc.spec)
		failure := failureOf(err, "", "")
		if err == nil || !strings.HasPrefix(err.Error(), tc.code+": ") || failure.Code != tc.code || failure.Resource != tc.path {
			t.Fatalf("%s: err = %v, failure = %+v, want %s on %s", name, err, failure, tc.code, tc.path)
		}
	}
}

// A model directory that is not there inside a storage root that still holds
// other models is gone. The same absence under an empty directory is what a
// dropped or unmounted filesystem looks like, and it is not the node's word
// that the files are gone.
func TestCheckModelPathsTellsAGoneDirectoryFromAnEmptyMountPoint(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "models")
	if err := os.MkdirAll(filepath.Join(storage, "other-model"), 0o755); err != nil {
		t.Fatal(err)
	}
	mountPoint := filepath.Join(root, "mnt")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		code string
	}{
		"models still there":   {filepath.Join(storage, "gone-model"), proto.ErrorModelPathMissing},
		"storage root emptied": {filepath.Join(mountPoint, "gone-model"), proto.ErrorModelPathUnavailable},
	} {
		err := checkModelPaths(proto.InstanceDesiredState{ModelPath: tc.path})
		if failure := failureOf(err, "", ""); err == nil || failure.Code != tc.code || failure.Resource != tc.path {
			t.Errorf("%s: err = %v, failure = %+v, want %s on %s", name, err, failure, tc.code, tc.path)
		}
	}
}

func TestCheckModelPathsGivesUpOnAHungFilesystem(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hung := func(string) (fs.FileInfo, error) { <-release; return nil, os.ErrNotExist }
	start := time.Now()
	err := checkModelPathsWith(proto.InstanceDesiredState{ModelPath: "/mnt/gpuworks/hung/model"}, hung)
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > modelPathCheckTimeout+time.Second {
		t.Fatalf("check took %s", elapsed)
	}
}
