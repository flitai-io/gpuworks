package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestAscendRuntimeSmokeUsesTheLogicalIDForTheSelectedPhysicalDie(t *testing.T) {
	previousFinder, previousCommand := ascendSmokeTargetFinder, ascendSmokeCommand
	previousLogical := ascendSmokeLogicalDevice
	t.Cleanup(func() {
		ascendSmokeTargetFinder, ascendSmokeCommand = previousFinder, previousCommand
		ascendSmokeLogicalDevice = previousLogical
	})
	wantTarget := ascendSmokeTarget{Home: "/opt/cann", Library: "/opt/cann/aarch64-linux/lib64/libascendcl.so"}
	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) { return wantTarget, true }
	ascendSmokeLogicalDevice = func(physical string) (string, error) {
		if physical != "14" {
			t.Fatalf("physical die = %q, want 14", physical)
		}
		return "6", nil
	}
	var visible string
	var target ascendSmokeTarget
	ascendSmokeCommand = func(_ context.Context, gotVisible string, gotTarget ascendSmokeTarget) ([]byte, error) {
		visible, target = gotVisible, gotTarget
		return []byte("ASCEND_ACL_SMOKE_OK"), nil
	}

	if err := AscendRuntimeSmokeOn(context.Background(), "14"); err != nil {
		t.Fatal(err)
	}
	if visible != "6" || target != wantTarget {
		t.Fatalf("smoke received logical visible=%q target=%+v", visible, target)
	}
}

func TestAscendRuntimeSmokeMappingFailureIsHostInconclusive(t *testing.T) {
	previousFinder, previousLogical := ascendSmokeTargetFinder, ascendSmokeLogicalDevice
	t.Cleanup(func() {
		ascendSmokeTargetFinder, ascendSmokeLogicalDevice = previousFinder, previousLogical
	})
	ascendSmokeLogicalDevice = func(string) (string, error) {
		return "", errors.New("physical device 14 has no logical device mapping")
	}
	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) {
		t.Fatal("CANN lookup ran despite the mapping failure")
		return ascendSmokeTarget{}, false
	}

	err := AscendRuntimeSmokeOn(context.Background(), "14")
	var unavailable *HostRuntimeUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "no logical device mapping") {
		t.Fatalf("mapping failure = %T %v, want HostRuntimeUnavailableError", err, err)
	}
}

func TestAscendRuntimeSmokeSecondCANNLookupFailureIsHostInconclusive(t *testing.T) {
	previousFinder, previousCommand := ascendSmokeTargetFinder, ascendSmokeCommand
	previousLogical := ascendSmokeLogicalDevice
	t.Cleanup(func() {
		ascendSmokeTargetFinder, ascendSmokeCommand = previousFinder, previousCommand
		ascendSmokeLogicalDevice = previousLogical
	})
	ascendSmokeLogicalDevice = func(physical string) (string, error) { return physical, nil }
	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) { return ascendSmokeTarget{}, false }
	ascendSmokeCommand = func(context.Context, string, ascendSmokeTarget) ([]byte, error) {
		t.Fatal("ACL command ran without a CANN target")
		return nil, nil
	}

	err := AscendRuntimeSmokeOn(context.Background(), "14")
	var unavailable *HostRuntimeUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("second lookup failure = %T %v, want HostRuntimeUnavailableError", err, err)
	}
}

func TestAscendSmokeVerdictRequiresSuccessMarker(t *testing.T) {
	if err := ascendSmokeVerdict([]byte("ASCEND_ACL_SMOKE_OK"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ascendSmokeVerdict([]byte("aclrtSetDevice=507033"), errors.New("exit status 1")); err == nil || !strings.Contains(err.Error(), "507033") {
		t.Fatalf("failure verdict = %v", err)
	}
	if err := ascendSmokeVerdict([]byte("no marker"), nil); err == nil {
		t.Fatal("a successful exit without the marker must not pass")
	}
}

func TestEnvironmentWithReplacesRatherThanDuplicates(t *testing.T) {
	got := environmentWith([]string{"A=old", "B=keep", "A=second"}, map[string]string{"A": "new", "C": "added"})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "A=old") || strings.Contains(joined, "A=second") ||
		!strings.Contains(joined, "A=new") || !strings.Contains(joined, "B=keep") || !strings.Contains(joined, "C=added") {
		t.Fatalf("environment = %v", got)
	}
}

func TestAscendSmokeEnvExposesOnlyTheNamedDie(t *testing.T) {
	target := ascendSmokeTarget{Home: "/usr/local/Ascend/ascend-toolkit/latest", Library: "/usr/local/Ascend/ascend-toolkit/latest/aarch64-linux/lib64/libascendcl.so"}
	env := map[string]string{}
	for _, entry := range ascendSmokeEnv([]string{"PATH=/usr/bin", "LD_LIBRARY_PATH=/opt/other/lib", "ASCEND_RT_VISIBLE_DEVICES=0,1,2"}, "7", target) {
		name, value, _ := strings.Cut(entry, "=")
		if _, duplicate := env[name]; duplicate {
			t.Fatalf("%s appears twice; the child would read the wrong one", name)
		}
		env[name] = value
	}
	if env["ASCEND_RT_VISIBLE_DEVICES"] != "7" {
		t.Fatalf("visible devices = %q, want only the die under test", env["ASCEND_RT_VISIBLE_DEVICES"])
	}
	if env["ASCEND_ACL_LIBRARY"] != target.Library || env["ASCEND_HOME_PATH"] != target.Home {
		t.Fatalf("CANN location = %q / %q", env["ASCEND_HOME_PATH"], env["ASCEND_ACL_LIBRARY"])
	}
	for _, want := range []string{target.Home + "/lib64", filepath.Dir(target.Library), "/opt/other/lib"} {
		if !strings.Contains(env["LD_LIBRARY_PATH"], want) {
			t.Fatalf("LD_LIBRARY_PATH = %q, want it to contain %q", env["LD_LIBRARY_PATH"], want)
		}
	}
	for _, element := range strings.Split(env["LD_LIBRARY_PATH"], ":") {
		if element == "" {
			t.Fatalf("LD_LIBRARY_PATH = %q has an empty element, which the linker reads as the current directory", env["LD_LIBRARY_PATH"])
		}
	}
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("the rest of the environment was dropped: PATH=%q", env["PATH"])
	}
}

func TestAscendSmokeEnvBuildsALibraryPathWithoutEmptyElements(t *testing.T) {
	target := ascendSmokeTarget{Home: "/opt/cann", Library: "/opt/cann/lib64/libascendcl.so"}
	var got string
	for _, entry := range ascendSmokeEnv([]string{"PATH=/usr/bin"}, "0", target) {
		if name, value, _ := strings.Cut(entry, "="); name == "LD_LIBRARY_PATH" {
			got = value
		}
	}
	want := "/opt/cann/lib64:" + accelerator.AscendDriverLibraryPath
	if got != want {
		t.Fatalf("LD_LIBRARY_PATH = %q, want %q", got, want)
	}
}

func TestAscendSmokePrerequisiteNamesTheMissingHalf(t *testing.T) {
	requirePython(t)
	previousFinder := ascendSmokeTargetFinder
	t.Cleanup(func() { ascendSmokeTargetFinder = previousFinder })
	hostPath := os.Getenv("PATH")

	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) {
		return ascendSmokeTarget{Home: "/opt/cann", Library: "/opt/cann/lib64/libascendcl.so"}, true
	}
	t.Setenv("PATH", "")
	if reason := AscendCLSmokePrerequisite(); !strings.Contains(reason, "python3") {
		t.Fatalf("reason without python3 = %q", reason)
	}

	t.Setenv("PATH", hostPath)
	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) { return ascendSmokeTarget{}, false }
	if reason := AscendCLSmokePrerequisite(); !strings.Contains(reason, "libascendcl") {
		t.Fatalf("reason without CANN = %q", reason)
	}

	ascendSmokeTargetFinder = func() (ascendSmokeTarget, bool) {
		return ascendSmokeTarget{Home: "/opt/cann", Library: "/opt/cann/lib64/libascendcl.so"}, true
	}
	if reason := AscendCLSmokePrerequisite(); reason != "" {
		t.Fatalf("reason with both halves present = %q", reason)
	}
}

// Which vendors' runtimes can be asked about one card on the host, and with
// which tool and time limit: a driver that gains or loses the check, or
// answers with another vendor's, changes what the periodic check does.
func TestHostRuntimeCheckIsEachDriversOwn(t *testing.T) {
	for _, tc := range []struct {
		vendor  string
		cli     string
		timeout time.Duration
	}{
		{"nvidia", "nvidia-smi", 0}, {"NVIDIA", "nvidia-smi", 0},
		{"ascend", "npu-smi", 45 * time.Second}, {"ASCEND", "npu-smi", 45 * time.Second},
		{"hygon", "hy-smi", 0}, {"HYGON", "hy-smi", 0},
		{"ppu", "", 0}, {"amd", "", 0}, {"kunlunxin", "", 0}, {"apple", "", 0}, {"", "", 0},
	} {
		check, ok := HostRuntimeCheckFor(tc.vendor)
		if ok != (tc.cli != "") {
			t.Fatalf("HostRuntimeCheckFor(%q) = %v, want %v", tc.vendor, ok, tc.cli != "")
		}
		if !ok {
			continue
		}
		if check.CLI != tc.cli || check.Timeout != tc.timeout {
			t.Errorf("%q is checked with %q within %v, want %q within %v", tc.vendor, check.CLI, check.Timeout, tc.cli, tc.timeout)
		}
		if check.Try == nil || check.Visible == nil || check.Prerequisite == nil {
			t.Fatalf("%q has an incomplete check: %+v", tc.vendor, check)
		}
	}
}

func TestHostRuntimeCheckNamesACardTheWayItsRuntimeWants(t *testing.T) {
	nvidia, _ := HostRuntimeCheckFor("nvidia")
	if visible, ok := nvidia.Visible(proto.GPUInfo{Index: 3, UUID: "GPU-3f"}); !ok || visible != "GPU-3f" {
		t.Fatalf("NVIDIA card = %q/%v, want it named by UUID", visible, ok)
	}
	if _, ok := nvidia.Visible(proto.GPUInfo{Index: 3}); ok {
		t.Fatal("an NVIDIA card with no UUID must not be named by index")
	}

	ascend, _ := HostRuntimeCheckFor("ascend")
	if visible, ok := ascend.Visible(proto.GPUInfo{Index: 14, UUID: "ascend-14"}); !ok || visible != "14" {
		t.Fatalf("Ascend die = %q/%v, want the physical die identifier", visible, ok)
	}
	if _, ok := ascend.Visible(proto.GPUInfo{Index: -1, UUID: "ascend-x"}); ok {
		t.Fatal("a die with no index cannot be addressed and must not be asked")
	}
}

func TestAscendSmokeVerdictGradesTheHostApartFromTheDie(t *testing.T) {
	for _, test := range []struct {
		name            string
		out             string
		err             error
		wantUnavailable string
		wantFailure     string
	}{{
		name:            "library will not load",
		out:             "ASCEND_ACL_SMOKE_UNAVAILABLE: /opt/cann/lib64/libascendcl.so did not load: libascend_hal.so: cannot open shared object file",
		err:             errors.New("exit status 10"),
		wantUnavailable: "libascend_hal.so",
	}, {
		name:            "runtime will not initialize",
		out:             "ASCEND_ACL_SMOKE_UNAVAILABLE: aclInit=100039",
		err:             errors.New("exit status 10"),
		wantUnavailable: "aclInit=100039",
	}, {
		name:        "the die cannot take a context",
		out:         "ASCEND_ACL_SMOKE_DIE_FAILED: aclrtSetDevice=507033",
		err:         errors.New("exit status 1"),
		wantFailure: "507033",
	}, {
		name: "the die answered",
		out:  "ASCEND_ACL_SMOKE_OK\n",
	}, {
		name: "the die answered but would not let go",
		out:  "ASCEND_ACL_SMOKE_CLEANUP: aclrtResetDevice=507015\nASCEND_ACL_SMOKE_OK\n",
	}, {
		name: "the die answered and CANN crashed at exit",
		out:  "ASCEND_ACL_SMOKE_OK\n",
		err:  errors.New("signal: segmentation fault"),
	}, {
		name: "the die answered and the process exited non-zero",
		out:  "ASCEND_ACL_SMOKE_OK\nterminate called after throwing an instance\n",
		err:  errors.New("exit status 134"),
	}, {
		name:            "the check crashed without saying anything",
		err:             errors.New("signal: segmentation fault"),
		wantUnavailable: "segmentation fault",
	}, {
		name:            "the check said something this agent cannot grade",
		out:             "Traceback (most recent call last):\n  File \"<string>\", line 3\n",
		err:             errors.New("exit status 1"),
		wantUnavailable: "Traceback",
	}} {
		t.Run(test.name, func(t *testing.T) {
			err := ascendSmokeVerdict([]byte(test.out), test.err)
			unavailable := HostRuntimeUnavailable(err)
			switch {
			case test.wantUnavailable != "":
				if !strings.Contains(unavailable, test.wantUnavailable) {
					t.Fatalf("unavailable = %q, want it to name %q", unavailable, test.wantUnavailable)
				}
			case test.wantFailure != "":
				if unavailable != "" {
					t.Fatalf("a die verdict was graded as a host fault: %q", unavailable)
				}
				if err == nil || !strings.Contains(err.Error(), test.wantFailure) {
					t.Fatalf("failure = %v, want it to name %q", err, test.wantFailure)
				}
			default:
				if err != nil {
					t.Fatalf("verdict = %v, want the die to pass", err)
				}
			}
		})
	}
}

// ascendSmokeStub builds a shared library with the ACL entry points the check
// calls, each returning what its variable says, so the real Python script can
// be exercised without CANN.
func ascendSmokeStub(t *testing.T) ascendSmokeTarget {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("no C compiler to build the ACL stub with: %v", err)
		}
		t.Skip("no C compiler to build the ACL stub with")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "lib64"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(home, "stub.c")
	if err := os.WriteFile(source, []byte(`#include <stdlib.h>
#include <stddef.h>

static int answer(const char *name) {
	const char *value = getenv(name);
	return value == NULL ? 0 : atoi(value);
}

int aclInit(const char *config) { (void)config; return answer("STUB_ACL_INIT"); }
int aclrtSetDevice(int id) { (void)id; return answer("STUB_ACL_SET_DEVICE"); }
int aclrtCreateContext(void **context, int id) {
	(void)id;
	*context = (void *)0x1;
	return answer("STUB_ACL_CREATE_CONTEXT");
}
int aclrtMalloc(void **buffer, size_t size, int policy) {
	(void)size;
	(void)policy;
	*buffer = (void *)0x2;
	return answer("STUB_ACL_MALLOC");
}
int aclrtFree(void *buffer) { (void)buffer; return answer("STUB_ACL_FREE"); }
int aclrtDestroyContext(void *context) { (void)context; return answer("STUB_ACL_DESTROY_CONTEXT"); }
int aclrtResetDevice(int id) { (void)id; return answer("STUB_ACL_RESET_DEVICE"); }
int aclFinalize(void) { return answer("STUB_ACL_FINALIZE"); }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(home, "lib64", "libascendcl.so")
	build := exec.Command(compiler, "-shared", "-fPIC", "-o", library, source)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the ACL stub: %v\n%s", err, out)
	}
	return ascendSmokeTarget{Home: home, Library: library}
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("the Ascend runtime check needs python3: %v", err)
		}
		t.Skip("python3 is not installed")
	}
}

func TestAscendACLSmokeScriptGradesWhereTheFailureHappened(t *testing.T) {
	requirePython(t)
	target := ascendSmokeStub(t)
	for _, test := range []struct {
		name            string
		variable        string
		code            string
		wantUnavailable string
		wantFailure     string
		wantOutput      string
	}{{
		name: "a die that answers",
	}, {
		name: "the runtime will not initialize", variable: "STUB_ACL_INIT", code: "100039",
		wantUnavailable: "aclInit=100039",
	}, {
		name: "the die will not be taken", variable: "STUB_ACL_SET_DEVICE", code: "507033",
		wantFailure: "aclrtSetDevice=507033",
	}, {
		name: "the die takes no context", variable: "STUB_ACL_CREATE_CONTEXT", code: "507033",
		wantFailure: "aclrtCreateContext=507033",
	}, {
		name: "the die has no memory to give", variable: "STUB_ACL_MALLOC", code: "207001",
		wantFailure: "aclrtMalloc=207001",
	}, {
		name: "the die answered but would not reset", variable: "STUB_ACL_RESET_DEVICE", code: "507015",
		wantOutput: "ASCEND_ACL_SMOKE_CLEANUP: aclrtResetDevice=507015",
	}} {
		t.Run(test.name, func(t *testing.T) {
			if test.variable != "" {
				t.Setenv(test.variable, test.code)
			}
			out, runErr := runAscendSmokeCommand(context.Background(), "3", target)
			err := ascendSmokeVerdict(out, runErr)
			unavailable := HostRuntimeUnavailable(err)
			switch {
			case test.wantUnavailable != "":
				if !strings.Contains(unavailable, test.wantUnavailable) {
					t.Fatalf("unavailable = %q, want it to name %q\noutput: %s", unavailable, test.wantUnavailable, out)
				}
			case test.wantFailure != "":
				if unavailable != "" {
					t.Fatalf("a die verdict was graded as a host fault: %q\noutput: %s", unavailable, out)
				}
				if err == nil || !strings.Contains(err.Error(), test.wantFailure) {
					t.Fatalf("failure = %v, want it to name %q\noutput: %s", err, test.wantFailure, out)
				}
			default:
				if err != nil {
					t.Fatalf("verdict = %v, want the die to pass\noutput: %s", err, out)
				}
			}
			if test.wantOutput != "" && !strings.Contains(string(out), test.wantOutput) {
				t.Fatalf("output = %s, want it to report %q", out, test.wantOutput)
			}
		})
	}
}

func TestAscendACLSmokeScriptReportsABrokenInstallationAsUnavailable(t *testing.T) {
	requirePython(t)
	home := t.TempDir()
	missing := ascendSmokeTarget{Home: home, Library: filepath.Join(home, "lib64", "libascendcl.so")}
	out, runErr := runAscendSmokeCommand(context.Background(), "0", missing)
	if reason := HostRuntimeUnavailable(ascendSmokeVerdict(out, runErr)); !strings.Contains(reason, "did not load") {
		t.Fatalf("missing library: unavailable = %q\noutput: %s", reason, out)
	}

	locate, err := exec.Command("python3", "-c", "import ctypes.util; print(ctypes.util.find_library('c') or '')").Output()
	if err != nil {
		t.Fatal(err)
	}
	libc := strings.TrimSpace(string(locate))
	if libc == "" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("the C library could not be located to stand in for a wrong library")
		}
		t.Skip("the C library could not be located to stand in for a wrong library")
	}
	wrong := ascendSmokeTarget{Home: home, Library: libc}
	out, runErr = runAscendSmokeCommand(context.Background(), "0", wrong)
	if reason := HostRuntimeUnavailable(ascendSmokeVerdict(out, runErr)); !strings.Contains(reason, "has no acl") {
		t.Fatalf("wrong library: unavailable = %q\noutput: %s", reason, out)
	}
}
