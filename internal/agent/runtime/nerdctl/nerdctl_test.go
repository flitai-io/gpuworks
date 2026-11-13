package nerdctl

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// installFakeNerdctl places a tiny executable first on PATH.  The production
// driver intentionally shells out to the CLI, so exercising the CLI boundary
// with a real process catches argument ordering, stream handling and error
// mapping without requiring containerd or a GPU.
func installFakeNerdctl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nerdctl")
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestDetectAndImageExistsUseGPUWorksNamespace(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$1 $2 $3\" in '-n gpuworks ps') exit 0;; '-n gpuworks image') exit 0;; esac")
	d := New()
	ok, err := d.Detect(context.Background())
	if err != nil || !ok {
		t.Fatalf("Detect=%v,%v", ok, err)
	}
	present, err := d.ImageExists(context.Background(), "img:test")
	if err != nil || !present {
		t.Fatalf("ImageExists=%v,%v", present, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "-n gpuworks ps") || !strings.Contains(string(data), "-n gpuworks image inspect img:test") {
		t.Fatalf("namespace was not pinned: %s", data)
	}
}

func TestInspectImageParsesJSONAndRunReportsErrors(t *testing.T) {
	inspect := `{"Image":{"Target":{"digest":"` + testTargetDigest + `"}},"ImageConfigDesc":{"digest":"` + testImageDigest + `"},"ImageConfig":{"config":{"Entrypoint":["vllm","serve"],"Cmd":["/model"],"Env":["A=B","EMPTY="],"Volumes":{"/cache":{}}}}}`
	installFakeNerdctl(t, "case \"$4\" in inspect) printf '%s\\n' '"+inspect+"';; esac; case \"$3\" in run) echo run-id;; esac")
	d := New()
	got, err := d.InspectImage(context.Background(), "img")
	if err != nil || got.ID != testImageDigest || got.Env["A"] != "B" || len(got.Volumes) != 1 {
		t.Fatalf("inspect=%+v err=%v", got, err)
	}
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "x", Image: "img"})
	if err != nil || id != "run-id" {
		t.Fatalf("run=%q err=%v", id, err)
	}
}

func TestInspectWorkloadUsesNamespaceAndReturnsImmutableIdentities(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	raw := `{"Id":"container-full-id","Image":"runner:v1","Path":"vllm","Args":["serve"],"Config":{"Entrypoint":[],"Cmd":[],"Env":[],"Labels":{"gpuworks.image-target":"` + testTargetDigest + `"}},"HostConfig":{"Runtime":"io.containerd.runc.v2","NetworkMode":"bridge"},"Mounts":[]}`
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'; case \"$3 $4\" in 'container inspect') printf '%s\\n' '"+raw+"';; esac")
	got, err := New().InspectWorkload(context.Background(), "gpuworks-name")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerID != "container-full-id" || got.ImageID != "" || got.ImageLookupRef != testTargetDigest || got.Env == nil || got.Mounts == nil {
		t.Fatalf("inspection=%+v", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(args)), "-n gpuworks container inspect --format") {
		t.Fatalf("namespace was not pinned: %s", args)
	}
}

func TestParseWorkloadInspectionRejectsMalformedAndMissingIdentity(t *testing.T) {
	for _, raw := range []string{`not-json`, `{"Id":"","Image":"sha256:x"}`, `{"Id":"id","Image":""}`} {
		if _, err := parseWorkloadInspection([]byte(raw)); err == nil {
			t.Fatalf("parse accepted %q", raw)
		}
	}
}

func TestInspectImageRejectsMalformedJSONAndStatusMapsNotFound(t *testing.T) {
	installFakeNerdctl(t, "case \"$4\" in inspect) printf 'not-json';; esac; echo 'no such object' >&2; exit 1")
	d := New()
	if _, err := d.InspectImage(context.Background(), "img"); err == nil {
		t.Fatal("malformed inspect unexpectedly succeeded")
	}
	// Status treats the CLI's not-found shape as an absent container.
	installFakeNerdctl(t, "echo 'no such object' >&2; exit 1")
	st, err := d.Status(context.Background(), "missing")
	if err != nil || st.Exists {
		t.Fatalf("status=%+v err=%v", st, err)
	}
}

func TestPullLoadSaveExecAndLogsPropagateStreams(t *testing.T) {
	installFakeNerdctl(t, "case \"$3\" in pull) echo layer-1 >&2; echo layer-2;; save) printf saved;; load) cat >/dev/null;; exec) echo exec-output;; logs) echo log-output;; esac")
	d := New()
	var lines []string
	if err := d.PullImage(context.Background(), "img", func(s string) { lines = append(lines, s) }); err != nil || len(lines) != 2 {
		t.Fatalf("pull lines=%v err=%v", lines, err)
	}
	var saved strings.Builder
	if err := d.SaveImage(context.Background(), "img", &saved); err != nil || saved.String() != "saved" {
		t.Fatalf("save=%q err=%v", saved.String(), err)
	}
	if err := d.LoadImage(context.Background(), "img", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	out, err := d.Exec(context.Background(), "id", []string{"echo", "x"})
	if err != nil || !strings.Contains(string(out), "exec-output") {
		t.Fatalf("exec=%q err=%v", out, err)
	}
	rc, err := d.Logs(context.Background(), "id", true, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if !strings.Contains(string(b), "log-output") {
		t.Fatalf("logs=%q", b)
	}
}

func TestExecPTYBuildsNamespacedAttachCommand(t *testing.T) {
	d := New()
	argv, err := d.ExecPTY(context.Background(), "container-123", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nerdctl", "-n", Namespace, "exec", "-i", "-t", "container-123", "/bin/sh", "-l"}
	if !slices.Equal(argv, want) {
		t.Fatalf("ExecPTY argv=%v, want %v", argv, want)
	}
	if _, ok := any(d).(runtime.PTYExecer); !ok {
		t.Fatal("nerdctl driver does not advertise PTYExecer")
	}
	if _, err := d.ExecPTY(context.Background(), " ", nil); err == nil {
		t.Fatal("ExecPTY accepted an empty container id")
	}
}

func TestPullReturnsWhenHelperInheritsProgressPipe(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "helper.ready")
	releaseFile := filepath.Join(dir, "helper.release")
	// The helper must hold the inherited pipe until PullImage returns, but it
	// must not be immortal: a SIGKILLed test binary (CI runner killing the job
	// process tree) never runs t.Cleanup, so the wait loop caps its own
	// lifetime at ~30s — well past every deadline inside this test — instead
	// of looping until the release file appears.
	installFakeNerdctl(t, "case \"$3\" in pull) (echo ready > '"+readyFile+"'; n=0; while [ ! -e '"+releaseFile+"' ] && [ \"$n\" -lt 150 ]; do n=$((n+1)); sleep 0.2; done) & echo layer-ready; exit 0;; esac")
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, nil, 0o600)
	})

	done := make(chan error, 1)
	go func() { done <- New().PullImage(context.Background(), "img", nil) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake nerdctl helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PullImage returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PullImage waited for an inherited progress pipe after the nerdctl process exited")
	}
}

func TestStatusParsesExitCodeAndRemoveIgnoresMissing(t *testing.T) {
	installFakeNerdctl(t, "case \"$3\" in inspect) echo 'false|17';; rm) echo 'not found' >&2; exit 1;; esac")
	d := New()
	st, err := d.Status(context.Background(), "id")
	if err != nil || !st.Exists || st.Running || st.ExitCode != 17 {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	if err := d.Remove(context.Background(), "id"); err != nil {
		t.Fatalf("remove missing=%v", err)
	}
}

// A container that exits stays inspectable with its code; only the CLI's own
// "no such object" answer means absent, and nothing else may be guessed into a
// stopped container, since the engine retires a running instance on that.
func TestStatusReportsOnlyWhatTheCLIProves(t *testing.T) {
	installFakeNerdctl(t, "case \"$3\" in inspect) printf '%s' \"$NERDCTL_INSPECT_OUTPUT\"; [ -z \"${NERDCTL_INSPECT_FAIL:-}\" ] || exit 1;; esac")
	d := New()
	for _, tc := range []struct {
		name, out string
		want      runtime.Status
	}{
		{name: "running", out: "true|0\n", want: runtime.Status{Exists: true, Running: true}},
		{name: "exited zero", out: "false|0\n", want: runtime.Status{Exists: true}},
		{name: "killed", out: "false|137\n", want: runtime.Status{Exists: true, ExitCode: 137}},
		{name: "warning first", out: "time=x level=warning msg=\"cgroup v1\"\nfalse|3\n", want: runtime.Status{Exists: true, ExitCode: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NERDCTL_INSPECT_OUTPUT", tc.out)
			got, err := d.Status(context.Background(), "id")
			if err != nil || got != tc.want {
				t.Fatalf("Status = %+v, err=%v; want %+v", got, err, tc.want)
			}
		})
	}
	for _, out := range []string{"", "true", "running|0", "false|"} {
		t.Setenv("NERDCTL_INSPECT_OUTPUT", out)
		if got, err := d.Status(context.Background(), "id"); err == nil {
			t.Fatalf("Status(%q) = %+v, want an error", out, got)
		}
	}
	t.Setenv("NERDCTL_INSPECT_FAIL", "1")
	t.Setenv("NERDCTL_INSPECT_OUTPUT", "time=x level=fatal msg=\"no such object id\"\n")
	if got, err := d.Status(context.Background(), "id"); err != nil || got.Exists {
		t.Fatalf("missing Status = %+v, err=%v", got, err)
	}
	t.Setenv("NERDCTL_INSPECT_OUTPUT", "time=x level=fatal msg=\"cannot access containerd socket\"\n")
	if got, err := d.Status(context.Background(), "id"); err == nil {
		t.Fatalf("daemon failure Status = %+v, want an error", got)
	}
	// A missing binary says "executable file not found", which is not the
	// container being absent.
	t.Setenv("PATH", t.TempDir())
	if got, err := d.Status(context.Background(), "id"); err == nil || got.Exists {
		t.Fatalf("Status without nerdctl = %+v, err=%v; want an error", got, err)
	}
}

func TestInspectImageAcceptsMultipleObjectsFirstLine(t *testing.T) {
	raw := map[string]any{"Image": map[string]any{"Target": map[string]any{"digest": testImageDigest}}, "ImageConfigDesc": map[string]any{"digest": testImageDigest}, "ImageConfig": map[string]any{}}
	b, _ := json.Marshal(raw)
	installFakeNerdctl(t, "case \"$4\" in inspect) printf '%s\\nsecond\\n' '"+string(b)+"';; esac")
	got, err := New().InspectImage(context.Background(), "img")
	if err != nil || got.ID != testImageDigest {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestBuildRunArgsPinsNamespaceFirst(t *testing.T) {
	args := New().buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-test", Image: "fake"})
	if len(args) < 2 || args[0] != "-n" || args[1] != Namespace {
		t.Fatalf("nerdctl args must start with -n %s, got %v", Namespace, args)
	}
}

func TestBuildRunArgsOverrideOfficialImageEntrypoints(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		entrypoint []string
		cmd        []string
	}{
		{"gpustack-vllm", "quay.io/gpustack/runner:cuda13.0-vllm0.25.1", []string{"vllm", "serve", "/model"}, []string{"--port", "30017"}},
		{"upstream-vllm", "vllm/vllm-openai:latest", []string{"vllm", "serve", "/model"}, []string{"--port", "30017"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := New().buildRunArgs(runtime.WorkloadSpec{
				Name: tt.name, Image: tt.image, Entrypoint: tt.entrypoint, Cmd: tt.cmd,
			})
			assertContainsPair(t, args, "--entrypoint", tt.entrypoint[0])
			imageIndex := slices.Index(args, tt.image)
			if imageIndex < 0 {
				t.Fatalf("image missing from nerdctl args: %v", args)
			}
			wantTail := append([]string{}, tt.entrypoint[1:]...)
			wantTail = append(wantTail, tt.cmd...)
			if !slices.Equal(args[imageIndex+1:], wantTail) {
				t.Fatalf("nerdctl command tail for %s = %v, want %v", tt.image, args[imageIndex+1:], wantTail)
			}
		})
	}
}

func TestBuildRunArgsUsesInjectedDockerArgsAndEnv(t *testing.T) {
	args := New().buildRunArgs(runtime.WorkloadSpec{
		Name:       "gpuworks-test",
		Image:      "vllm/vllm-openai:latest",
		DockerArgs: []string{"--gpus", "device=2,4"},
		Env:        map[string]string{"CUDA_VISIBLE_DEVICES": "2,4"},
	})

	assertContainsPair(t, args, "--gpus", "device=2,4")
	assertContainsPair(t, args, "-e", "CUDA_VISIBLE_DEVICES=2,4")
}

func TestBuildRunArgsHostNetworkAlwaysUsesHostFlag(t *testing.T) {
	// Unlike the docker driver, nerdctl only targets Linux hosts (no Docker
	// Desktop VM indirection), so --network host is unconditional.
	args := New().buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-host", Image: "fake", HostNetwork: true, Ports: []int{30000}})
	assertContainsPair(t, args, "--network", "host")
	assertNotContains(t, args, "-p")
}

func TestBuildRunArgsOmitsRestartPolicy(t *testing.T) {
	// nerdctl has no persistent daemon behind it; --restart is meaningless
	// there, unlike for the docker driver which always sets --restart=no.
	args := New().buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-test", Image: "fake"})
	assertNotContains(t, args, "--restart")
}

func assertContainsPair(t *testing.T, args []string, wantKey, wantValue string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == wantKey && args[i+1] == wantValue {
			return
		}
	}
	t.Fatalf("args missing pair %q %q: %+v", wantKey, wantValue, args)
}

func assertNotContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			t.Fatalf("args unexpectedly contain %q: %+v", want, args)
		}
	}
}

func TestListContainersUsesGPUWorksNamespace(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\ncase \"$1 $2 $3 $4\" in '-n gpuworks ps -a') printf 'gpuworks-11111111-222\\n';; esac")
	names, err := New().ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if want := []string{"gpuworks-11111111-222"}; !slices.Equal(names, want) {
		t.Fatalf("ListContainers = %v, want %v", names, want)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "-n gpuworks ps -a --format {{.Names}}" {
		t.Fatalf("nerdctl argv = %q, want the namespace-scoped -a listing", got)
	}
}

func TestRunningContainersListsOnlyWhatNerdctlPsShowsAsUp(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\ncase \"$*\" in '-n gpuworks ps -a '*) printf 'gpuworks-up\\ngpuworks-exited\\n';; '-n gpuworks ps '*) printf 'gpuworks-up\\n';; esac")
	names, err := New().RunningContainers(context.Background())
	if err != nil {
		t.Fatalf("RunningContainers: %v", err)
	}
	if want := []string{"gpuworks-up"}; !slices.Equal(names, want) {
		t.Fatalf("RunningContainers = %v, want %v", names, want)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "-n gpuworks ps --format {{.Names}}" {
		t.Fatalf("nerdctl argv = %q, want the namespace-scoped listing without -a", got)
	}
}

func TestRunningContainersIsEmptyWhenNothingRunsInTheNamespace(t *testing.T) {
	installFakeNerdctl(t, "exit 0")
	names, err := New().RunningContainers(context.Background())
	if err != nil || len(names) != 0 {
		t.Fatalf("RunningContainers = %v, err=%v, want an empty listing", names, err)
	}
}

func TestRunningContainersPropagatesNerdctlFailure(t *testing.T) {
	installFakeNerdctl(t, "echo 'containerd is not running' >&2\nexit 1")
	names, err := New().RunningContainers(context.Background())
	if err == nil {
		t.Fatalf("RunningContainers = %v, want an error when containerd is unreachable", names)
	}
	if names != nil {
		t.Fatalf("RunningContainers returned names alongside a failure: %v", names)
	}
}

func TestListContainersPropagatesNerdctlFailure(t *testing.T) {
	installFakeNerdctl(t, "echo 'containerd is not running' >&2\nexit 1")
	names, err := New().ListContainers(context.Background())
	if err == nil {
		t.Fatalf("ListContainers = %v, want an error when containerd is unreachable", names)
	}
	if names != nil {
		t.Fatalf("ListContainers returned names alongside a failure: %v", names)
	}
}

func TestBuildRunArgsExpandsDeviceDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"rdma_cm", "uverbs0", "uverbs1"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "by-path"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := New().buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-rdma", Image: "fake", DockerArgs: []string{
		"--device", dir, "--device=" + dir + ":/dev/ib:rw", "--device", "/dev/davinci3", "--ulimit", "memlock=-1",
	}})
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--device " + dir + "/rdma_cm --device " + dir + "/uverbs0 --device " + dir + "/uverbs1",
		"--device " + dir + "/uverbs1:/dev/ib/uverbs1:rw",
		"--device /dev/davinci3", "--ulimit memlock=-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args = %s\nmissing %q", got, want)
		}
	}
	if strings.Contains(got, "--device "+dir+" ") || strings.Contains(got, "by-path") {
		t.Fatalf("directory passed through or subdirectory expanded: %s", got)
	}
}

func TestNewInNamespacePinsEveryCommandToThatNamespace(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" >> '"+argsFile+"'\nexit 0")
	d := NewInNamespace("k8s.io")
	if _, err := d.ListContainers(context.Background()); err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if err := d.TagImage(context.Background(), "a:1", "b:1"); err != nil {
		t.Fatalf("TagImage: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		if !strings.HasPrefix(line, "-n k8s.io ") {
			t.Fatalf("nerdctl argv %q is not pinned to k8s.io", line)
		}
	}
}
