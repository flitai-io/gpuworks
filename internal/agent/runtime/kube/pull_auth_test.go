package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// installNerdctl puts a fake nerdctl first in PATH; it stands in for the
// nsenter wrapper the real runtime installs, which passes the environment on.
func installNerdctl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nerdctl"), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestPullLoginIsStagedWhereTheHostSeesIt: nerdctl runs in the node's mount
// namespace, so the private docker config of a pull with a registry login
// must be under the agent's state directory (a hostPath at the same path on
// the node), not in the container's /tmp; and it is gone after the pull,
// failed or not.
func TestPullLoginIsStagedWhereTheHostSeesIt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		stateDir := t.TempDir()
		logDir := t.TempDir()
		t.Setenv("LOG", logDir)
		exit := "0"
		if fail {
			exit = "1"
		}
		installNerdctl(t, `echo "$DOCKER_CONFIG" > "$LOG/config_dir"
cat "$DOCKER_CONFIG/config.json" > "$LOG/config_json"
stat -c %a "$DOCKER_CONFIG" > "$LOG/mode"
stat -c %a "$(dirname "$DOCKER_CONFIG")" > "$LOG/parent_mode"
exit `+exit)
		d := New(Options{Images: newImageStore(stateDir)})
		ctx := runtime.WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "registry.example", Username: "robot", Secret: "tok3n"}})
		err := d.PullImage(ctx, "registry.example/model:v1", nil)
		if (err != nil) != fail {
			t.Fatalf("fail=%v err=%v", fail, err)
		}
		read := func(name string) string {
			raw, _ := os.ReadFile(filepath.Join(logDir, name))
			return strings.TrimSpace(string(raw))
		}
		dir := read("config_dir")
		if want := filepath.Join(stateDir, "kube-tools", "pull-auth"); filepath.Dir(dir) != want {
			t.Fatalf("DOCKER_CONFIG = %q, want a directory under %s", dir, want)
		}
		if read("mode") != "700" || read("parent_mode") != "700" {
			t.Fatalf("modes: dir %s, parent %s; want 700", read("mode"), read("parent_mode"))
		}
		if !strings.Contains(read("config_json"), `"registry.example"`) {
			t.Fatalf("config.json = %s", read("config_json"))
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("the login directory survived the pull (fail=%v)", fail)
		}
	}
}

func TestPullWithoutALoginStagesNothing(t *testing.T) {
	stateDir := t.TempDir()
	installNerdctl(t, `[ -z "${DOCKER_CONFIG:-}" ]`)
	d := New(Options{Images: newImageStore(stateDir)})
	if err := d.PullImage(context.Background(), "registry.example/model:v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "kube-tools")); !os.IsNotExist(err) {
		t.Fatalf("a login-less pull created %v", err)
	}
}
