// Package kubeboot starts the agent of a Kubernetes cluster's DaemonSet from
// a binary kept in the node's state directory rather than the one in the
// image. The image is only the agent's runtime (base, nerdctl) and carries a
// binary to seed that directory with; the platform's ordinary agent upgrade
// then replaces the binary in place (internal/agent/conn), the container's
// restart serving as the supervisor that an installed agent has in systemd.
// The binary survives the pod: the state directory is a host path. See ADR
// 0052.
package kubeboot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// RevisionEnv is set by the agent image (build/agent-image/Dockerfile) to
	// the image's runtime revision; a binary started without it is not the
	// image's own.
	RevisionEnv = "GPUWORKS_AGENT_IMAGE_REVISION"
	// BinaryEnv is set for the process the image's binary hands over to: the
	// path of the binary it runs as.
	BinaryEnv = "GPUWORKS_KUBE_AGENT_BINARY"
	// UnavailableEnv says why the image's own binary kept running instead,
	// for the agent to log; the node is then upgraded by its DaemonSet image.
	UnavailableEnv = "GPUWORKS_KUBE_AGENT_BINARY_UNAVAILABLE"

	// Dir is the directory under the state directory holding the binary.
	Dir        = "agent"
	binaryName = "gpuworks-agent"
	// seedFile records the sha256 of the image binary the directory was
	// seeded from; a different image binary (the operator changed the image)
	// seeds it again.
	seedFile = "seed"
	// alignFile marks a freshly seeded binary: on its first start the agent
	// takes the server's own binary once (Align).
	alignFile = "align-pending"
)

// execFn is syscall.Exec; tests replace it.
var execFn = syscall.Exec

// Prepare makes stateDir/agent/gpuworks-agent the binary to run: kept when
// it was seeded from this image binary, copied from image otherwise. fresh
// reports a new copy.
func Prepare(stateDir, image string) (bin string, fresh bool, err error) {
	dir := filepath.Join(stateDir, Dir)
	bin = filepath.Join(dir, binaryName)
	sum, err := fileSHA256(image)
	if err != nil {
		return "", false, fmt.Errorf("read the image's agent binary: %w", err)
	}
	seed, _ := os.ReadFile(filepath.Join(dir, seedFile))
	if _, statErr := os.Stat(bin); statErr == nil && strings.TrimSpace(string(seed)) == sum {
		return bin, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	// A pending upgrade belongs to the binary being replaced.
	for _, stale := range []string{".old", ".new", ".upgrade-pending", ".upgrade-rolled-back"} {
		_ = os.Remove(bin + stale)
	}
	if err := copyFile(image, bin, 0o755); err != nil {
		return "", false, fmt.Errorf("seed the agent binary: %w", err)
	}
	if err := writeFile(filepath.Join(dir, alignFile), nil); err != nil {
		return "", false, err
	}
	if err := writeFile(filepath.Join(dir, seedFile), []byte(sum+"\n")); err != nil {
		return "", false, err
	}
	return bin, true, nil
}

// Boot hands a `run` of the image's own binary over to the state directory's
// binary and does not return when it succeeds. It returns without doing
// anything for a binary that is not the image's, one already handed over,
// or a run that is not a Kubernetes agent's; when the hand-over fails it
// sets UnavailableEnv and returns, and the image's binary runs on.
func Boot(args []string) {
	if os.Getenv(RevisionEnv) == "" || os.Getenv(BinaryEnv) != "" {
		return
	}
	stateDir, kube := kubeRun(args)
	if !kube || stateDir == "" {
		return
	}
	image, err := os.Executable()
	if err != nil {
		_ = os.Setenv(UnavailableEnv, err.Error())
		return
	}
	bin, _, err := Prepare(stateDir, image)
	if err != nil {
		_ = os.Setenv(UnavailableEnv, err.Error())
		return
	}
	argv := append([]string{bin}, args...)
	env := append(os.Environ(), BinaryEnv+"="+bin)
	if err := execFn(bin, argv, env); err != nil {
		// A state directory mounted noexec is the usual cause.
		_ = os.Setenv(UnavailableEnv, fmt.Sprintf("start %s: %v", bin, err))
	}
}

// Running reports whether this process runs as the state directory's binary
// Boot handed over to, so its upgrades replace that binary in place.
func Running() bool {
	want := os.Getenv(BinaryEnv)
	if want == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	for strings.HasSuffix(self, ".old") {
		self = strings.TrimSuffix(self, ".old")
	}
	return filepath.Clean(self) == filepath.Clean(want)
}

// TakeAlign reports whether the running binary was freshly seeded and has
// not yet taken the server's binary, and clears the mark: it is taken once
// per seed, so a server binary that fails and is rolled back is not taken
// again and again.
func TakeAlign() bool {
	if !Running() {
		return false
	}
	return takeAlign(os.Getenv(BinaryEnv))
}

func takeAlign(bin string) bool {
	return os.Remove(filepath.Join(filepath.Dir(bin), alignFile)) == nil
}

// kubeRun reads a `run`'s arguments: the state directory, and whether it is
// a Kubernetes agent's (--driver=kube, as the manifest writes it).
func kubeRun(args []string) (stateDir string, kube bool) {
	if len(args) == 0 || args[0] != "run" {
		return "", false
	}
	for i := 1; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if a == args[i] {
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		switch name {
		case "state":
			stateDir = value
		case "driver":
			kube = strings.EqualFold(value, "kube")
		}
	}
	return stateDir, kube
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".seed"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
