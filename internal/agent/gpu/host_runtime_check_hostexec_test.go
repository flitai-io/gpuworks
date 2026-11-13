package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// hostWith is an agent that runs the vendor's CLIs on the host (a Kubernetes
// DaemonSet): the host's root holds the given files, and nsenter, which runs
// the command in the host's mount namespace, is a script that records what it
// was asked to run and the environment it passes on.
func hostWith(t *testing.T, files ...string) (log string) {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot := hostRoot
	hostRoot = root
	cliOnHost.Store(true)
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })

	bin, logDir := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + logDir + "/args'\nenv > '" + logDir + "/env'\n" + `printf '%s\n%s\n%s\n' CUDA_RUNTIME_SMOKE_OK ASCEND_ACL_SMOKE_OK HYGON_HIP_SMOKE_OK` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "nsenter"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logDir
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("nsenter was not run: %v", err)
	}
	return string(raw)
}

// The agent image has no python3. The per-card check finds the host's and runs
// it through nsenter, with the card selection in the environment it passes on.
func TestRuntimeChecksRunPythonOnTheHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   func(context.Context) error
		wants []string // lines expected in the passed environment
	}{
		{"cuda", func(ctx context.Context) error { return CUDARuntimeSmokeOn(ctx, "GPU-0123") }, []string{"CUDA_VISIBLE_DEVICES=GPU-0123"}},
		{"ascend", func(ctx context.Context) error {
			out, err := runAscendSmokeCommand(ctx, "3", ascendSmokeTarget{Home: "/usr/local/Ascend/ascend-toolkit/latest", Library: "/usr/local/Ascend/ascend-toolkit/latest/lib64/libascendcl.so"})
			return ascendSmokeVerdict(out, err)
		}, []string{"ASCEND_RT_VISIBLE_DEVICES=3", "ASCEND_HOME_PATH=/usr/local/Ascend/ascend-toolkit/latest"}},
		{"hygon", func(ctx context.Context) error {
			out, err := runHygonSmokeCommand(ctx, "/opt/dtk/lib/libamdhip64.so", "0000:3b:00.0")
			return hygonSmokeVerdict(out, err)
		}, []string{"HYGON_SMOKE_BDF=0000:3b:00.0", "HYGON_HIP_LIBRARY=/opt/dtk/lib/libamdhip64.so"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := hostWith(t, "/usr/bin/python3")
			if err := tc.run(context.Background()); err != nil {
				t.Fatalf("check = %v", err)
			}
			args := strings.Split(strings.TrimSpace(readLog(t, logs, "args")), "\n")
			if len(args) < 6 || strings.Join(args[:5], " ") != "-t 1 -m -- /usr/bin/python3" || args[5] != "-c" {
				t.Fatalf("nsenter args = %q", args)
			}
			env := readLog(t, logs, "env")
			for _, want := range tc.wants {
				if !strings.Contains("\n"+env, "\n"+want+"\n") {
					t.Fatalf("%s is not in the environment passed through nsenter:\n%s", want, env)
				}
			}
		})
	}
}

// Off the host, nothing changes: python3 is run directly.
func TestRuntimeCheckRunsPythonDirectlyOffTheHost(t *testing.T) {
	bin, logDir := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + logDir + "/args'\necho CUDA_RUNTIME_SMOKE_OK\n"
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := CUDARuntimeSmokeOn(context.Background(), "GPU-1"); err != nil {
		t.Fatal(err)
	}
	if args := readLog(t, logDir, "args"); !strings.HasPrefix(args, "-c\n") {
		t.Fatalf("python3 args = %q", args)
	}
}

// The prerequisites look where the check runs: python3 and the vendor's
// library on the host, not in the agent image.
func TestRuntimeCheckPrerequisitesLookOnTheHost(t *testing.T) {
	// The agent image has none of it.
	t.Setenv("PATH", t.TempDir())

	hostWith(t, "/usr/bin/python3", "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/usr/local/Ascend/cann-8.0.0/lib64/libascendcl.so")
	if reason := CUDASmokePrerequisite(); reason != "" {
		t.Fatalf("CUDA prerequisite on a host with python3 and libcuda = %q", reason)
	}
	if reason := AscendCLSmokePrerequisite(); reason != "" {
		t.Fatalf("Ascend prerequisite on a host with CANN = %q", reason)
	}
	target, ok := findAscendSmokeTarget()
	if !ok || target.Home != "/usr/local/Ascend/cann-8.0.0" || target.Library != "/usr/local/Ascend/cann-8.0.0/lib64/libascendcl.so" {
		t.Fatalf("Ascend target = %+v, %v (a host path, not prefixed with the host root)", target, ok)
	}
	// No Hygon library on this host.
	if reason := HygonHIPSmokePrerequisite(); !strings.Contains(reason, "libamdhip64.so") {
		t.Fatalf("Hygon prerequisite without DTK = %q", reason)
	}

	// Without python3 on the host the reason says so.
	hostWith(t, "/usr/lib/x86_64-linux-gnu/libcuda.so.1")
	if reason := CUDASmokePrerequisite(); !strings.Contains(reason, "python3") {
		t.Fatalf("CUDA prerequisite without host python3 = %q", reason)
	}
	// Hygon with DTK on the host.
	hostWith(t, "/usr/bin/python3", strings.TrimPrefix(accelerator.HygonHIPLibraryPaths[0], "/"))
	if reason := HygonHIPSmokePrerequisite(); reason != "" {
		t.Fatalf("Hygon prerequisite with DTK = %q", reason)
	}
}
