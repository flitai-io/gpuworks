package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The NVIDIA checks only Doctor runs: driver/fabric-manager facts, a CUDA
// driver API smoke, peer access and an NCCL all-reduce. They are explicit and
// manual because the multi-GPU ones take tens of seconds.

// cudaInitProbe is the whole-node CUDA check as a command: the same script
// the check itself runs (accelerator.CUDASmokeScript), with every card
// visible to it. It says whether the driver initialises and the first
// visible card takes a context; each card is asked on its own afterwards
// (doctorCardRuntimeChecks), the way the periodic check asks it. cudaRecheck is the one-line version for a reader who only wants to
// see whether the driver initialises at all; it prints 0 when it does.
var cudaInitProbe = "python3 -c " + accelerator.ShellSingleQuote(accelerator.CUDASmokeScript)

const cudaRecheck = `python3 -c 'import ctypes; print(ctypes.CDLL("libcuda.so.1").cuInit(0))'`

// pythonCommand builds the peer-access and NCCL checks' python3 command,
// the same host-or-container way a vendor CLI runs: a Kubernetes DaemonSet
// pod's own image has no python3, so it must be the host's. A test
// overrides it to observe what these checks ask for without depending on
// gpu's own process-global switch (RunCLIsOnHost is one-way and shared by
// the whole test binary).
var pythonCommand = gpu.HostCommand

// cliOnHostForDoctor is gpu.CLIsOnHost, the same seaming reason as
// pythonCommand: a test overrides it to exercise runPythonFileDiagnostic's
// directory choice without touching the real, one-way global switch.
var cliOnHostForDoctor = gpu.CLIsOnHost

// createDiagnosticScript is os.CreateTemp; a test overrides it to observe
// which directory runPythonFileDiagnostic chose.
var createDiagnosticScript = os.CreateTemp

// doctorNVIDIAChecks runs the NVIDIA-only checks. stateDir is where the NCCL
// check's script file is written when CLIs run on the host (runPythonFileDiagnostic);
// "" falls back to os.TempDir(), which is fine off Kubernetes and for any
// caller that genuinely has no state directory to offer.
func doctorNVIDIAChecks(ctx context.Context, r *proto.DoctorReport, gpuCount int, stateDir string) {
	// The driver version is the one the inventory already reported, not a
	// fresh nvidia-smi: on a host where a card failed to initialize every
	// nvidia-smi waits on the driver lock for up to a minute, and the
	// inventory's answer comes from the agent's shared, cached queries.
	fabric := "driver=" + r.Accelerators.Driver + " " + runDiagnostic(ctx, "sh", "-lc", `set +e
fm="$(systemctl is-active nvidia-fabricmanager 2>/dev/null || systemctl is-active nvidia-fabricmanager.service 2>/dev/null || true)"
if [ -z "$fm" ] || [ "$fm" = "unknown" ]; then fm=not-found; fi
fmver="$(nvidia-fabricmanager --version 2>/dev/null | head -1 || true)"
if [ -z "$fmver" ]; then fmver=not-found; fi
printf 'fabric_manager=%s fabric_manager_version=%s\n' "$fm" "$fmver"`)
	cuda := doctorCUDACheck(ctx)
	peer, nccl := cudaDependentSkip, cudaDependentSkip
	// Peer access and NCCL initialize the same driver the CUDA check just
	// failed to (or could not try): they would only fail the same way, after
	// another wait on a driver that may be slow to say so. With fewer than
	// two cards they say that instead, without touching the driver.
	if strings.HasPrefix(cuda, "PASS") || gpuCount < 2 {
		peer = diagnosticPeerAccess(ctx, gpuCount)
		nccl = diagnosticNCCL(ctx, gpuCount, stateDir)
	}
	r.Facts = append(r.Facts,
		proto.DoctorFact{Code: proto.DoctorFactFabricManager, Value: fabric},
		proto.DoctorFact{Code: proto.DoctorFactCUDARuntime, Value: cuda},
		proto.DoctorFact{Code: proto.DoctorFactPeerAccess, Value: peer},
		proto.DoctorFact{Code: proto.DoctorFactNCCL, Value: nccl})
	if diagnosticFailed(cuda) {
		// The fabric manager state is a lead, not a verdict: only NVSwitch
		// systems need it, so it is shown next to the failure rather than
		// reported on its own.
		r.Findings = append(r.Findings, proto.DoctorFinding{
			Code: proto.DoctorCUDAInitFailed, Level: proto.DoctorLevelError,
			Probe:    cudaInitProbe,
			Evidence: []proto.DoctorField{{Name: "result", Value: cuda}, {Name: "driver/fabric", Value: fabric}},
			Recheck:  cudaRecheck,
		})
	}
	for _, check := range []struct{ probe, result string }{
		{"python3 -c " + accelerator.ShellSingleQuote(peerAccessScript), peer},
		// mp.spawn needs the script to be an importable __main__, so this
		// one runs from a file; the check does the same (runPythonFileDiagnostic).
		{"f=$(mktemp /tmp/gpuworks-doctor-XXXXXX.py) && cat > \"$f\" <<'GPUWORKS_NCCL'\n" +
			ncclScript + "\nGPUWORKS_NCCL\npython3 \"$f\"; rm -f \"$f\"", nccl},
	} {
		if diagnosticFailed(check.result) {
			r.Findings = append(r.Findings, proto.DoctorFinding{
				Code: proto.DoctorMultiGPUCheckFailed, Level: proto.DoctorLevelWarning,
				Probe: check.probe, Evidence: []proto.DoctorField{{Name: "result", Value: check.result}},
			})
		}
	}
}

// cudaDependentSkip is what the multi-GPU checks say when the CUDA check did
// not pass.
const cudaDependentSkip = "SKIP (the CUDA runtime check did not pass)"

func diagnosticFailed(result string) bool { return strings.HasPrefix(result, "FAIL") }

func runDiagnostic(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result == "" {
			result = err.Error()
		} else {
			result += ": " + err.Error()
		}
	}
	if result == "" {
		result = "not available"
	}
	return result
}

// doctorCUDACheck is diagnosticCUDA; a test stands in for the host's driver.
var doctorCUDACheck = diagnosticCUDA

func diagnosticCUDA(ctx context.Context) string {
	if reason := gpu.CUDASmokePrerequisite(); reason != "" {
		// Said as a skip, and said for the periodic per-card check too,
		// which needs the same two things: a check that never runs is
		// otherwise silent, and silence reads as a pass.
		return "SKIP (" + reason + "; the periodic per-card runtime check does not run either)"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := gpu.CUDARuntimeSmoke(probeCtx); err != nil {
		if reason := gpu.HostRuntimeUnavailable(err); reason != "" {
			return "SKIP (" + reason + ")"
		}
		return "FAIL: " + err.Error()
	}
	return "PASS cuInit=ok cuDeviceGet=ok cuCtxCreate=ok cuMemAlloc=4096B cuMemFree=ok cuCtxDestroy=ok"
}

const peerAccessScript = `import ctypes
lib=ctypes.CDLL("libcuda.so.1")
lib.cuInit.argtypes=[ctypes.c_uint]; lib.cuInit.restype=ctypes.c_int
lib.cuDeviceGetCount.argtypes=[ctypes.POINTER(ctypes.c_int)]; lib.cuDeviceGetCount.restype=ctypes.c_int
lib.cuDeviceGet.argtypes=[ctypes.POINTER(ctypes.c_int),ctypes.c_int]; lib.cuDeviceGet.restype=ctypes.c_int
lib.cuDeviceCanAccessPeer.argtypes=[ctypes.POINTER(ctypes.c_int),ctypes.c_int,ctypes.c_int]; lib.cuDeviceCanAccessPeer.restype=ctypes.c_int
def call(name, fn, *args):
    rc=fn(*args)
    if rc: raise RuntimeError("%s=%d" % (name, rc))
call("cuInit", lib.cuInit, 0)
n=ctypes.c_int(); call("cuDeviceGetCount", lib.cuDeviceGetCount, ctypes.byref(n))
if n.value<2:
    print("SKIP (driver reports fewer than 2 GPUs)"); raise SystemExit(0)
devs=[]
for i in range(n.value):
    d=ctypes.c_int(); call("cuDeviceGet", lib.cuDeviceGet, ctypes.byref(d), i); devs.append(d.value)
pairs=[]
for i,a in enumerate(devs):
    for j,b in enumerate(devs):
        if i==j: continue
        can=ctypes.c_int(); call("cuDeviceCanAccessPeer", lib.cuDeviceCanAccessPeer, ctypes.byref(can), a, b)
        pairs.append("%d->%d:%d" % (i,j,can.value))
print("PASS pairs=" + ",".join(pairs))`

func diagnosticPeerAccess(ctx context.Context, count int) string {
	if count < 2 {
		return "SKIP (fewer than 2 GPUs)"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return runPythonDiagnostic(probeCtx, peerAccessScript)
}

// ncclScript runs on every visible card. A node without torch cannot run it;
// that is a skip, not a failed check.
const ncclScript = `import os, socket, sys
try:
    import torch
except ImportError:
    print("SKIP (torch is not installed)"); sys.exit(0)
import torch.distributed as dist
import torch.multiprocessing as mp
def worker(rank, world, port):
    os.environ["MASTER_ADDR"]="127.0.0.1"; os.environ["MASTER_PORT"]=str(port)
    torch.cuda.set_device(rank)
    dist.init_process_group("nccl", rank=rank, world_size=world)
    x=torch.ones(1, device="cuda")
    dist.all_reduce(x)
    if abs(float(x.item())-world)>1e-4: raise RuntimeError("all_reduce result=%s" % x.item())
    dist.destroy_process_group()
if __name__ == "__main__":
    n=torch.cuda.device_count()
    if n<2:
        print("SKIP (fewer than 2 GPUs)"); raise SystemExit(0)
    s=socket.socket(); s.bind(("127.0.0.1",0)); port=s.getsockname()[1]; s.close()
    mp.spawn(worker, args=(n,port), nprocs=n, join=True)
    print("PASS world_size=%d all_reduce=ok" % n)`

func diagnosticNCCL(ctx context.Context, count int, stateDir string) string {
	if count < 2 {
		return "SKIP (fewer than 2 GPUs)"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	return runPythonFileDiagnostic(probeCtx, stateDir, ncclScript)
}

// runPythonDiagnostic runs under the node's runtime-check key, like every
// check that opens the driver (gpu.RuntimeCheckKey): a check stuck in the
// driver is never joined by another, however often Diagnose is clicked.
// pythonCommand places python3 the same way a vendor CLI runs (on the
// host, in a Kubernetes DaemonSet pod, whose image has no python3 of its
// own); -c needs no file, so there is nowhere for host and container paths
// to disagree.
func runPythonDiagnostic(ctx context.Context, script string) string {
	out, err := gpu.RunTracked(ctx, gpu.RuntimeCheckKey, 0, func(ctx context.Context) *exec.Cmd {
		return pythonCommand(ctx, "python3", "-c", script)
	})
	return formatDiagnosticOutput([]byte(out), err)
}

// runPythonFileDiagnostic runs a script that needs a real file (mp.spawn's
// "spawn" start method re-imports it as __main__, which a bare -c script
// cannot be). The file goes under dir — the state directory when CLIs run
// on the host, the same path there as in the agent's own container,
// because os.TempDir() is not: a privileged container's /tmp is its own,
// invisible to the host process pythonCommand runs this under.
func runPythonFileDiagnostic(ctx context.Context, dir, script string) string {
	if strings.TrimSpace(dir) == "" || !cliOnHostForDoctor() {
		dir = os.TempDir()
	}
	f, err := createDiagnosticScript(dir, "gpuworks-doctor-*.py")
	if err != nil {
		return "FAIL: cannot create diagnostic script: " + err.Error()
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		return "FAIL: cannot write diagnostic script: " + err.Error()
	}
	if err := f.Close(); err != nil {
		return "FAIL: cannot close diagnostic script: " + err.Error()
	}
	out, err := gpu.RunTracked(ctx, gpu.RuntimeCheckKey, 0, func(ctx context.Context) *exec.Cmd {
		return pythonCommand(ctx, "python3", path)
	})
	return formatDiagnosticOutput([]byte(out), err)
}

func formatDiagnosticOutput(out []byte, err error) string {
	if errors.Is(err, gpu.ErrStillRunning) {
		return "SKIP (an earlier runtime check on this node was killed and has not exited; nothing new is started until it does)"
	}
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result == "" {
			result = err.Error()
		} else {
			result += ": " + err.Error()
		}
		return "FAIL: " + result
	}
	if result == "" {
		return "FAIL: no diagnostic output"
	}
	return result
}
