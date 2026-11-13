package kubeboot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The first start seeds the state directory from the image; later starts of
// the same image keep whatever the directory holds, an upgraded binary and
// its pending upgrade included; a different image seeds it again.
func TestPrepareSeedsOnceAndAgainForAnotherImage(t *testing.T) {
	state := t.TempDir()
	image := filepath.Join(t.TempDir(), "gpuworks-agent")
	write(t, image, "image r1 agent")

	bin, fresh, err := Prepare(state, image)
	if err != nil || !fresh {
		t.Fatalf("first start: fresh=%v err=%v", fresh, err)
	}
	if bin != filepath.Join(state, "agent", "gpuworks-agent") || read(t, bin) != "image r1 agent" {
		t.Fatalf("seeded %s = %q", bin, read(t, bin))
	}
	if info, _ := os.Stat(bin); info.Mode()&0o111 == 0 {
		t.Fatalf("seeded binary is not executable: %v", info.Mode())
	}
	if !exists(filepath.Join(state, "agent", alignFile)) {
		t.Fatal("a fresh seed is not marked to take the server's binary")
	}

	// The platform upgrades the binary in place, and the upgrade is pending.
	write(t, bin, "server agent")
	write(t, bin+".old", "image r1 agent")
	write(t, bin+".upgrade-pending", "{}")
	_ = os.Remove(filepath.Join(state, "agent", alignFile))
	again, fresh, err := Prepare(state, image)
	if err != nil || fresh || again != bin {
		t.Fatalf("restart of the same image: bin=%s fresh=%v err=%v", again, fresh, err)
	}
	if read(t, bin) != "server agent" || !exists(bin+".old") || !exists(bin+".upgrade-pending") {
		t.Fatal("a restart of the same image replaced the upgraded binary or its pending upgrade")
	}

	// The operator moves the DaemonSet to another image.
	write(t, image, "image r2 agent")
	if _, fresh, err := Prepare(state, image); err != nil || !fresh {
		t.Fatalf("new image: fresh=%v err=%v", fresh, err)
	}
	if read(t, bin) != "image r2 agent" {
		t.Fatalf("after a new image the binary is %q", read(t, bin))
	}
	for _, stale := range []string{".old", ".upgrade-pending"} {
		if exists(bin + stale) {
			t.Fatalf("%s of the replaced binary survived the new seed", stale)
		}
	}
}

// A binary lost between the renames of an upgrade is seeded again.
func TestPrepareReseedsAMissingBinary(t *testing.T) {
	state := t.TempDir()
	image := filepath.Join(t.TempDir(), "gpuworks-agent")
	write(t, image, "image agent")
	bin, _, err := Prepare(state, image)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := Prepare(state, image); err != nil || !fresh || read(t, bin) != "image agent" {
		t.Fatalf("missing binary: fresh=%v err=%v", fresh, err)
	}
}

func TestPrepareFailsWithoutTheImagesBinaryOrAWritableDirectory(t *testing.T) {
	if _, _, err := Prepare(t.TempDir(), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("no error without the image's binary")
	}
	image := filepath.Join(t.TempDir(), "gpuworks-agent")
	write(t, image, "image agent")
	blocked := filepath.Join(t.TempDir(), "state")
	write(t, blocked, "a file where the state directory should be")
	if _, _, err := Prepare(blocked, image); err == nil {
		t.Fatal("no error when the state directory cannot hold the binary")
	}
}

func TestKubeRunReadsTheManifestsArguments(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		state string
		kube  bool
	}{
		{[]string{"run", "--server=https://gw", "--state=/opt/gpuworks", "--driver=kube"}, "/opt/gpuworks", true},
		{[]string{"run", "--state", "/opt/gw", "--driver", "kube"}, "/opt/gw", true},
		{[]string{"run", "--state=/opt/gpuworks"}, "/opt/gpuworks", false},
		{[]string{"hold", "--state=/opt/gpuworks", "--driver=kube"}, "", false},
		{nil, "", false},
	} {
		state, kube := kubeRun(tc.args)
		if state != tc.state || kube != tc.kube {
			t.Errorf("kubeRun(%q) = %q %v, want %q %v", tc.args, state, kube, tc.state, tc.kube)
		}
	}
}

func stubExec(t *testing.T, fail error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := execFn
	execFn = func(bin string, argv, env []string) error {
		calls = append(calls, append([]string{bin}, argv...))
		for _, e := range env {
			if e == BinaryEnv+"="+bin {
				return fail
			}
		}
		t.Errorf("exec of %s without %s", bin, BinaryEnv)
		return fail
	}
	t.Cleanup(func() { execFn = prev })
	return &calls
}

// Boot hands the image's own run over to the state directory's binary, with
// the run's arguments; anything else is left to run as it is.
func TestBootHandsOverOnlyTheImagesKubernetesRun(t *testing.T) {
	state := t.TempDir()
	args := []string{"run", "--server=https://gw", "--state=" + state, "--driver=kube"}
	t.Run("not the image", func(t *testing.T) {
		t.Setenv(RevisionEnv, "")
		calls := stubExec(t, nil)
		Boot(args)
		if len(*calls) != 0 || exists(filepath.Join(state, Dir)) {
			t.Fatal("a binary outside the image handed over")
		}
	})
	t.Run("already handed over", func(t *testing.T) {
		t.Setenv(RevisionEnv, "r1")
		t.Setenv(BinaryEnv, "/opt/gpuworks/agent/gpuworks-agent")
		calls := stubExec(t, nil)
		Boot(args)
		if len(*calls) != 0 {
			t.Fatal("the handed-over binary handed over again")
		}
	})
	t.Run("not a kubernetes run", func(t *testing.T) {
		t.Setenv(RevisionEnv, "r1")
		t.Setenv(BinaryEnv, "")
		calls := stubExec(t, nil)
		Boot([]string{"hold", "--state=" + state})
		if len(*calls) != 0 {
			t.Fatal("a hold pod handed over")
		}
	})
	t.Run("hands over", func(t *testing.T) {
		t.Setenv(RevisionEnv, "r1")
		t.Setenv(BinaryEnv, "")
		t.Setenv(UnavailableEnv, "")
		calls := stubExec(t, nil)
		Boot(args)
		bin := filepath.Join(state, Dir, "gpuworks-agent")
		want := append([]string{bin, bin}, args...)
		if len(*calls) != 1 || len((*calls)[0]) != len(want) {
			t.Fatalf("exec calls = %q, want %q", *calls, want)
		}
		for i := range want {
			if (*calls)[0][i] != want[i] {
				t.Fatalf("exec calls = %q, want %q", *calls, want)
			}
		}
	})
	t.Run("exec refused", func(t *testing.T) {
		t.Setenv(RevisionEnv, "r1")
		t.Setenv(BinaryEnv, "")
		t.Setenv(UnavailableEnv, "")
		stubExec(t, errors.New("permission denied"))
		Boot(args)
		if os.Getenv(UnavailableEnv) == "" {
			t.Fatal("a refused hand-over left no reason for the agent to report")
		}
	})
}

func TestRunningAndTakeAlign(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, "")
	if Running() || TakeAlign() {
		t.Fatal("a process that was not handed over claims to be")
	}
	t.Setenv(BinaryEnv, filepath.Join(t.TempDir(), "gpuworks-agent"))
	if Running() {
		t.Fatal("a process running another binary claims to be the state directory's")
	}
	t.Setenv(BinaryEnv, self)
	if !Running() {
		t.Fatal("the handed-over binary does not know it")
	}
}

func TestTheServersBinaryIsTakenOncePerSeed(t *testing.T) {
	state := t.TempDir()
	image := filepath.Join(t.TempDir(), "gpuworks-agent")
	write(t, image, "image agent")
	bin, _, err := Prepare(state, image)
	if err != nil {
		t.Fatal(err)
	}
	if !takeAlign(bin) {
		t.Fatal("a fresh seed is not taken")
	}
	if takeAlign(bin) {
		t.Fatal("the server's binary is taken twice for one seed")
	}
	if _, _, err := Prepare(state, image); err != nil || takeAlign(bin) {
		t.Fatalf("a restart of the same image asks for the server's binary again (err %v)", err)
	}
}
