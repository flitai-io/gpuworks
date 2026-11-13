package logline

import "testing"

// The samples are literal lines from vLLM, uvicorn, SGLang, logrus-style and
// Ascend toolkit output (see the live-store analysis in
// artifacts/archive/2026-09-19-log-center/) — the parser must classify engines as they
// actually write, not a tidy idealized format.
func TestLevel(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"(APIServer pid=97413) INFO 09-18 20:41:29 [utils.py:299] vLLM API server", LevelInfo},
		{"(APIServer pid=97413) WARNING 09-18 20:41:29 [envs.py:1830] Unknown vLLM environment variable detected: VLLM_VERSION", LevelWarn},
		{"(APIServer pid=83923) [ERROR] 2026-09-18-20:39:28 (PID:83923, Device:-1, RankID:-1) ERR99999 UNKNOWN applicaiton exception", LevelError}, //nolint:misspell // Preserve the exact error text emitted by the engine fixture.
		{`(APIServer pid=97413) pydantic_core._pydantic_core.ValidationError: 1 validation error for ModelConfig`, ""},
		{`time="2026-09-18T20:48:25+08:00" level=fatal msg="no such container gpuworks-5a39472d97ba"`, LevelFatal},
		{"level=warning runtime is deprecated", LevelWarn},
		{`level: "error" something failed`, LevelError},
		{"INFO:     127.0.0.1:34930 - \"GET /v1/models HTTP/1.1\" 200 OK", LevelInfo},
		{"(EngineCore pid=7362) INFO 09-18 10:51:17 [core.py:1501] [shutdown] EngineCore: start mode=abort timeout=0s", LevelInfo},
		{"Traceback (most recent call last):", LevelError},
		{"DEBUG urllib3.connectionpool http://host:8000 \"/v1/chat\"", LevelDebug},
		{"executing: vllm", ""},
		{"******************************************************************************", ""},
		{"* STARTING gpuworks-2fdc2d0f28da *", ""},
		{"the word error in lowercase prose is not a stamp", ""},
		{"non-default args: {'model': '/model', 'gpu_memory_utilization': 0.9}", ""},
		{"[2026-09-18 20:41:31][ERROR][pid=42] backend unreachable", LevelError},
		{"Error while reading config", LevelError},
	}
	for _, tc := range cases {
		if got := Level(tc.line); got != tc.want {
			t.Errorf("Level(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestIsNoise(t *testing.T) {
	noise := []string{
		`(APIServer pid=1) INFO:     127.0.0.1:49254 - "GET /metrics HTTP/1.1" 200 OK`,
		`INFO:     10.0.0.9:33210 - "GET /health HTTP/1.1" 200 OK`,
		`INFO: 127.0.0.1:40000 - "GET /healthz" 200 OK`,
		`INFO: 127.0.0.1:40001 - "GET /ready HTTP/1.1" 200 OK`,
		`INFO: 127.0.0.1:40002 - "GET /openapi.json HTTP/1.1" 200 OK`,
		`INFO: 127.0.0.1:40003 - "GET /docs HTTP/1.1" 200 OK`,
		`INFO: 127.0.0.1:40004 - "HEAD /version HTTP/1.1" 200 OK`,
	}
	keep := []string{
		`(APIServer pid=97413) INFO 09-18 20:41:29 [api_server.py:1400] Received request chatcmpl-1: prompt: 'Write a poem.', params:`,
		`INFO:     127.0.0.1:34930 - "POST /v1/completions HTTP/1.1" 200 OK`,
		`INFO:     127.0.0.1:34930 - "GET /v1/models HTTP/1.1" 200 OK`,
		`INFO:     127.0.0.1:34930 - "GET /metrics_summary HTTP/1.1" 200 OK`,
		`INFO:     127.0.0.1:34930 - "GET /v1/metrics HTTP/1.1" 200 OK`,
	}
	for _, line := range noise {
		if !IsNoise(line) {
			t.Errorf("IsNoise(%q) = false, want true (scrape endpoint)", line)
		}
	}
	for _, line := range keep {
		if IsNoise(line) {
			t.Errorf("IsNoise(%q) = true, want false (application traffic)", line)
		}
	}
}
