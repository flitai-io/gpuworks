package docker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	hostruntime "runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/ptyio"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestLocalDockerHostRejectsRemoteAndFindsUnixSocket(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	if host, ok := localDockerHost(); ok || host != "tcp://127.0.0.1:2375" {
		t.Fatalf("remote docker host = %q,%t", host, ok)
	}
	sock := shortUnixSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	if host, ok := localDockerHost(); !ok || host != sock {
		t.Fatalf("unix docker host = %q,%t", host, ok)
	}
}

func TestDetectRequiresLocalSocketBeforeDockerInfo(t *testing.T) {
	installFakeDocker(t, "[ \"$1\" = info ] && exit 0; exit 1")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "missing.sock"))
	if ok, err := New().Detect(context.Background()); err != nil || ok {
		t.Fatalf("missing socket detect = %t,%v", ok, err)
	}
	sock := shortUnixSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	if ok, err := New().Detect(context.Background()); err != nil || !ok {
		t.Fatalf("local socket detect = %t,%v", ok, err)
	}
}

func TestInspectWorkloadReturnsImmutableIdentitiesAndResolvedConfig(t *testing.T) {
	raw := `{"Id":"container-full-id","Image":"sha256:image-old","Path":"python","Args":["-m","serve"],"Config":{"Entrypoint":[],"Cmd":[],"Env":["LOG_LEVEL=info","HF_TOKEN=secret"]},"HostConfig":{"Runtime":"nvidia","NetworkMode":"host","Devices":[{"PathOnHost":"/dev/nvidia0","PathInContainer":"/dev/nvidia0","CgroupPermissions":"rwm"}],"DeviceRequests":[{"Driver":"nvidia","Count":1}]},"Mounts":[{"Type":"bind","Source":"/models/a","Destination":"/model","RW":false}]}`
	installFakeDocker(t, "case \"$1 $2\" in 'container inspect') printf '%s\\n' '"+raw+"';; esac")
	got, err := New().InspectWorkload(context.Background(), "gpuworks-name")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerID != "container-full-id" || got.ImageID != "sha256:image-old" {
		t.Fatalf("identities=%q,%q", got.ContainerID, got.ImageID)
	}
	if got.Path != "python" || !slices.Equal(got.Args, []string{"-m", "serve"}) || got.Env["HF_TOKEN"] != "secret" {
		t.Fatalf("resolved config=%+v", got)
	}
	if got.Entrypoint == nil || got.Cmd == nil || len(got.Mounts) != 1 || !got.Mounts[0].ReadOnly || len(got.Devices) != 2 {
		t.Fatalf("empty/default and runtime fields=%+v", got)
	}
}

func TestParseWorkloadInspectionRejectsMalformedAndMissingIdentity(t *testing.T) {
	for _, raw := range []string{`not-json`, `{"Id":"","Image":"sha256:x"}`, `{"Id":"id","Image":""}`} {
		if _, err := parseWorkloadInspection([]byte(raw)); err == nil {
			t.Fatalf("parse accepted %q", raw)
		}
	}
}

func shortUnixSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gwdock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "docker.sock")
}

func installFakeDocker(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	installFakeDocker(t, "case \"$1\" in pull) (echo ready > '"+readyFile+"'; n=0; while [ ! -e '"+releaseFile+"' ] && [ \"$n\" -lt 150 ]; do n=$((n+1)); sleep 0.2; done) & echo layer-ready; exit 0;; esac")
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
			t.Fatal("fake docker helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PullImage returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PullImage waited for an inherited progress pipe after the docker process exited")
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
		{"gpustack-sglang", "quay.io/gpustack/runner:cuda13.0-sglang0.5.15.post1", []string{"python", "-m", "sglang.launch_server"}, []string{"--model-path", "/model"}},
		{"upstream-sglang", "lmsysorg/sglang:latest", []string{"python", "-m", "sglang.launch_server"}, []string{"--model-path", "/model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildRunArgs(runtime.WorkloadSpec{
				Name: tt.name, Image: tt.image, Entrypoint: tt.entrypoint, Cmd: tt.cmd,
			})
			assertContainsPair(t, args, "--entrypoint", tt.entrypoint[0])
			imageIndex := slices.Index(args, tt.image)
			if imageIndex < 0 {
				t.Fatalf("image missing from docker args: %v", args)
			}
			wantTail := append([]string{}, tt.entrypoint[1:]...)
			wantTail = append(wantTail, tt.cmd...)
			if !slices.Equal(args[imageIndex+1:], wantTail) {
				t.Fatalf("docker command tail for %s = %v, want %v", tt.image, args[imageIndex+1:], wantTail)
			}
		})
	}
}

func TestBuildRunArgsUsesInjectedNVIDIADockerArgs(t *testing.T) {
	args := buildRunArgs(runtime.WorkloadSpec{
		Name:       "gpuworks-test",
		Image:      "vllm/vllm-openai:latest",
		DockerArgs: []string{"--gpus", "device=2,4"},
		Env:        map[string]string{"CUDA_VISIBLE_DEVICES": "2,4"},
	})

	assertContainsPair(t, args, "--gpus", "device=2,4")
	assertContainsPair(t, args, "-e", "CUDA_VISIBLE_DEVICES=2,4")
}

func TestBuildRunArgsUsesInjectedAscendDockerArgs(t *testing.T) {
	args := buildRunArgs(runtime.WorkloadSpec{
		Name:       "gpuworks-test",
		Image:      "quay.io/ascend/vllm-ascend:latest",
		DockerArgs: []string{"--runtime", "ascend"},
		Env:        map[string]string{"ASCEND_RT_VISIBLE_DEVICES": "2,4"},
	})

	assertContainsPair(t, args, "--runtime", "ascend")
	assertContainsPair(t, args, "-e", "ASCEND_RT_VISIBLE_DEVICES=2,4")
	assertNotContains(t, args, "--gpus")
}

func TestBuildRunArgsMakesHostNetworkReachableOnDockerDesktop(t *testing.T) {
	args := buildRunArgs(runtime.WorkloadSpec{Name: "gpuworks-host", Image: "fake", HostNetwork: true, Ports: []int{30000}})
	if hostruntime.GOOS == "linux" {
		assertContainsPair(t, args, "--network", "host")
		return
	}
	assertContainsPair(t, args, "-p", "127.0.0.1:30000:30000")
	assertNotContains(t, args, "host")
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

func TestListContainersParsesFakeDockerCLI(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeDocker(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\ncase \"$1\" in ps) printf 'gpuworks-11111111-222\\nunmanaged\\n';; esac")
	d := New()
	names, err := d.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	want := []string{"gpuworks-11111111-222", "unmanaged"}
	if !slices.Equal(names, want) {
		t.Fatalf("ListContainers = %v, want %v", names, want)
	}
	// The one-VM-two-logical-nodes wrapper scopes this exact invocation to a
	// single logical node, so the argv is part of the contract.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "ps -a --format {{.Names}}" {
		t.Fatalf("docker argv = %q, want the -a listing with names", got)
	}
}

// The fake answers the two listings the way docker does (checked against
// docker 29.8.1): `ps` without -a names running and paused containers, which
// inspect both reports as Running, and leaves exited ones to `ps -a`.
func TestRunningContainersListsOnlyWhatDockerPsShowsAsUp(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeDocker(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'\ncase \"$*\" in 'ps -a '*) printf 'gpuworks-up\\ngpuworks-paused\\ngpuworks-exited\\n';; 'ps '*) printf 'gpuworks-paused\\ngpuworks-up\\n';; esac")
	names, err := New().RunningContainers(context.Background())
	if err != nil {
		t.Fatalf("RunningContainers: %v", err)
	}
	if want := []string{"gpuworks-paused", "gpuworks-up"}; !slices.Equal(names, want) {
		t.Fatalf("RunningContainers = %v, want %v", names, want)
	}
	// Starting with `ps` keeps the listing inside the logical node the
	// one-VM-two-logical-nodes wrapper scopes that subcommand to.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "ps --format {{.Names}}" {
		t.Fatalf("docker argv = %q, want the listing without -a", got)
	}
}

func TestRunningContainersIsEmptyWhenNothingRuns(t *testing.T) {
	installFakeDocker(t, "exit 0")
	names, err := New().RunningContainers(context.Background())
	if err != nil || len(names) != 0 {
		t.Fatalf("RunningContainers = %v, err=%v, want an empty listing", names, err)
	}
}

func TestRunningContainersPropagatesFakeDockerFailure(t *testing.T) {
	installFakeDocker(t, "case \"$1\" in ps) echo 'Cannot connect to the Docker daemon' >&2; exit 1;; esac")
	names, err := New().RunningContainers(context.Background())
	if err == nil {
		t.Fatalf("RunningContainers = %v, want an error when the daemon is unreachable", names)
	}
	if names != nil {
		t.Fatalf("RunningContainers returned names alongside a failure: %v", names)
	}
}

func TestListContainersPropagatesFakeDockerFailure(t *testing.T) {
	installFakeDocker(t, "case \"$1\" in ps) echo 'Cannot connect to the Docker daemon' >&2; exit 1;; esac")
	names, err := New().ListContainers(context.Background())
	if err == nil {
		t.Fatalf("ListContainers = %v, want an error when the daemon is unreachable", names)
	}
	if names != nil {
		t.Fatalf("ListContainers returned names alongside a failure: %v", names)
	}
}

// docker pull prints byte counters only to a terminal, so the pull runs on
// one, wide enough for the counters; the redraw control sequences are removed
// and a carriage return ends a line, so the puller sees one clean line per
// update with the bytes in it.
func TestPullRunsOnATerminalAndReportsCleanByteCounterLines(t *testing.T) {
	if !ptyio.Supported() {
		t.Skip("no pty on this platform")
	}
	installFakeDocker(t, `case "$1" in pull)
[ -t 1 ] || { echo "not a terminal"; exit 3; }
echo "size $(stty -F /proc/$$/fd/1 size 2>/dev/null || echo unknown)"
printf '3.20: Pulling from docker/library/alpine\n'
printf '25f1d6b1951a: Pulling fs layer\n'
printf '\033[1A\033[2K\r25f1d6b1951a: Downloading [=====>    ]  1.049MB/3.624MB\r\033[1B'
printf '\033[1A\033[2K\r25f1d6b1951a: Download complete\r\033[1B'
printf '\033[?25h'
exit 0;; esac`)
	var lines []string
	if err := New().PullImage(context.Background(), "alpine:3.20", func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"size 50 200",
		"3.20: Pulling from docker/library/alpine",
		"25f1d6b1951a: Pulling fs layer",
		"25f1d6b1951a: Downloading [=====>    ]  1.049MB/3.624MB",
		"25f1d6b1951a: Download complete",
	}
	if hostruntime.GOOS != "linux" {
		want[0] = lines[0]
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q\nwant %q", lines, want)
	}
}

// A failing pull's own words, written to the terminal, are the error.
func TestPullOnATerminalReturnsTheCLIError(t *testing.T) {
	if !ptyio.Supported() {
		t.Skip("no pty on this platform")
	}
	installFakeDocker(t, `case "$1" in pull) echo "Error response from daemon: manifest unknown" >&2; exit 1;; esac`)
	err := New().PullImage(context.Background(), "alpine:missing", nil)
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("err = %v, want the CLI's message", err)
	}
}

func TestScanPullLinesEndsLinesAtCarriageReturnsAndDropsBlankRedraws(t *testing.T) {
	var lines []string
	input := "a: Waiting\r\n\r\x1b[2K\rb: Downloading 1.0MB/2.0MB\r\x1b[1B\x1b[2K\rtail without newline"
	if err := scanPullLines(strings.NewReader(input), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a: Waiting", "b: Downloading 1.0MB/2.0MB", "tail without newline"}; !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	if err := scanPullLines(iotest.ErrReader(syscall.EIO), func(string) {}); err != nil {
		t.Fatalf("the end of a terminal's output is not an error: %v", err)
	}
	if err := scanPullLines(iotest.ErrReader(syscall.EBADF), func(string) {}); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("a read failure is passed on: %v", err)
	}
}

// A daemon still coming up is ErrDaemonNotReady, so startup can wait for it;
// one that refuses this agent's account its socket is a *SocketDeniedError
// naming the socket Detect resolved, so the operator can be told whom to
// add to which group; anything else is plain false. The stderr lines are
// what docker 24 and 27 print.
func TestDetectSeparatesADaemonNotReadyFromOneThisAgentMayNotUse(t *testing.T) {
	sock := shortUnixSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	for _, tc := range []struct {
		name, stderr     string
		notReady, denied bool
	}{
		{"not up yet", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", true, false},
		{"refusing connections", "error during connect: dial unix /var/run/docker.sock: connect: connection refused", true, false},
		{"permission denied", `permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.24/info": dial unix /var/run/docker.sock: connect: permission denied`, false, true},
		{"permission denied, docker 25+", `permission denied while trying to connect to the docker API at unix:///var/run/docker.sock`, false, true},
		{"other failure", "unknown flag: --format", false, false},
		{"a permission error that is not the socket", "open /root/.docker/config.json: permission denied", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeDocker(t, "echo '"+tc.stderr+"' >&2; exit 1")
			ok, err := New().Detect(context.Background())
			if ok || errors.Is(err, runtime.ErrDaemonNotReady) != tc.notReady {
				t.Fatalf("detect = %t,%v; want not-ready=%t", ok, err, tc.notReady)
			}
			var denied *runtime.SocketDeniedError
			if errors.As(err, &denied) != tc.denied {
				t.Fatalf("detect = %v; want socket denied=%t", err, tc.denied)
			}
			switch {
			case tc.notReady:
				if !strings.Contains(err.Error(), tc.stderr) {
					t.Fatalf("error %q does not carry the daemon's answer", err)
				}
			case tc.denied:
				if denied.Socket != sock || denied.Detail != tc.stderr || !runtime.IsPermissionDenied(err) {
					t.Fatalf("denied = %+v (permission denied=%t), want socket %s and docker's line", denied, runtime.IsPermissionDenied(err), sock)
				}
			default:
				if err != nil {
					t.Fatalf("detect error = %v, want none", err)
				}
			}
		})
	}
	if !daemonNotReady(context.DeadlineExceeded, "") {
		t.Fatal("a docker info that outlived its timeout is a busy daemon")
	}
}

func TestBuildRunArgsRendersWorkloadLabels(t *testing.T) {
	args := buildRunArgs(runtime.WorkloadSpec{
		Name:   "gpuworks-test",
		Image:  "vllm/vllm-openai:latest",
		Labels: map[string]string{"gpuworks.server": "https://gw.example:8800", "gpuworks.instance": "inst-1"},
	})

	assertContainsPair(t, args, "--label", "gpuworks.server=https://gw.example:8800")
	assertContainsPair(t, args, "--label", "gpuworks.instance=inst-1")
	if i := slices.Index(args, "vllm/vllm-openai:latest"); i < 0 || slices.Index(args, "--label") > i {
		t.Fatalf("labels must come before the image: %q", args)
	}
}
