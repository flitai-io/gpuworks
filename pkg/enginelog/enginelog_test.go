package enginelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const realVLLMLog = `INFO 08-18 10:00:01 [config.py:123] Initializing vllm engine
(EngineCore_0 pid=4123) INFO 08-18 10:00:12 [model_runner.py:1024] Model loading took 14.9889 GiB and 4.32 seconds
(EngineCore_0 pid=4123) INFO 08-18 10:00:20 [worker.py:210] the model weights take 14.99 GiB; non_torch_memory takes 0.30 GiB; PyTorch activation peak memory takes 1.20 GiB; the rest of the memory reserved for KV Cache is 60.20 GiB.
(EngineCore_0 pid=4123) INFO 08-18 10:00:21 [gpu_worker.py:88] Available KV cache memory: 60.20 GiB
INFO 08-18 10:00:22 [executor.py:55] GPU KV cache size: 1,234,567 tokens
INFO 08-18 10:00:22 [executor.py:56] Maximum concurrency for 32768 tokens per request: 12.34x
INFO 08-18 10:00:23 [cache_engine.py:77] # GPU blocks: 27000, # CPU blocks: 4096
(EngineCore_0 pid=4123) INFO 08-18 10:00:35 [model_runner.py:1600] Graph capturing finished in 12 secs, took 0.58 GiB`

func TestParseVLLMDialect(t *testing.T) {
	m := ParseDialect("vllm", realVLLMLog)
	if !m.Found {
		t.Fatal("Found = false, want true")
	}
	if m.ModelWeightsGiB != 14.9889 {
		t.Errorf("ModelWeightsGiB = %v, want 14.9889", m.ModelWeightsGiB)
	}
	if m.KVCacheGiB != 60.20 {
		t.Errorf("KVCacheGiB = %v, want 60.20", m.KVCacheGiB)
	}
	if m.KVCacheTokens != 1234567 {
		t.Errorf("KVCacheTokens = %v, want 1234567", m.KVCacheTokens)
	}
	if m.MaxConcurrency != 12.34 {
		t.Errorf("MaxConcurrency = %v, want 12.34", m.MaxConcurrency)
	}
	if m.NumGPUBlocks != 27000 {
		t.Errorf("NumGPUBlocks = %v, want 27000", m.NumGPUBlocks)
	}
	if m.GraphCaptureGiB != 0.58 {
		t.Errorf("GraphCaptureGiB = %v, want 0.58", m.GraphCaptureGiB)
	}
}

func TestParseUnknownDialectFallsBackToVLLM(t *testing.T) {
	m := ParseDialect("unknown", `Model loading took 8.5 GiB and 2.10 seconds`)
	if !m.Found || m.ModelWeightsGiB != 8.5 {
		t.Fatalf("unexpected result: %+v", m)
	}
}

func TestParseEmptyAndGarbage(t *testing.T) {
	for _, in := range []string{"", "   \n\t", "nothing to see here\nrandom log line 42\n"} {
		m := Parse(in)
		if m.Found {
			t.Errorf("Parse(%q).Found = true, want false", in)
		}
		if m != (Measured{}) {
			t.Errorf("Parse(%q) = %+v, want zero", in, m)
		}
	}
}

// The line a failed engine's output ends with is its last error, found among
// the final lines and stripped of colour codes; an ordinary last line says
// nothing about the failure and gives none.
func TestLastErrorIsTheFinalErrorLine(t *testing.T) {
	long := strings.Repeat("x", 400)
	for name, tc := range map[string]struct{ logs, want string }{
		"python exception":   {"Traceback (most recent call last):\n  File \"x\", line 1\n\x1b[31mRuntimeError: Failed to infer device type\x1b[0m\n\n", "RuntimeError: Failed to infer device type"},
		"argparse":           {"usage: vllm\nvllm: error: unrecognized arguments: --x\n", "vllm: error: unrecognized arguments: --x"},
		"prefixed exception": {"(EngineCore pid=1) torch.OutOfMemoryError: CUDA out of memory\nINFO shutting down\n", "(EngineCore pid=1) torch.OutOfMemoryError: CUDA out of memory"},
		"ordinary last line": {"INFO serving\nINFO 200 GET /health\n", ""},
		"nothing":            {"", ""},
		"bounded":            {"ValueError: " + long, "ValueError: " + long[:288] + "…"},
	} {
		if got := LastError(tc.logs); got != tc.want {
			t.Errorf("%s: LastError = %q, want %q", name, got, tc.want)
		}
	}
	// Only the final lines are searched: an error long before the end is history.
	if got := LastError("ValueError: early\n" + strings.Repeat("INFO line\n", 60)); got != "" {
		t.Errorf("an error before the final lines = %q", got)
	}
}

// vLLM ends a failed start with the API server's "Engine core
// initialization failed. See root cause above." The cause is a worker's or
// the engine core's exception further up, each rank repeating it. Both
// fixtures are this repository's 8 × B300 Kimi-K3.1-Pro failures; the worker
// tracebacks of the first are verbatim, the wrapper tail after them is the
// shape vLLM prints.
func TestRootCauseLooksPastVLLMWrapperExceptions(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"vllm-max-num-seqs-failure.txt", "ValueError: max_num_seqs (1024) exceeds available Mamba cache blocks (839). Each decode sequence requires one Mamba cache block"},
		{"vllm-kv-cache-failure.txt", "ValueError: To serve at least one request with the models's max seq len (1048576), (16.92 GiB KV cache is needed, which is larger than the available KV cache memory (14.21 GiB)."},
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if last := LastError(string(raw)); !strings.Contains(last, "See root cause above") {
			t.Fatalf("%s: fixture no longer ends on the wrapper: %q", tc.file, last)
		}
		got := RootCause(string(raw))
		if !strings.HasPrefix(got, tc.want) || len([]rune(got)) > lastErrorMax+1 {
			t.Errorf("%s: RootCause = %q, want it to start %q", tc.file, got, tc.want)
		}
	}
}

func TestRootCauseKeepsTheFinalErrorUnlessItIsAWrapper(t *testing.T) {
	for name, tc := range map[string]struct{ logs, want string }{
		// An exception handled long before the end is not the cause of an
		// argparse failure.
		"final error is not a wrapper": {"ValueError: handled during load\nusage: vllm\nvllm: error: unrecognized arguments: --x\n", "vllm: error: unrecognized arguments: --x"},
		"only wrappers":                {"RuntimeError: Engine core initialization failed. See root cause above.\n", "RuntimeError: Engine core initialization failed. See root cause above."},
		"dotted exception under tag":   {"(Worker_TP0 pid=9) ERROR 11-05 13:00:00 [w.py:1] torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB\n(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above. Failed core proc(s): {}\n", "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB"},
		"nothing":                      {"INFO serving\n", ""},
	} {
		if got := RootCause(tc.logs); got != tc.want {
			t.Errorf("%s: RootCause = %q, want %q", name, got, tc.want)
		}
	}
}
