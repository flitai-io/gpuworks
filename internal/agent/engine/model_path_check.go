package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
)

// modelPathCheckTimeout bounds the stat of a model path. A hard-mounted NFS
// that stopped answering would otherwise hang the launch forever.
const modelPathCheckTimeout = 2 * time.Second

// ModelPathUnavailable prefixes the failure message of a launch whose model
// directory is missing; the structured reason carries the same code.
const ModelPathUnavailable = proto.ErrorModelPathUnavailable

// checkModelPaths verifies, with a time limit, that the primary model
// directory and every model component mount exist as directories.
func checkModelPaths(spec proto.InstanceDesiredState) error {
	return checkModelPathsWith(spec, os.Stat)
}

// checkModelPathsWith is checkModelPaths with the stat to run, so a test can
// simulate a hung filesystem without touching state other goroutines read.
func checkModelPathsWith(spec proto.InstanceDesiredState, stat func(string) (os.FileInfo, error)) error {
	paths := []string{}
	if strings.TrimSpace(spec.ModelPath) != "" {
		paths = append(paths, spec.ModelPath)
	}
	for _, mount := range spec.Mounts {
		if strings.HasPrefix(mount.Purpose, "model_component:") && strings.TrimSpace(mount.Source) != "" {
			paths = append(paths, mount.Source)
		}
	}
	for _, path := range paths {
		err := statDirWithin(stat, path, modelPathCheckTimeout)
		if errors.Is(err, fs.ErrNotExist) && modelscan.MissingDirectory(stat, path) {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorModelPathMissing, Resource: path},
				fmt.Errorf("%s: model path %s does not exist on this node", proto.ErrorModelPathMissing, path))
		}
		if err != nil {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorModelPathUnavailable, Resource: path, Detail: err.Error()},
				fmt.Errorf("%s: model path %s is unavailable: %v", ModelPathUnavailable, path, err))
		}
	}
	return nil
}

func statDirWithin(stat func(string) (os.FileInfo, error), path string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		info, err := stat(path)
		if err == nil && !info.IsDir() {
			err = errors.New("not a directory")
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("the filesystem did not answer in time")
	}
}
