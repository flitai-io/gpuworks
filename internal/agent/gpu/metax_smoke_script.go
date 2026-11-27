package gpu

// metaxHostSmokeScript selects a card by PCI address and asks the MACA
// runtime to inspect and allocate memory on it.
const metaxHostSmokeScript = `import ctypes, os

library = os.environ.get("MACA_SMOKE_LIBRARY", "")
if not library:
    candidates = []
    maca = os.environ.get("MACA_PATH", "")
    if maca:
        candidates.append(os.path.join(maca, "lib", "libmcruntime.so"))
    candidates += ["/opt/maca/lib/libmcruntime.so", "/opt/maca/lib64/libmcruntime.so", "/usr/local/maca/lib/libmcruntime.so"]
    library = next((p for p in candidates if os.path.isfile(p)), "")
if not library:
    print("MACA_RUNTIME_SMOKE_UNAVAILABLE: the host has no libmcruntime.so", flush=True)
    raise SystemExit(10)

try:
    lib = ctypes.CDLL(library)
    lib.mcInit.argtypes = [ctypes.c_uint]
    lib.mcInit.restype = ctypes.c_int
    lib.mcGetDeviceCount.argtypes = [ctypes.POINTER(ctypes.c_int)]
    lib.mcGetDeviceCount.restype = ctypes.c_int
    lib.mcDeviceGetByPCIBusId.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_char_p]
    lib.mcDeviceGetByPCIBusId.restype = ctypes.c_int
    lib.mcSetDevice.argtypes = [ctypes.c_int]
    lib.mcSetDevice.restype = ctypes.c_int
    lib.mcMalloc.argtypes = [ctypes.POINTER(ctypes.c_void_p), ctypes.c_size_t]
    lib.mcMalloc.restype = ctypes.c_int
    lib.mcFree.argtypes = [ctypes.c_void_p]
    lib.mcFree.restype = ctypes.c_int
    lib.mcMemGetInfo.argtypes = [ctypes.POINTER(ctypes.c_size_t), ctypes.POINTER(ctypes.c_size_t)]
    lib.mcMemGetInfo.restype = ctypes.c_int
except (OSError, AttributeError) as exc:
    print("MACA_RUNTIME_SMOKE_UNAVAILABLE: %s" % exc, flush=True)
    raise SystemExit(10)

rc = lib.mcInit(0)
if rc:
    print("MACA_RUNTIME_SMOKE_UNAVAILABLE: mcInit=%d" % rc, flush=True)
    raise SystemExit(10)
count = ctypes.c_int()
rc = lib.mcGetDeviceCount(ctypes.byref(count))
if rc or count.value < 1:
    print("MACA_RUNTIME_SMOKE_UNAVAILABLE: mcGetDeviceCount rc=%d count=%d" % (rc, count.value), flush=True)
    raise SystemExit(10)

bdf = os.environ.get("MACA_SMOKE_BDF", "").strip()
if not bdf:
    print("MACA_RUNTIME_SMOKE_UNAVAILABLE: no PCI address supplied", flush=True)
    raise SystemExit(10)
device = ctypes.c_int(-1)
rc = lib.mcDeviceGetByPCIBusId(ctypes.byref(device), bdf.encode())
if rc or device.value < 0:
    print("MACA_RUNTIME_SMOKE_DIE_FAILED: %s is not usable by the runtime (rc=%d)" % (bdf, rc), flush=True)
    raise SystemExit(1)
rc = lib.mcSetDevice(device.value)
if rc:
    print("MACA_RUNTIME_SMOKE_DIE_FAILED: mcSetDevice on %s rc=%d" % (bdf, rc), flush=True)
    raise SystemExit(1)
free, total = ctypes.c_size_t(), ctypes.c_size_t()
rc = lib.mcMemGetInfo(ctypes.byref(free), ctypes.byref(total))
if rc or total.value == 0:
    print("MACA_RUNTIME_SMOKE_DIE_FAILED: %s mcMemGetInfo rc=%d total=%d" % (bdf, rc, total.value), flush=True)
    raise SystemExit(1)
buffer = ctypes.c_void_p()
rc = lib.mcMalloc(ctypes.byref(buffer), 1 << 20)
if rc or not buffer.value:
    print("MACA_RUNTIME_SMOKE_DIE_FAILED: mcMalloc on %s rc=%d" % (bdf, rc), flush=True)
    raise SystemExit(1)
rc = lib.mcFree(buffer)
if rc:
    print("MACA_RUNTIME_SMOKE_DIE_FAILED: mcFree on %s rc=%d" % (bdf, rc), flush=True)
    raise SystemExit(1)
print("MACA_RUNTIME_SMOKE_OK", flush=True)
`
