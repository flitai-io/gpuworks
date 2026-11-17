package udocker

import (
	"context"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/assets"
)

func TestRunEnvCriticalVars(t *testing.T) {
	d := New("/tmp/base")
	env := d.runEnv(runtime.WorkloadSpec{
		GPUIndexes: []int{2, 3},
		Env:        map[string]string{"HF_HUB_OFFLINE": "1", "NCCL_IB_DISABLE": "override"},
	})
	// The loader shim must be present (CUDA under proot depends on it).
	if !strings.Contains(env["LD_LIBRARY_PATH"], "/opt/nvidia-shim") {
		t.Errorf("LD_LIBRARY_PATH missing nvidia-shim: %q", env["LD_LIBRARY_PATH"])
	}
	if env["NCCL_SOCKET_IFNAME"] != "lo" {
		t.Error("NCCL_SOCKET_IFNAME must be lo under udocker")
	}
	if env["NCCL_NVLS_ENABLE"] != "0" {
		t.Error("NCCL_NVLS_ENABLE must be 0 (Hopper hang under udocker)")
	}
	if env["CUDA_VISIBLE_DEVICES"] != "2,3" {
		t.Errorf("GPU selection wrong: %q", env["CUDA_VISIBLE_DEVICES"])
	}
	// Caller env overrides the defaults.
	if env["HF_HUB_OFFLINE"] != "1" || env["NCCL_IB_DISABLE"] != "override" {
		t.Error("caller env should override driver defaults")
	}
}

func TestCopyTreePreservesSymlinkIdentity(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(filepath.Join(src, "real"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst, nil); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dst, "alias"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alias mode = %v, want symlink", info.Mode())
	}
	if got, err := os.Readlink(filepath.Join(dst, "alias")); err != nil || got != "real" {
		t.Fatalf("alias target = %q, err=%v; want real", got, err)
	}
}

func TestAscendRunArgsBindSelectedDeviceAndSkipNVIDIAEnvironment(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	spec := runtime.WorkloadSpec{Name: "ascend", GPUIndexes: []int{3}, Env: map[string]string{"ASCEND_VISIBLE_DEVICES": "3", "ASCEND_RT_VISIBLE_DEVICES": "0"}}
	env := d.runEnv(spec)
	if env["ASCEND_VISIBLE_DEVICES"] != "3" || env["CUDA_VISIBLE_DEVICES"] != "" || env["NCCL_IB_DISABLE"] != "" {
		t.Fatalf("Ascend environment leaked NVIDIA defaults: %+v", env)
	}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/davinci3:/dev/davinci3", "--env=ASCEND_VISIBLE_DEVICES=3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Ascend args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "nvidia-smi") || strings.Contains(joined, "/dev/davinci0:/dev/davinci0") {
		t.Fatalf("unselected/NVIDIA device leaked into Ascend args: %s", joined)
	}
}

func TestPPUDeviceArgsBecomeUdockerVolumesWithoutNVIDIAEnvironment(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	spec := runtime.WorkloadSpec{Name: "ppu", DockerArgs: []string{"--device", "/dev/alixpu:/dev/alixpu:rwm", "--device", "/dev/alixpu_ppu7:/dev/alixpu_ppu7:rwm"}, Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void"}}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/alixpu:/dev/alixpu", "--volume=/dev/alixpu_ppu7:/dev/alixpu_ppu7"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("PPU args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "NCCL_IB_DISABLE") || strings.Contains(joined, "nvidia-smi") {
		t.Fatalf("PPU args leaked NVIDIA setup: %s", joined)
	}
}

func TestHygonDeviceArgsBecomeUdockerVolumesWithoutNVIDIAEnvironment(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	spec := runtime.WorkloadSpec{
		Name: "hygon",
		DockerArgs: []string{
			"--device", "/dev/kfd:/dev/kfd:rwm",
			"--device=/dev/dri/renderD128:/dev/dri/renderD128:rwm",
		},
		Env:    map[string]string{"HYGON_VISIBLE_DEVICES": "1"},
		Mounts: []runtime.Mount{{Source: "/opt/hyhal", Target: "/opt/hyhal", ReadOnly: true}},
	}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/kfd:/dev/kfd", "--volume=/dev/dri/renderD128:/dev/dri/renderD128", "--volume=/opt/hyhal:/opt/hyhal"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Hygon args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "CUDA_VISIBLE_DEVICES") || strings.Contains(joined, "NCCL_IB_DISABLE") || strings.Contains(joined, "nvidia-smi") {
		t.Fatalf("Hygon args leaked NVIDIA setup: %s", joined)
	}
}

func TestNVIDIAVisibleDevicesBecomeUdockerVolumes(t *testing.T) {
	// The device nodes have to be given a fixed topology: acceleratorMounts
	// only binds a node that the driver report or the filesystem vouches for,
	// so a bare host without /dev/nvidia* would bind nothing.
	stubNVIDIADevices(t, map[int]string{2: "/dev/nvidia2", 4: "/dev/nvidia4"},
		"/dev/nvidia2", "/dev/nvidia4")
	spec := runtime.WorkloadSpec{
		Name: "nvidia",
		Env:  map[string]string{"CUDA_VISIBLE_DEVICES": "2,4"},
	}
	joined := strings.Join(New("/tmp/base").containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/nvidia2:/dev/nvidia2", "--volume=/dev/nvidia4:/dev/nvidia4"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("NVIDIA args missing %q: %s", want, joined)
		}
	}
}

func TestDomesticSpecsSkipNVIDIASetup(t *testing.T) {
	tests := []struct {
		name string
		spec runtime.WorkloadSpec
	}{
		{name: "ascend", spec: runtime.WorkloadSpec{Env: map[string]string{"ASCEND_VISIBLE_DEVICES": "0"}}},
		{name: "ppu", spec: runtime.WorkloadSpec{DockerArgs: []string{"--device", "/dev/alixpu"}}},
		{name: "hygon", spec: runtime.WorkloadSpec{Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if needsNVIDIASetup(tt.spec) {
				t.Fatal("domestic workload would invoke udocker setup --nvidia")
			}
		})
	}
	if !needsNVIDIASetup(runtime.WorkloadSpec{}) {
		t.Fatal("NVIDIA/default workload unexpectedly skipped setup --nvidia")
	}
}

func TestContainerRunArgsPreserveContainerWithoutDockerBooleanFlag(t *testing.T) {
	d := New("/tmp/base")
	args := d.containerRunArgs(runtime.WorkloadSpec{
		Name:       "my-container",
		Env:        map[string]string{"EXAMPLE": "value"},
		Mounts:     []runtime.Mount{{Source: "/host", Target: "/container"}},
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-m", "example"},
	})
	joined := strings.Join(args, " ")
	if args[0] != "run" {
		t.Fatalf("first argument = %q, want run", args[0])
	}
	if strings.Contains(joined, "--rm") {
		t.Fatalf("udocker run arguments contain unsupported Docker-style rm option: %v", args)
	}
	for _, want := range []string{
		"--env=EXAMPLE=value",
		"--volume=/host:/container",
		"--entrypoint=python3",
		"my-container -m example",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("udocker run arguments missing %q: %v", want, args)
		}
	}
}

func TestContainerRunArgsPreserveHAMiMountsAndEnvironment(t *testing.T) {
	d := New("/tmp/base")
	args := d.containerRunArgs(runtime.WorkloadSpec{
		Name: "dgpu", Env: map[string]string{
			"LD_PRELOAD":                 "/gpuworks/libvgpu.so",
			"CUDA_DEVICE_MEMORY_LIMIT_0": "8589934592",
		}, Mounts: []runtime.Mount{
			{Source: "/state/hami/libvgpu.so", Target: "/gpuworks/libvgpu.so", ReadOnly: true},
			{Source: "/state/hami-cache", Target: "/gpuworks/cache"},
		},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--env=LD_PRELOAD=/gpuworks/libvgpu.so",
		"--env=CUDA_DEVICE_MEMORY_LIMIT_0=8589934592",
		"--volume=/state/hami/libvgpu.so:/gpuworks/libvgpu.so",
		"--volume=/state/hami-cache:/gpuworks/cache",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("udocker args missing %q: %v", want, args)
		}
	}
}

func TestContainerRunArgsExposeHostNvidiaSMI(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	args := d.containerRunArgs(runtime.WorkloadSpec{Name: "sglang"})
	want := "--volume=/host/bin/nvidia-smi:/usr/bin/nvidia-smi"
	if !slices.Contains(args, want) {
		t.Fatalf("udocker args = %v, want %q", args, want)
	}

	args = d.containerRunArgs(runtime.WorkloadSpec{Name: "custom", Mounts: []runtime.Mount{{
		Source: "/custom/nvidia-smi", Target: "/usr/bin/nvidia-smi",
	}}})
	joined := strings.Join(args, " ")
	if strings.Count(joined, ":/usr/bin/nvidia-smi") != 1 || !strings.Contains(joined, "/custom/nvidia-smi:/usr/bin/nvidia-smi") {
		t.Fatalf("explicit nvidia-smi mount was not preserved: %v", args)
	}
}

func TestContainerRunArgsOverrideImageEntrypointWithoutDuplication(t *testing.T) {
	d := New("/tmp/base")
	// Defaults inspected from the official linux/amd64 image configs:
	// GPUStack runners use `tini --`, upstream vLLM uses `vllm serve`, and
	// upstream SGLang uses NVIDIA's entrypoint script. GPUWorks must override
	// all four with the same engine-owned command contract.
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
			containerName := tt.name + "-container"
			args := d.containerRunArgs(runtime.WorkloadSpec{
				Name: containerName, Image: tt.image, Entrypoint: tt.entrypoint, Cmd: tt.cmd,
			})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--entrypoint="+tt.entrypoint[0]) {
				t.Fatalf("udocker entrypoint override missing for %s: %v", tt.image, args)
			}
			wantTail := append([]string{containerName}, tt.entrypoint[1:]...)
			wantTail = append(wantTail, tt.cmd...)
			if len(args) < len(wantTail) || !slices.Equal(args[len(args)-len(wantTail):], wantTail) {
				t.Fatalf("udocker command tail for %s = %v, want %v", tt.image, args, wantTail)
			}
			if strings.Contains(joined, containerName+" "+strings.Join(tt.entrypoint, " ")) {
				t.Fatalf("image entrypoint would be duplicated for %s: %v", tt.image, args)
			}
		})
	}
}

func TestDiagnosticsReportsEmbeddedOfflineSource(t *testing.T) {
	t.Setenv("GPUWORKS_UDOCKER_BUNDLE", "")
	t.Setenv("GPUWORKS_ALLOW_ONLINE_UDOCKER_INSTALL", "")
	d := New("/opt/gpuworks")
	report := d.Diagnostics()
	for _, want := range []string{
		"embedded offline bundle " + assets.UdockerBundleVersion,
		"/opt/gpuworks/offline/udocker",
		"online:   false",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("diagnostics missing %q:\n%s", want, report)
		}
	}
}

func TestUdockerControlledContainerPTYArgv(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.prootPath = filepath.Join(base, "udocker", "bin", "proot-x86_64")
	d.udockerScript = filepath.Join(base, "offline", "udocker", "udocker")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.rememberSpec("demo", runtime.WorkloadSpec{
		Name:       "demo",
		Env:        map[string]string{"VLLM_HOST_IP": "127.0.0.1"},
		Mounts:     []runtime.Mount{{Source: "/models/demo", Target: "/model"}},
		GPUIndexes: []int{2},
	}); err != nil {
		t.Fatal(err)
	}
	var drv runtime.Driver = d
	if _, ok := drv.(runtime.PTYExecer); !ok {
		t.Fatal("udocker controlled shell must implement PTYExecer")
	}
	argv, err := d.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"run", "--env=VLLM_HOST_IP=127.0.0.1", "--env=CUDA_VISIBLE_DEVICES=2", "--volume=/models/demo:/model", "--workdir=/", "--entrypoint=/bin/sh", "demo", "-l"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("controlled udocker PTY argv missing %q: %v", want, argv)
		}
	}
}

func TestUdockerControlledContainerPTYLoadsPersistedSpecAfterRestart(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	first := New(base)
	if err := os.MkdirAll(first.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := first.rememberSpec("demo", runtime.WorkloadSpec{
		Env:    map[string]string{"MODEL_PATH": "/model"},
		Mounts: []runtime.Mount{{Source: "/models/demo", Target: "/model"}},
	}); err != nil {
		t.Fatal(err)
	}

	restarted := New(base)
	restarted.provisioned = true
	restarted.prootPath = filepath.Join(base, "udocker", "bin", "proot-x86_64")
	restarted.udockerScript = filepath.Join(base, "offline", "udocker", "udocker")
	argv, err := restarted.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--env=MODEL_PATH=/model", "--volume=/models/demo:/model", "--workdir=/"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("restarted driver PTY argv missing %q: %v", want, argv)
		}
	}
}

func TestContainerRootPrefersExactUdockerName(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	wanted := filepath.Join(d.udockerDir, "containers", "gpuworks-current", "ROOT")
	other := filepath.Join(d.udockerDir, "containers", "stale-container", "ROOT")
	if err := os.MkdirAll(wanted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := d.containerRoot(context.Background(), "gpuworks-current")
	if err != nil {
		t.Fatal(err)
	}
	if got != wanted {
		t.Fatalf("container root = %q, want exact named root %q", got, wanted)
	}
}

func TestRootfsFixupsCreateCanonicalHostPathLoopback(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	realID := "7f315f0c-9cc5-4e01-b7de-canonical"
	realContainer := filepath.Join(d.udockerDir, "containers", realID)
	realRoot := filepath.Join(realContainer, "ROOT")
	for _, dir := range []string{
		filepath.Join(realRoot, "etc"),
		filepath.Join(realRoot, "root"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	namedContainer := filepath.Join(d.udockerDir, "containers", "gpuworks-current")
	if err := os.Symlink(realContainer, namedContainer); err != nil {
		t.Fatal(err)
	}

	if err := d.rootfsFixups(context.Background(), "gpuworks-current"); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	loopback := filepath.Join(canonicalRoot, strings.TrimPrefix(canonicalRoot, string(filepath.Separator)))
	target, err := os.Readlink(loopback)
	if err != nil {
		t.Fatalf("read canonical loopback %s: %v", loopback, err)
	}
	if target != canonicalRoot {
		t.Fatalf("canonical loopback target = %q, want %q", target, canonicalRoot)
	}
	if strings.Contains(loopback, filepath.Join("home", "udocker", ".udocker")) {
		t.Fatalf("loopback used the old hard-coded udocker dir: %s", loopback)
	}
	if strings.Contains(loopback, "gpuworks-current") {
		t.Fatalf("loopback used named alias instead of real container id: %s", loopback)
	}
	// Repeated setup is expected on every workload start and must be idempotent.
	if err := d.rootfsFixups(context.Background(), "gpuworks-current"); err != nil {
		t.Fatalf("second rootfs fixup: %v", err)
	}
}

func TestHostRootLoopbackReplacesOnlyStaleSymlink(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	realRoot := filepath.Join(d.udockerDir, "containers", "real-id", "ROOT")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	loopback := filepath.Join(canonicalRoot, strings.TrimPrefix(canonicalRoot, string(filepath.Separator)))
	if err := os.MkdirAll(filepath.Dir(loopback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/stale/root", loopback); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureHostRootLoopback(canonicalRoot); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(loopback); err != nil || target != canonicalRoot {
		t.Fatalf("repaired loopback = %q, %v; want %q", target, err, canonicalRoot)
	}

	if err := os.Remove(loopback); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(loopback, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureHostRootLoopback(canonicalRoot); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("non-symlink loopback error = %v", err)
	}
}

func TestUdockerInvocationRunUser(t *testing.T) {
	d := New("/tmp/base")
	d.runUser = "udocker"
	d.prootPath = "/tmp/base/udocker/bin/proot-x86_64"
	d.pyPath = "/tmp/base/offline/udocker/python"
	inv := d.udockerInvocation([]string{"run", "myctr"})
	if inv[0] != "runuser" || inv[1] != "-u" || inv[2] != "udocker" {
		t.Errorf("expected runuser wrapper, got %v", inv[:3])
	}
	joined := strings.Join(inv, " ")
	if !strings.Contains(joined, "UDOCKER_DIR=") || !strings.Contains(joined, "UDOCKER_USE_PROOT_EXECUTABLE=") || !strings.Contains(joined, "PYTHONPATH=") {
		t.Errorf("invocation missing UDOCKER env: %v", inv)
	}
	if strings.Count(joined, "runuser") != 1 {
		t.Errorf("invocation must switch user exactly once, got %v", inv)
	}
}

type deviceInfo struct {
	name string
	mode os.FileMode
	stat *syscall.Stat_t
}

func (i deviceInfo) Name() string       { return i.name }
func (i deviceInfo) Size() int64        { return 0 }
func (i deviceInfo) Mode() os.FileMode  { return i.mode }
func (i deviceInfo) ModTime() time.Time { return time.Time{} }
func (i deviceInfo) IsDir() bool        { return false }
func (i deviceInfo) Sys() any           { return i.stat }

func TestRequiredDeviceGroupsUsesDeviceOwnershipWithoutVendorNames(t *testing.T) {
	mounts := []runtime.Mount{
		{Source: "/dev/accelerator0"},
		{Source: "/dev/accelerator-manager"},
		{Source: "/usr/lib/accelerator"},
	}
	stat := func(path string) (os.FileInfo, error) {
		if strings.HasPrefix(path, "/dev/") {
			return deviceInfo{name: filepath.Base(path), mode: os.ModeDevice | 0o660, stat: &syscall.Stat_t{Uid: 0, Gid: 1234}}, nil
		}
		return deviceInfo{name: filepath.Base(path), mode: 0o755, stat: &syscall.Stat_t{Uid: 0, Gid: 0}}, nil
	}
	lookup := func(gid string) (*user.Group, error) {
		if gid != "1234" {
			t.Fatalf("group lookup = %q, want device gid 1234", gid)
		}
		return &user.Group{Gid: gid, Name: "accelerator-access"}, nil
	}
	groups, devices, err := requiredDeviceGroups(mounts, 9001, 9001, map[int]bool{9001: true}, stat, lookup, false)
	if err != nil {
		t.Fatal(err)
	}
	if devices != 2 {
		t.Fatalf("device count = %d, want 2", devices)
	}
	if !slices.Equal(groups, []string{"accelerator-access"}) {
		t.Fatalf("device groups = %v", groups)
	}
}

func TestRequiredDeviceGroupsHonorsExistingSupplementaryMembership(t *testing.T) {
	mounts := []runtime.Mount{{Source: "/dev/accelerator0"}}
	stat := func(string) (os.FileInfo, error) {
		return deviceInfo{name: "accelerator0", mode: os.ModeDevice | 0o660, stat: &syscall.Stat_t{Uid: 0, Gid: 1234}}, nil
	}
	groups, devices, err := requiredDeviceGroups(mounts, 9001, 9001, map[int]bool{9001: true, 1234: true}, stat, user.LookupGroupId, false)
	if err != nil {
		t.Fatal(err)
	}
	if devices != 1 || len(groups) != 0 {
		t.Fatalf("devices=%d missing groups=%v, want 1 and none", devices, groups)
	}
}

func TestUdockerMountOwnerRequiresProvisioningAndReportsResolvedIdentity(t *testing.T) {
	d := New(t.TempDir())
	if _, err := d.MountOwner(); err == nil {
		t.Fatal("unprovisioned driver guessed a mount owner")
	}
	d.provisioned = true
	d.runUser = "udocker"
	d.runUID = 1234
	d.runGID = 1235
	owner, err := d.MountOwner()
	if err != nil {
		t.Fatal(err)
	}
	if owner.UID != 1234 || owner.GID != 1235 || owner.Username != "udocker" {
		t.Fatalf("owner=%+v", owner)
	}
}

func TestUdockerMountOwnerAllowRootUsesCurrentIdentity(t *testing.T) {
	d := New(t.TempDir())
	d.provisioned = true
	d.runUser = ""
	d.runUID = os.Geteuid()
	d.runGID = os.Getegid()
	owner, err := d.MountOwner()
	if err != nil {
		t.Fatal(err)
	}
	if owner.UID != os.Geteuid() || owner.GID != os.Getegid() || owner.Username != "" {
		t.Fatalf("allow-root owner=%+v", owner)
	}
}

func TestUdockerInvocationDirect(t *testing.T) {
	d := New("/tmp/base")
	d.pyBin = "python3"
	inv := d.udockerInvocation([]string{"images"})
	if inv[0] != "python3" || inv[1] != "-m" || inv[2] != "udocker" {
		t.Errorf("expected direct python invocation, got %v", inv)
	}
}

func TestDedicatedUdockerUserRequiresRunnablePython(t *testing.T) {
	if !shouldUseDedicatedUdockerUser("/usr/bin/python3", nil) {
		t.Fatal("runnable Python should keep the dedicated udocker user")
	}
	if shouldUseDedicatedUdockerUser("/root/miniconda3/bin/python3", os.ErrPermission) {
		t.Fatal("inaccessible Python should select --allow-root compatibility mode")
	}
	if shouldUseDedicatedUdockerUser("", nil) {
		t.Fatal("empty Python path should not select the dedicated udocker user")
	}
}

func TestFindPython3UsesExecutableFromPATH(t *testing.T) {
	dir := t.TempDir()
	python := filepath.Join(dir, "python3")
	if err := os.WriteFile(python, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := findPython3(); got != python {
		t.Fatalf("findPython3() = %q, want %q", got, python)
	}
}

func TestUdockerCLIEnvPinsLocalDirAndProot(t *testing.T) {
	d := New("/tmp/base")
	d.prootPath = "/tmp/base/udocker/bin/proot-x86_64"
	d.pyPath = "/tmp/base/offline/udocker/python"
	got := map[string]string{}
	for _, entry := range d.cliEnv() {
		if k, v, ok := strings.Cut(entry, "="); ok && (k == "UDOCKER_DIR" || k == "UDOCKER_USE_PROOT_EXECUTABLE" || k == "PYTHONPATH") {
			got[k] = v
		}
	}
	if got["UDOCKER_DIR"] != d.udockerDir || got["UDOCKER_USE_PROOT_EXECUTABLE"] != d.prootPath || got["PYTHONPATH"] != d.pyPath {
		t.Fatalf("direct udocker env = %#v", got)
	}
}

func TestUdockerInvocationOfflineExecutable(t *testing.T) {
	d := New("/tmp/base")
	d.udockerExe = "/opt/gpuworks/offline/udocker/bin/udocker"
	inv := d.udockerInvocation([]string{"images"})
	if inv[0] != d.udockerExe || inv[1] != "--allow-root" {
		t.Errorf("expected offline executable invocation, got %v", inv)
	}
}

func TestLoadImageStreamsDockerArchiveToUdocker(t *testing.T) {
	// The staged udocker CLI load is the native-disabled path.
	t.Setenv(disableNativeEnv, "1")
	base := t.TempDir()
	archivePath := filepath.Join(base, "received.tar")
	stub := filepath.Join(base, "udocker-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nset -eu\ntest \"$1\" = --allow-root\ntest \"$2\" = load\ncase \"$3\" in -i=*) cp \"${3#-i=}\" \"$GPUWORKS_TEST_ARCHIVE\";; *) exit 3;; esac\ntest \"$4\" = gpuworks/partition-probe\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_TEST_ARCHIVE", archivePath)
	d := New(base)
	d.provisioned = true
	d.udockerExe = stub
	d.runUser = ""
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "docker-save-archive"
	if err := d.LoadImage(context.Background(), "gpuworks/partition-probe:v1", strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("loaded archive = %q, want %q", got, want)
	}
}

func TestImageRepository(t *testing.T) {
	for image, want := range map[string]string{
		"gpuworks/partition-probe:v1":                    "gpuworks/partition-probe",
		"registry.example:5000/team/probe:v1":            "registry.example:5000/team/probe",
		"registry.example:5000/team/probe@sha256:abcdef": "registry.example:5000/team/probe",
	} {
		if got := imageRepository(image); got != want {
			t.Errorf("imageRepository(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestUseOfflineBundleExecutableAndPreseedDir(t *testing.T) {
	base := t.TempDir()
	makeUdockerTempRootTraversable(t, base)
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	udo := filepath.Join(bundle, "bin", "udocker")
	if err := os.WriteFile(udo, []byte("#!/bin/sh\necho udocker 1.3.17\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "udocker-dir", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "udocker-dir", "lib", "marker"), []byte("preseed"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.bundleDir = bundle
	ok, err := d.useOfflineBundle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || d.udockerExe != udo {
		t.Fatalf("offline bundle not selected: ok=%v exe=%q", ok, d.udockerExe)
	}
	if _, err := os.Stat(filepath.Join(d.udockerDir, "lib", "marker")); err != nil {
		t.Fatalf("preseed dir not copied: %v", err)
	}
}

func TestUseOfflineBundleSelfContainedPythonEntry(t *testing.T) {
	base := t.TempDir()
	makeUdockerTempRootTraversable(t, base)
	// The test must be independent of the CI image's Python installation. The
	// production driver still defaults to python3; here a tiny executable is
	// enough because useOfflineBundle only needs a successful --version probe.
	stubPython := filepath.Join(base, "python-stub")
	if err := os.WriteFile(stubPython, []byte("#!/bin/sh\necho Python 3.11.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "udocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "englib", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "englib", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(bundle, "udocker", "udocker")
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env python3\nimport sys\nprint('offline', ' '.join(sys.argv[1:]))\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Like the embedded bundle, the fixture ships no PRoot of its own: the
	// patched static-nofd PRoot must be installed under both udocker names.
	archName, _ := engineArchName(goruntime.GOARCH)
	if err := os.WriteFile(filepath.Join(bundle, "englib", "lib", "VERSION"), []byte("1.2.11\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(base)
	d.pyBin = stubPython
	d.bundleDir = bundle
	d.embeddedBundle = false
	ok, err := d.useOfflineBundle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !d.offline || d.udockerScript != entry {
		t.Fatalf("self-contained bundle not selected: ok=%v offline=%v script=%q", ok, d.offline, d.udockerScript)
	}
	wantProot := filepath.Join(d.udockerDir, "bin", "proot-"+archName)
	if d.prootPath != wantProot {
		t.Fatalf("self-built proot path = %q, want %q", d.prootPath, wantProot)
	}
	var first []byte
	for _, name := range []string{"proot-" + archName, "proot-" + archName + "-4_8_0"} {
		data, err := os.ReadFile(filepath.Join(d.udockerDir, "bin", name))
		if err != nil {
			t.Fatalf("read self-built %s: %v", name, err)
		}
		if len(data) < 1<<20 {
			t.Fatalf("self-built %s is unexpectedly small: %d bytes", name, len(data))
		}
		if first == nil {
			first = data
		} else if string(data) != string(first) {
			t.Fatalf("self-built proot aliases differ")
		}
	}
}

func TestPullImageLogsOfflineInstallSkipped(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	// What this test covers is the offline-bundle log line and the udocker CLI
	// pull output, which only the native-disabled escape hatch reaches. Left
	// enabled, the native pull dials the real registry for the Docker Hub
	// reference; on a runner whose DNS resolves registry-1.docker.io to a
	// black-holed address the dial burns its full timeout (job 177015 spent
	// 61s here) and the pull never behaved like the fixture assumes.
	t.Setenv(disableNativeEnv, "1")
	base := t.TempDir()
	makeUdockerTempRootTraversable(t, base)
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	udo := filepath.Join(bundle, "bin", "udocker")
	if err := os.WriteFile(udo, []byte("#!/bin/sh\ncase \"$1 $2\" in \"--allow-root --version\") echo udocker 1.3.17;; \"--allow-root pull\") echo pulled \"$3\";; *) echo ok;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.bundleDir = bundle
	d.embeddedBundle = false
	var lines []string
	if err := d.PullImage(context.Background(), "example/image:tag", func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "installation skipped") {
		t.Fatalf("offline skip log missing: %q", joined)
	}
	if !strings.Contains(joined, "pulled example/image:tag") {
		t.Fatalf("pull output missing: %q", joined)
	}
}

func TestPullStorageProgressUsesManifestLayerSizes(t *testing.T) {
	d := New(t.TempDir())
	layersDir := filepath.Join(d.udockerDir, "layers")
	// Where udocker 1.3 writes it: repos/<reference as pulled, host included>/<tag>.
	manifestDir := filepath.Join(d.udockerDir, "repos", "quay.io", "gpustack", "runner", "tag")
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "{\"layers\":[{\"digest\":\"sha256:first\",\"size\":100},{\"digest\":\"sha256:second\",\"size\":200}]}"
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layersDir, "sha256:first"), make([]byte, 80), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layersDir, "sha256:second"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	layers, grown := d.pullStorageProgress("quay.io/gpustack/runner:tag", 0)
	want := []storageLayer{{digest: "sha256:first", done: 80, size: 100}, {digest: "sha256:second", done: 200, size: 200}}
	if !reflect.DeepEqual(layers, want) || grown != 0 {
		t.Fatalf("storage progress = %+v, %d; want %+v, 0", layers, grown, want)
	}
}

func TestPullStorageProgressFallsBackToLayerGrowth(t *testing.T) {
	d := New(t.TempDir())
	layersDir := filepath.Join(d.udockerDir, "layers")
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layersDir, "existing"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline := directoryFileBytes(layersDir)
	if err := os.WriteFile(filepath.Join(layersDir, "downloading"), make([]byte, 75), 0o644); err != nil {
		t.Fatal(err)
	}
	layers, grown := d.pullStorageProgress("example/image:tag", baseline)
	if layers != nil || grown != 75 {
		t.Fatalf("fallback storage progress = %+v, %d; want no layers, 75", layers, grown)
	}
}

func TestPullStorageProgressUsesOnlyRequestedImageManifest(t *testing.T) {
	d := New(t.TempDir())
	layersDir := filepath.Join(d.udockerDir, "layers")
	requested := filepath.Join(d.udockerDir, "repos", "quay.io", "gpustack", "runner", "wanted")
	unrelated := filepath.Join(d.udockerDir, "repos", "other", "image", "newer")
	// The Docker Hub image of the same name is a different image.
	hub := filepath.Join(d.udockerDir, "repos", "gpustack", "runner", "wanted")
	for _, dir := range []string{layersDir, requested, unrelated, hub} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(requested, "manifest"), []byte("{\"layers\":[{\"digest\":\"sha256:wanted\",\"size\":100}]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "manifest"), []byte("{\"layers\":[{\"digest\":\"sha256:other\",\"size\":999}]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hub, "manifest"), []byte("{\"layers\":[{\"digest\":\"sha256:other\",\"size\":999}]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layersDir, "sha256:wanted"), make([]byte, 60), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layersDir, "sha256:other"), make([]byte, 999), 0o644); err != nil {
		t.Fatal(err)
	}
	layers, _ := d.pullStorageProgress("quay.io/gpustack/runner:wanted", 999)
	if want := []storageLayer{{digest: "sha256:wanted", done: 60, size: 100}}; !reflect.DeepEqual(layers, want) {
		t.Fatalf("requested image progress = %+v, want %+v", layers, want)
	}
}

func TestSplitImageRef(t *testing.T) {
	tests := []struct {
		image, repository, tag string
	}{
		{"gpustack/runner:cuda12.8", "gpustack/runner", "cuda12.8"},
		// udocker keeps the registry host in the repository directory.
		{"quay.io/gpustack/runner:cuda12.8", "quay.io/gpustack/runner", "cuda12.8"},
		{"registry.example:5000/team/image", "registry.example:5000/team/image", "latest"},
		{"library/ubuntu", "library/ubuntu", "latest"},
		{"quay.io/gpustack/runner@sha256:deadbeef", "", ""},
	}
	for _, tt := range tests {
		repository, tag := splitImageRef(tt.image)
		if repository != tt.repository || tag != tt.tag {
			t.Errorf("splitImageRef(%q) = %q, %q; want %q, %q", tt.image, repository, tag, tt.repository, tt.tag)
		}
	}
}

// Root-run CI provisions udocker under a dedicated non-root account. Go's
// t.TempDir intentionally creates a 0700 directory tree, so that account
// cannot execute a fixture nested below it unless the test opens traversal
// permission on the test-only root and its immediate parent.
func makeUdockerTempRootTraversable(t *testing.T, base string) {
	t.Helper()
	for _, path := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatalf("chmod udocker fixture root %s: %v", path, err)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"quay.io/gpustack/runner:cuda13.0-vllm0.25.1", "quay.io/gpustack/runner:cuda13.0-vllm0.25.1"},
		{"gpustack/runner", "gpustack/runner:latest"},
		{"registry.example:5000/team/runner", "registry.example:5000/team/runner:latest"},
		{" repo/team/runner:tag ", "repo/team/runner:tag"},
		{"repo/team/runner@sha256:deadbeef", "repo/team/runner@sha256:deadbeef"},
	}
	for _, tt := range tests {
		if got := normalizeImage(tt.image); got != tt.want {
			t.Errorf("normalizeImage(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestUdockerImageListedRequiresExactRepository(t *testing.T) {
	output := "REPOSITORY\nswr.example/gpustack/runner:cuda12.8-vllm0.17.1    .\ngpustack/other:latest    P\n"
	if udockerImageListed(output, "gpustack/runner:cuda12.8-vllm0.17.1") {
		t.Fatal("mirror suffix was accepted as the missing canonical image")
	}
	if !udockerImageListed(output, "swr.example/gpustack/runner:cuda12.8-vllm0.17.1") {
		t.Fatal("exact mirror image was not found")
	}
	if !udockerImageListed("REPOSITORY\ngpustack/runner:latest    .\n", "gpustack/runner") {
		t.Fatal("implicit latest tag was not normalized")
	}
	if udockerImageListed("REPOSITORY\nother/team/runner:latest    .\n", "gpustack/runner:latest") {
		t.Fatal("same basename in another repository was accepted")
	}
	if udockerImageListed("REPOSITORY\nubuntu:latest    .\n", "docker.io/library/ubuntu:latest") {
		t.Fatal("Docker Hub alias was accepted without an explicit udocker tag")
	}
	if !udockerImageListed("REPOSITORY\ndocker.io/library/ubuntu:latest    .\n", "docker.io/library/ubuntu:latest") {
		t.Fatal("exact Docker Hub repository was not found")
	}
	if udockerImageListed("REPOSITORY\nrepo/image:latest    .\n", "repo/image@sha256:deadbeef") {
		t.Fatal("digest reference was accepted as a tagged image")
	}
}

func TestQuoteAll(t *testing.T) {
	got := quoteAll([]string{"a b", "c'd"})
	if got[0] != "'a b'" {
		t.Errorf("quote space: %q", got[0])
	}
	if !strings.Contains(got[1], `'\''`) {
		t.Errorf("quote apostrophe: %q", got[1])
	}
}

func TestEngineArchName(t *testing.T) {
	for _, test := range []struct {
		goarch, want string
		ok           bool
	}{
		{goarch: "amd64", want: "x86_64", ok: true},
		{goarch: "arm64", want: "arm64", ok: true},
		{goarch: "riscv64", ok: false},
	} {
		got, ok := engineArchName(test.goarch)
		if got != test.want || ok != test.ok {
			t.Errorf("engineArchName(%q)=(%q,%t), want (%q,%t)", test.goarch, got, ok, test.want, test.ok)
		}
	}
}

func TestDetectIsSafeOnUnsupportedHost(t *testing.T) {
	if goruntime.GOOS == "linux" {
		t.Skip("linux detection depends on the host Python installation")
	}
	d := New(t.TempDir())
	available, err := d.Detect(context.Background())
	if err != nil || available {
		t.Fatalf("non-linux udocker detection = available:%t err:%v", available, err)
	}
}

func TestStatusAndLogsTrackPidAndRedirectedOutput(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.stateDir, "running.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := d.Status(context.Background(), "running")
	if err != nil || !status.Exists || !status.Running {
		t.Fatalf("running status=%+v err=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(d.stateDir, "dead.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = d.Status(context.Background(), "dead")
	if err != nil || !status.Exists || status.Running || status.ExitCode != 1 {
		t.Fatalf("dead status=%+v err=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(d.stateDir, "running.log"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs, err := d.Logs(context.Background(), "running", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	buf, err := io.ReadAll(logs)
	if err != nil || string(buf) != "hello\n" {
		t.Fatalf("logs=%q err=%v", buf, err)
	}
}

func TestRemoveAndExecUseConfiguredCLIAndMissingIdsAreIdempotent(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.stateDir = filepath.Join(base, "state")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(base, "bin", "udocker")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho udocker-stub-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.udockerExe = stub
	if err := d.Stop(context.Background(), "missing"); err != nil {
		t.Fatalf("missing Stop: %v", err)
	}
	if err := d.Remove(context.Background(), "missing"); err != nil {
		t.Fatalf("missing Remove: %v", err)
	}
	out, err := d.Exec(context.Background(), "missing", []string{"true"})
	if err != nil || !strings.Contains(string(out), "udocker-stub-ok") {
		t.Fatalf("Exec output=%q err=%v", out, err)
	}
}

func TestRunWithProgressUsesPreparedRootfsAndTracksProcess(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.udockerExe = filepath.Join(base, "fake-udocker")
	d.prootPath = filepath.Join(base, "proot")
	d.stateDir = filepath.Join(base, "state")
	d.udockerDir = filepath.Join(base, "udocker")
	d.hostTmpRoot = t.TempDir()
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake exits at once and its reaper writes the exit status into the
	// state directory; wait for that before TempDir removes the directory.
	t.Cleanup(func() { d.procs().Forget("demo") })
	if err := os.MkdirAll(filepath.Join(d.udockerDir, "containers", "demo", "ROOT", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.udockerDir, "containers", "demo", "ROOT", "root"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake accepts inspect/setup and exits immediately for the detached run.
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(d.udockerExe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var phases []string
	id, err := d.RunWithProgress(context.Background(), runtime.WorkloadSpec{Name: "demo", Image: "fixture:latest"}, func(phase, _ string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("RunWithProgress: %v", err)
	}
	if id != "demo" {
		t.Fatalf("id = %q, want demo", id)
	}
	if len(phases) < 4 || phases[0] != "container_prepare" || phases[len(phases)-1] != "container_launch" {
		t.Fatalf("unexpected progress phases: %v", phases)
	}
	if _, ok := d.pidOf("demo"); !ok {
		t.Fatal("Run did not persist a pid")
	}
	if _, err := os.Stat(filepath.Join(d.udockerDir, "containers", "demo", "ROOT", "etc", "hosts")); err != nil {
		t.Fatalf("rootfs fixup did not write hosts: %v", err)
	}
	// The launch path prepares the short-path /tmp source, and every later
	// invocation for the workload binds it.
	tmp, ok := d.preparedWorkloadTmp("demo")
	if !ok {
		t.Fatal("Run did not prepare the short-path /tmp source")
	}
	if args := strings.Join(d.containerRunArgs(runtime.WorkloadSpec{Name: "demo"}), " "); !strings.Contains(args, "--volume="+tmp+":/tmp") {
		t.Fatalf("run args miss the /tmp bind %q: %s", tmp, args)
	}
}

func TestNameAndPathOwnershipHelpers(t *testing.T) {
	d := New(t.TempDir())
	if got := d.Name(); got != "udocker" {
		t.Fatalf("Name() = %q", got)
	}
	p := filepath.Join(t.TempDir(), "owned")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	stat := st.Sys().(*syscall.Stat_t)
	owned, err := pathOwnedBy(p, int(stat.Uid), int(stat.Gid))
	if err != nil || !owned {
		t.Fatalf("pathOwnedBy = %v, err=%v", owned, err)
	}
}

// writeUdockerStub installs an executable stand-in for the udocker CLI and
// returns its path. argsFile, when non-empty, records one invocation per line
// so a test can prove the CLI was really called.
func writeUdockerStub(t *testing.T, base, body, argsFile string) string {
	t.Helper()
	stub := filepath.Join(base, "bin", "udocker")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n"
	if argsFile != "" {
		script += "printf '%s\\n' \"$*\" >> '" + argsFile + "'\n"
	}
	if err := os.WriteFile(stub, []byte(script+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

func TestListContainersReadsTheUdockerContainerDirectory(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	containersDir := filepath.Join(d.udockerDir, "containers")

	// A container created by gpuworks' native unpacker: the directory carries
	// the name GPUWorks passed to Run.
	mkContainer := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(containersDir, name, "ROOT", "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(containersDir, name, "container.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkContainer("gpuworks-aaa")
	mkContainer("gpuworks-bbb")
	// A container udocker created itself: generated id plus a name alias.
	mkContainer("3f9c1d2e-0a1b-2c3d-4e5f-67890abcdef0")
	if err := os.Symlink("gpuworks-aaa", filepath.Join(containersDir, "alias-name")); err != nil {
		t.Fatal(err)
	}
	// The staging directory of an in-flight create: it already has a ROOT but is
	// not a container yet, and sweeping it would corrupt that create.
	if err := os.MkdirAll(filepath.Join(containersDir, "gpuworks-ccc"+native.CreateStagingSuffix, "ROOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Entries that are not containers at all.
	if err := os.MkdirAll(filepath.Join(containersDir, "gpuworks-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shared immutable base rootfs cache is not a container and must never be
	// reported as one, or the sweeper would delete an image every container of
	// that image is built from.
	if err := os.MkdirAll(filepath.Join(d.udockerDir, "base", "sha256-deadbeef", "ROOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containersDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := d.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	want := []string{"3f9c1d2e-0a1b-2c3d-4e5f-67890abcdef0", "alias-name", "gpuworks-aaa", "gpuworks-bbb"}
	if !slices.Equal(names, want) {
		t.Fatalf("ListContainers = %v, want %v", names, want)
	}

	// A node that has never provisioned udocker has no container directory at
	// all; that is an empty node, not a failure.
	if names, err := New(t.TempDir()).ListContainers(context.Background()); err != nil || len(names) != 0 {
		t.Fatalf("unprovisioned node = %v, err=%v", names, err)
	}
}

func TestRemoveReclaimsRootfsWhenTheUdockerCLIFails(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.stateDir = filepath.Join(base, "udocker-state")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(d.udockerDir, "containers", "gpuworks-demo")
	if err := os.MkdirAll(filepath.Join(containerDir, "ROOT", "usr", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "ROOT", "usr", "local", "model.bin"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gpuworks-demo.pid", "gpuworks-demo.log", "gpuworks-demo.spec.json"} {
		if err := os.WriteFile(filepath.Join(d.stateDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The CLI reproduction of the measured failure: udocker ends its recursive
	// delete with a chmod on a root-owned ROOT directory and reports the
	// directory as undeletable, verbatim from the A100 node log.
	argsFile := filepath.Join(base, "cli-args")
	d.udockerExe = writeUdockerStub(t, base,
		`echo "Info: deleting container: gpuworks-demo"
echo "Error: deleting directory:  `+containerDir+`" >&2
echo "Error: deleting container" >&2
exit 1`, argsFile)

	if err := d.Remove(context.Background(), "gpuworks-demo"); err != nil {
		t.Fatalf("Remove returned an error although the rootfs was reclaimed: %v", err)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Fatalf("container rootfs still present after Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.stateDir, "gpuworks-demo.pid")); !os.IsNotExist(err) {
		t.Fatalf("state files still present after Remove: %v", err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(recorded), "rm gpuworks-demo") {
		t.Fatalf("udocker CLI was not invoked for the removal: %q err=%v", recorded, err)
	}
}

func TestRemoveReportsErrorWhenTheRootfsCannotBeReclaimed(t *testing.T) {
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.stateDir = filepath.Join(base, "udocker-state")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The container tree is not inspectable (containers/ is a file, so every
	// path below it fails with ENOTDIR): the CLI failed and the rootfs could not
	// be reclaimed either, which must not be reported as a successful deletion.
	if err := os.MkdirAll(d.udockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.udockerDir, "containers"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.udockerExe = writeUdockerStub(t, base,
		`echo "Error: deleting directory:  `+filepath.Join(d.udockerDir, "containers", "gpuworks-demo")+`" >&2
exit 1`, "")

	err := d.Remove(context.Background(), "gpuworks-demo")
	if err == nil {
		t.Fatal("Remove reported success while the container rootfs could not be reclaimed")
	}
	if !strings.Contains(err.Error(), "gpuworks-demo") {
		t.Fatalf("error does not name the container: %v", err)
	}
}

// A container created by `udocker create --name=` lives in a generated-id
// directory with the name as a symlink to it. Removing the link reclaims no
// rootfs, so the CLI failure must stay reported: claiming success there would
// hide the leftover rootfs, and following the link would widen the delete target
// beyond the container directory this driver is allowed to touch.
func TestRemoveDoesNotClaimSuccessWhenTheRootfsIsBehindANameAlias(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.stateDir = filepath.Join(base, "udocker-state")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(d.udockerDir, "containers", "3f9c1d2e-0a1b-2c3d-4e5f-67890abcdef0")
	if err := os.MkdirAll(filepath.Join(realDir, "ROOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(d.udockerDir, "containers", "gpuworks-alias")
	if err := os.Symlink(filepath.Base(realDir), alias); err != nil {
		t.Fatal(err)
	}
	d.udockerExe = writeUdockerStub(t, base,
		`echo "Error: deleting directory:  `+realDir+`" >&2
exit 1`, "")

	err := d.Remove(context.Background(), "gpuworks-alias")
	if err == nil {
		t.Fatal("Remove reported success while the container rootfs survived behind the name alias")
	}
	if _, err := os.Lstat(realDir); err != nil {
		t.Fatalf("test setup expected the real rootfs to survive: %v", err)
	}
}

func TestRemoveTreatsAMissingContainerAsAlreadyRemoved(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // the missing-container error comes from the legacy CLI
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.stateDir = filepath.Join(base, "udocker-state")
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d.udockerExe = writeUdockerStub(t, base,
		`echo "Error: container not found" >&2
exit 1`, "")
	if err := d.Remove(context.Background(), "gpuworks-gone"); err != nil {
		t.Fatalf("removing an already removed container must be idempotent: %v", err)
	}
}

// A native pull failure is the answer: it must reach the caller as an error
// and must not be retried through the udocker CLI.
func TestPullImageReportsNativeFailureWithoutCLIFallback(t *testing.T) {
	base := t.TempDir()
	calls := filepath.Join(base, "cli-calls")
	stub := filepath.Join(base, "udocker-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$*\" >> '"+calls+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.provisioned = true
	d.udockerExe = stub
	// Port 1 on loopback refuses immediately: no network, no timeout.
	err := d.PullImage(context.Background(), "127.0.0.1:1/team/app:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "pull 127.0.0.1:1/team/app:v1") {
		t.Fatalf("native failure not reported: %v", err)
	}
	if data, _ := os.ReadFile(calls); len(data) != 0 {
		t.Fatalf("native failure fell back to the udocker CLI: %s", data)
	}
}

// The NVIDIA shim gets the host's driver libraries; one the host lacks is
// skipped, but one it has that cannot be written (a full disk) fails the
// fixup instead of starting an engine that cannot find NVML.
func TestRootfsFixupsCopyDriverLibrariesAndFailWhenOneCannotBeWritten(t *testing.T) {
	host := t.TempDir()
	for _, lib := range []string{"libcuda.so.1", "libnvidia-ml.so.1"} {
		if err := os.WriteFile(filepath.Join(host, lib), []byte(lib), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := driverLibDirs
	driverLibDirs = []string{host}
	t.Cleanup(func() { driverLibDirs = saved })

	d := New(t.TempDir())
	root := filepath.Join(d.udockerDir, "containers", "gpuworks-shim", "ROOT")
	for _, dir := range []string{filepath.Join(root, "etc"), filepath.Join(root, "root")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.rootfsFixups(context.Background(), "gpuworks-shim"); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(root)
	shim := filepath.Join(canonical, "opt", "nvidia-shim")
	for _, lib := range []string{"libcuda.so.1", "libnvidia-ml.so.1"} {
		if got, err := os.ReadFile(filepath.Join(shim, lib)); err != nil || string(got) != lib {
			t.Fatalf("%s in the shim = %q, %v", lib, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(shim, "libnvidia-nvvm.so.4")); !os.IsNotExist(err) {
		t.Fatalf("a library the host lacks must be skipped, got %v", err)
	}

	// A destination the copy cannot replace stands in for a full disk.
	if err := os.Remove(filepath.Join(shim, "libnvidia-ml.so.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shim, "libnvidia-ml.so.1", "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := d.rootfsFixups(context.Background(), "gpuworks-shim")
	if err == nil || !strings.Contains(err.Error(), "copy NVIDIA driver library libnvidia-ml.so.1") {
		t.Fatalf("fixup with an unwritable driver library = %v, want it named", err)
	}
}

// A disk the run user cannot write to refuses the launch before the workload
// starts: root-side steps still succeed on the blocks a filesystem reserves
// for root, and the engine would then die with no output. The refusal names
// the directory and its free space; nothing is started and no pid is kept.
func TestRunWithProgressRefusesALaunchTheRunUserCannotWrite(t *testing.T) {
	t.Setenv(launcherEnv, "udocker")
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.udockerExe = filepath.Join(base, "fake-udocker")
	d.prootPath = filepath.Join(base, "proot")
	d.stateDir = filepath.Join(base, "state")
	d.udockerDir = filepath.Join(base, "udocker")
	d.hostTmpRoot = t.TempDir()
	for _, dir := range []string{d.stateDir, filepath.Join(d.udockerDir, "containers", "demo", "ROOT", "etc")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.udockerDir, "containers", "demo", "ROOT", "root"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(base, "ran")
	if err := os.WriteFile(d.udockerExe, []byte("#!/bin/sh\ntouch "+ran+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	full := d.stateDir
	d.freeSpace = func(path string) int64 {
		if path == full {
			return 30 << 20
		}
		return 50 << 30
	}
	var phases []string
	_, err := d.RunWithProgress(context.Background(), runtime.WorkloadSpec{Name: "demo", Image: "fixture:latest"}, func(phase, _ string) {
		phases = append(phases, phase)
	})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space to start the container: 0.0 GiB free under "+full) {
		t.Fatalf("launch on a full disk = %v", err)
	}
	if slices.Contains(phases, "container_launch") {
		t.Fatalf("the refused launch still reported launching: %v", phases)
	}
	if _, ok := d.pidOf("demo"); ok {
		t.Fatal("a refused launch kept a pid")
	}

	// With room on both, the same launch goes ahead.
	d.freeSpace = func(string) int64 { return 50 << 30 }
	t.Cleanup(func() { d.procs().Forget("demo") })
	if _, err := d.RunWithProgress(context.Background(), runtime.WorkloadSpec{Name: "demo", Image: "fixture:latest"}, func(string, string) {}); err != nil {
		t.Fatalf("launch with room: %v", err)
	}
}
