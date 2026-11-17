package enginelog

import (
	"reflect"
	"strings"
	"testing"
)

// Every signature fires on a real-format line and stays quiet on a near miss
// a healthy engine prints.
func TestMatchRecognisesEachSignature(t *testing.T) {
	cases := []struct {
		code string
		hit  string
		miss string
	}{
		{CodeGLIBCTooOld, "libc.so.6: version `GLIBC_2.33' not found (required by /opt/libvruntime.so)", "INFO: glibc 2.35 detected"},
		{CodeDGPUMemoryLimit, "[HAMI-core ERROR (pid:9 thread=1 allocator.c:52)]: Device 1 OOM 9663676416 / 8589934592", "[HAMI-core Info(9:1:allocator.c:50)]: _usage=1 limit=2 new_allocated=1"},
		{CodeKVCacheInsufficient, "ValueError: The model's max seq len (32768) is larger than the maximum number of tokens that can be stored in KV cache (12000).", "INFO: # GPU blocks: 4096, maximum number of tokens that can be stored in KV cache: 65536"},
		{CodeCUDAOOM, "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB.", "INFO: Memory profiling takes 3.2 seconds"},
		{CodeSHMInsufficient, "RuntimeError: unable to open shared memory object </torch_1_2> in read-write mode", "INFO: shm broadcast enabled"},
		{CodeNCCLFailure, "torch.distributed.DistBackendError: NCCL error in: ProcessGroupNCCL.cpp:1970, unhandled system error, ncclSystemError", "INFO: NCCL version 2.21.5+cuda12.4"},
		{CodeDiskFull, "OSError: [Errno 28] No space left on device: '/root/.cache/huggingface'", "INFO: Disk usage 40%"},
		{CodePortInUse, "ERROR: [Errno 98] error while attempting to bind on address ('0.0.0.0', 8000): address already in use", "INFO: Uvicorn running on http://0.0.0.0:8000"},
		{CodeImagePullDenied, "Error response from daemon: pull access denied for registry.local/vllm, repository does not exist or may require 'docker login'", "Status: Downloaded newer image for vllm/vllm-openai:latest"},
		{CodeDtypeUnsupported, "ValueError: Bfloat16 is only supported on GPUs with compute capability of at least 8.0. Your Tesla T4 GPU has compute capability 7.5.", "INFO: dtype=torch.bfloat16"},
		{CodeGPUArchUnsupported, "RuntimeError: CUDA error: no kernel image is available for execution on the device", "INFO: Using FlashAttention backend"},
		{CodeTrustRemoteCodeRequired, "ValueError: Loading /models/x requires you to execute the configuration file in that repo on your local machine.", "INFO: Loading model weights"},
		{CodeModelArchUnsupported, "ValueError: Model architectures ['FooForCausalLM'] are not supported for now.", "INFO: Resolved architecture: Qwen3ForCausalLM"},
		{CodeModelFilesMissing, "OSError: /models/qwen does not appear to have a file named config.json.", "INFO: Loading config.json"},
		{CodeHFDownloadFailed, "huggingface_hub.errors.RepositoryNotFoundError: 401 Client Error. Repository Not Found for url: https://huggingface.co/api/models/x", "INFO: Downloading from https://huggingface.co"},
	}
	covered := map[string]bool{CodeNoOutput: true}
	for _, c := range cases {
		covered[c.code] = true
		got := Match([]string{c.hit})
		if len(got) == 0 || got[0].Code != c.code {
			t.Errorf("%s: Match(hit) = %+v", c.code, got)
		}
		for _, f := range Match([]string{c.miss}) {
			t.Errorf("%s: near miss %q matched %s", c.code, c.miss, f.Code)
		}
	}
	for _, code := range Codes() {
		if !covered[code] {
			t.Errorf("code %s has no test case", code)
		}
		if Hypothesis(code) == "" {
			t.Errorf("code %s has no hypothesis", code)
		}
		if code != strings.ToUpper(code) {
			t.Errorf("code %s is not UPPER_SNAKE", code)
		}
	}
	if Hypothesis("CONTAINER_EXITED") != "" {
		t.Error("a code outside the table has a hypothesis")
	}
}

// The dynamic loader's refusal names the missing symbol version, in either
// spelling a loader prints.
func TestMatchReadsTheMissingGLIBC(t *testing.T) {
	for _, line := range []string{
		"libc.so.6: version `GLIBC_2.33' not found (required by /opt/libvruntime.so)",
		"/lib64/libc.so.6: glibc_2.34 not found",
	} {
		got := Match([]string{line})
		if len(got) != 1 || got[0].Code != CodeGLIBCTooOld || !strings.HasPrefix(got[0].GLIBC, "GLIBC_2.3") {
			t.Fatalf("Match(%q) = %+v", line, got)
		}
		if !strings.Contains(got[0].Fallback(), "glibc too old: missing "+got[0].GLIBC) {
			t.Fatalf("fallback = %q", got[0].Fallback())
		}
	}
}

// A refusal by the dGPU interposer is reported as the limit it enforces, read
// from the last refusal (an engine retries smaller first).
func TestMatchReadsTheLastDGPURefusal(t *testing.T) {
	lines := []string{
		"[HAMI-core ERROR (pid:9 thread=1 allocator.c:52)]: Device 1 OOM 9663676416 / 8589934592",
		"INFO retrying",
		"[HAMI-core ERROR (pid:9 thread=1 allocator.c:52)]: Device 1 OOM 9126805504 / 8589934592",
	}
	got := Match(lines)
	if len(got) != 1 {
		t.Fatalf("Match = %+v", got)
	}
	c := got[0]
	if c.Code != CodeDGPUMemoryLimit || c.Device != 1 || c.NeededBytes != 9126805504 || c.LimitBytes != 8589934592 {
		t.Fatalf("cause = %+v", c)
	}
	if want := "dGPU memory limit reached on GPU 1: the engine needed 8.5 GiB, its limit is 8.0 GiB"; c.Fallback() != want {
		t.Fatalf("fallback = %q, want %q", c.Fallback(), want)
	}
	if !reflect.DeepEqual(c.Lines(), []string{lines[0], lines[2]}) || c.Evidence[1].Index != 2 {
		t.Fatalf("evidence = %+v", c.Evidence)
	}
}

// An engine that merely uses /dev/shm (a KV offload file, Ray's object store
// notice) is not a shared-memory failure; HCCL's failure is a collective one.
func TestMatchIgnoresInformationalShmMentions(t *testing.T) {
	for _, line := range []string{
		"(EngineCore) INFO [shared_offload_region.py:71] Created mmap file /dev/shm/vllm_offload_1.mmap (4.29 GB)",
		"INFO: shm broadcast enabled",
	} {
		if got := Match([]string{line}); len(got) != 0 {
			t.Errorf("Match(%q) = %+v", line, got)
		}
	}
	got := Match([]string{"HCCL function error: hcclCommInitRootInfoConfig(numRanks, &rootInfo, rank, config, &(comm->hcclComm_)), error code is 1"})
	if len(got) != 1 || got[0].Code != CodeNCCLFailure {
		t.Fatalf("HCCL failure = %+v, want %s", got, CodeNCCLFailure)
	}
}

// "No space left on device" on /dev/shm is a shared-memory problem, not a
// full disk.
func TestMatchSeparatesShmFromDiskFull(t *testing.T) {
	got := Match([]string{"OSError: [Errno 28] No space left on device: '/dev/shm/nccl-abc'"})
	if len(got) != 1 || got[0].Code != CodeSHMInsufficient {
		t.Fatalf("Match = %+v, want %s only", got, CodeSHMInsufficient)
	}
}

func TestMatchKeepsNewestEvidenceAndIndexes(t *testing.T) {
	lines := []string{"start", "CUDA out of memory #1", "x", "CUDA out of memory #2", "CUDA out of memory #3", "CUDA out of memory #4"}
	got := Match(lines)
	if len(got) != 1 || len(got[0].Evidence) != MaxEvidence {
		t.Fatalf("Match = %+v", got)
	}
	if ev := got[0].Evidence; ev[0].Index != 3 || ev[2].Index != 5 || ev[2].Line != "CUDA out of memory #4" {
		t.Fatalf("evidence = %+v, want the newest three with their indexes", ev)
	}
	if Match(nil) != nil {
		t.Fatal("Match(nil) should report nothing")
	}
}

// Classify reads a stopped container's whole output: an empty one is a cause
// of its own, colour codes do not hide a match, and output without a known
// failure has no cause.
func TestClassifyReadsAStoppedContainersOutput(t *testing.T) {
	if got := Classify(" \n"); len(got) != 1 || got[0].Code != CodeNoOutput || got[0].Evidence != nil {
		t.Fatalf("Classify(empty) = %+v", got)
	}
	got := Classify("INFO start\n\x1b[31mtorch.OutOfMemoryError: CUDA out of memory\x1b[0m\n")
	if len(got) != 1 || got[0].Code != CodeCUDAOOM || got[0].Evidence[0].Line != "torch.OutOfMemoryError: CUDA out of memory" || got[0].Evidence[0].Index != 1 {
		t.Fatalf("Classify(oom) = %+v", got)
	}
	if got := Classify("RuntimeError: Failed to infer device type\n"); got != nil {
		t.Fatalf("Classify(unknown) = %+v", got)
	}
}
