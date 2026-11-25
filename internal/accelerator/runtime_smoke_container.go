package accelerator

import "strings"

// The runtime check run inside a container, for hosts that have the vendor's
// driver but not its runtime: PPU hosts carry only the management library
// (libhgml.so) and many Ascend hosts only the driver, without CANN. The
// runner images the platform deploys carry the runtime, so the check borrows
// one that is already on the node and asks the card from inside it, with the
// card injected the way a real instance gets it.

// ContainerSmokeFramework is the framework a runner image must be built for
// to ask this vendor's cards from inside it (the <backend> of the runner tag,
// as cmd/gpuworks-image-manifest spells it), and false for a vendor that has
// no container check.
func ContainerSmokeFramework(vendor string) (string, bool) {
	spec, _ := vendorSpecOf(vendor)
	return spec.smokeFramework, spec.smokeFramework != ""
}

// ContainerSmokeScript is the python the container runs for this vendor.
func ContainerSmokeScript(vendor string) (string, bool) {
	spec, _ := vendorSpecOf(vendor)
	return spec.smokeScript, spec.smokeScript != ""
}

// RunnerTagFramework is the framework a runner image reference names in its
// tag ("…/gpuworks-runner:hggc13.0-sglang0.5.13-r6" is hggc), or "".
func RunnerTagFramework(ref string) string {
	ref = strings.TrimSpace(ref)
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	tag := ref[colon+1:]
	for _, framework := range []string{"cuda", "cann", "dtk", "hggc", "metal"} {
		if rest, ok := strings.CutPrefix(tag, framework); ok && rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			return framework
		}
	}
	return ""
}

// ascendContainerPreamble finds libascendcl.so inside the image the way the
// host check finds it on the host, and hands it to AscendACLSmokeScript. An
// image without one is the host's problem, not the die's.
var ascendContainerPreamble = `import glob as _glob, os as _os


def _find_acl():
    patterns = [_os.environ.get("ASCEND_HOME_PATH", "")] + ` + pythonList(AscendToolkitHomes) + `
    for pattern in patterns:
        for home in sorted(_glob.glob(pattern)) if pattern else []:
            for lib in ` + pythonList(AscendACLLibraryPaths) + `:
                if _os.path.isfile(_os.path.join(home, lib)):
                    return _os.path.join(home, lib)
    return ""


if not _os.environ.get("ASCEND_ACL_LIBRARY"):
    _os.environ["ASCEND_ACL_LIBRARY"] = _find_acl()
if not _os.environ["ASCEND_ACL_LIBRARY"]:
    print("ASCEND_ACL_SMOKE_UNAVAILABLE: the image has no libascendcl.so", flush=True)
    raise SystemExit(10)
`

func pythonList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = `"` + v + `"`
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// CUDAGradedSmokeScript is CUDASmokeScript with its failures graded the way
// the ACL and HIP checks grade theirs. Inside a container that sees only the
// one injected card, a runtime that will not load or initialize says more
// about the image and the injection than about the card, so it is the host's;
// failing to take the card or allocate on it is the card's.
const CUDAGradedSmokeScript = `import ctypes


def report(marker, detail, code):
    print("%s: %s" % (marker, detail), flush=True)
    raise SystemExit(code)


def unavailable(detail):
    report("CUDA_RUNTIME_SMOKE_UNAVAILABLE", detail, 10)


def card_failed(detail):
    report("CUDA_RUNTIME_SMOKE_CARD_FAILED", detail, 1)


try:
    lib = ctypes.CDLL("libcuda.so.1")
except OSError as exc:
    unavailable("libcuda.so.1 did not load: %s" % exc)
for name, argtypes in (
    ("cuInit", [ctypes.c_uint]),
    ("cuDeviceGet", [ctypes.POINTER(ctypes.c_int), ctypes.c_int]),
    ("cuCtxCreate_v2", [ctypes.POINTER(ctypes.c_void_p), ctypes.c_uint, ctypes.c_int]),
    ("cuMemAlloc_v2", [ctypes.POINTER(ctypes.c_uint64), ctypes.c_size_t]),
    ("cuMemFree_v2", [ctypes.c_uint64]),
    ("cuCtxDestroy_v2", [ctypes.c_void_p]),
):
    try:
        entry = getattr(lib, name)
    except AttributeError:
        unavailable("libcuda.so.1 has no %s" % name)
    entry.argtypes = argtypes
    entry.restype = ctypes.c_int

rc = lib.cuInit(0)
if rc:
    unavailable("cuInit=%d" % rc)
device = ctypes.c_int()
rc = lib.cuDeviceGet(ctypes.byref(device), 0)
if rc:
    unavailable("cuDeviceGet=%d" % rc)
context = ctypes.c_void_p()
rc = lib.cuCtxCreate_v2(ctypes.byref(context), 0, device)
if rc:
    card_failed("cuCtxCreate=%d" % rc)
pointer = ctypes.c_uint64()
rc = lib.cuMemAlloc_v2(ctypes.byref(pointer), 4096)
if rc:
    card_failed("cuMemAlloc=%d" % rc)
for name, call in (("cuMemFree", lambda: lib.cuMemFree_v2(pointer.value)), ("cuCtxDestroy", lambda: lib.cuCtxDestroy_v2(context))):
    rc = call()
    if rc:
        print("CUDA_RUNTIME_SMOKE_CLEANUP: %s=%d" % (name, rc), flush=True)
print("CUDA_RUNTIME_SMOKE_OK", flush=True)`
