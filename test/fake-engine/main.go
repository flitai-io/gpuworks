// fake-engine is a tiny deterministic HTTP inference process used by L1/L3
// tests. It deliberately accepts both vLLM and SGLang-shaped argv.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/test/fake-engine/handler"
)

func main() {
	args := append([]string(nil), os.Args[1:]...)
	dialect := valueArg(args, "--dialect")
	if dialect == "" {
		dialect = "vllm"
	}
	if dialect != "vllm" && dialect != "sglang" {
		fmt.Fprintln(os.Stderr, "fake-engine: invalid dialect")
		os.Exit(2)
	}
	_ = os.MkdirAll("/tmp", 0o755)
	_ = os.WriteFile("/tmp/fake-engine-argv.json", mustJSON(args), 0o644)
	if os.Getenv("FAKE_ENGINE_CRASH_ON_START") == "1" {
		os.Exit(42)
	}
	if os.Getenv("FAKE_ENGINE_OOM_EXIT") == "1" {
		// Keep the failure distinguishable from an arbitrary SIGKILL. The agent
		// uses this dialect marker when a runtime cannot expose OOMKilled state.
		fmt.Fprintln(os.Stderr, "torch.cuda.OutOfMemoryError: CUDA out of memory")
		os.Exit(137)
	}
	if d := os.Getenv("FAKE_ENGINE_STARTUP_DELAY"); d != "" {
		if n, err := time.ParseDuration(d); err == nil {
			time.Sleep(n)
		}
	}
	model := valueArg(args, "--model", "--model-path")
	if model == "" {
		for i, arg := range args {
			if (arg == "serve" || arg == "launch_server") && i+1 < len(args) {
				for _, candidate := range args[i+1:] {
					if !strings.HasPrefix(candidate, "-") {
						model = candidate
						break
					}
				}
				if model != "" {
					break
				}
			}
		}
	}
	if model == "" {
		fmt.Fprintln(os.Stderr, "fake-engine: missing --model/--model-path")
		os.Exit(2)
	}
	if st, err := os.Stat(filepath.Join(model, "config.json")); err != nil || st.IsDir() {
		fmt.Fprintf(os.Stderr, "fake-engine: model %q has no config.json\n", model)
		os.Exit(2)
	}
	if err := prepareOffloadTiers(valueArg(args, "--kv-transfer-config")); err != nil {
		// vLLM's filesystem tier writes a configuration file into root_dir while
		// it is being constructed and fails to start when it cannot. A fixture
		// that accepted the flag and wrote nothing would pass a deployment whose
		// managed directory the engine user cannot actually write to.
		fmt.Fprintf(os.Stderr, "fake-engine: KV offload tier unusable: %v\n", err)
		os.Exit(2)
	}
	port := valueArg(args, "--port")
	if port == "" {
		port = "8000"
	}
	// Engine-style startup banner: the product ships container stdout to the
	// log store, so the fixture must produce the same shape of lines a real
	// engine logs while booting.
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stdout, "INFO %s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
	logf("Starting fake-engine API server (dialect=%s) on 0.0.0.0:%s", dialect, port)
	logf("Loaded model weights from %s", model)
	// Like vLLM and SGLang, the engine answers under --served-model-name when
	// one is given (model list, responses, metric labels), else under the path.
	served := handler.ServedModel(args, model)
	mux := handler.New(handler.Options{
		Model: served, Voices: strings.FieldsFunc(os.Getenv("FAKE_ENGINE_VOICES"), func(r rune) bool { return r == ',' }),
		Health503: os.Getenv("FAKE_ENGINE_HEALTH_503") == "1", TTFTDelay: envDuration("FAKE_ENGINE_TTFT_DELAY"), TPOTDelay: envDuration("FAKE_ENGINE_TPOT_DELAY"),
		PromptTokens: envInt("FAKE_ENGINE_PROMPT_TOKENS", 16), CompletionTokens: envInt("FAKE_ENGINE_COMPLETION_TOKENS", 8), CachedTokens: envInt("FAKE_ENGINE_CACHED_TOKENS", 0), StreamChunks: envInt("FAKE_ENGINE_STREAM_CHUNKS", 1),
		ResponseText:                         os.Getenv("FAKE_ENGINE_RESPONSE_TEXT"),
		Logf:                                 logf,
		Dialect:                              dialect,
		MetricsObservationsPerScrape:         envInt("FAKE_ENGINE_METRICS_OBSERVATIONS_PER_SCRAPE", 1),
		MetricsTTFTSeconds:                   envFloat("FAKE_ENGINE_METRICS_TTFT_SECONDS", 0.05),
		MetricsTPOTSeconds:                   envFloat("FAKE_ENGINE_METRICS_TPOT_SECONDS", 0.02),
		MetricsE2ESeconds:                    envFloat("FAKE_ENGINE_METRICS_E2E_SECONDS", 1.5),
		MetricsQueueSeconds:                  envFloat("FAKE_ENGINE_METRICS_QUEUE_SECONDS", 0.01),
		MetricsPrefillSeconds:                envFloat("FAKE_ENGINE_METRICS_PREFILL_SECONDS", 0.3),
		MetricsDecodeSeconds:                 envFloat("FAKE_ENGINE_METRICS_DECODE_SECONDS", 1.2),
		MetricsPreemptionsPerScrape:          envInt("FAKE_ENGINE_METRICS_PREEMPTIONS_PER_SCRAPE", 0),
		MetricsRequestSuccessStopPerScrape:   envInt("FAKE_ENGINE_METRICS_REQUEST_SUCCESS_STOP_PER_SCRAPE", 1),
		MetricsRequestSuccessLengthPerScrape: envInt("FAKE_ENGINE_METRICS_REQUEST_SUCCESS_LENGTH_PER_SCRAPE", 0),
		MetricsRequestSuccessAbortPerScrape:  envInt("FAKE_ENGINE_METRICS_REQUEST_SUCCESS_ABORT_PER_SCRAPE", 0),
		MetricsRequestsRunning:               envInt("FAKE_ENGINE_METRICS_REQUESTS_RUNNING", 1),
		MetricsRequestsWaiting:               envInt("FAKE_ENGINE_METRICS_REQUESTS_WAITING", 0),
		MetricsKVCacheUsagePercent:           envFloat("FAKE_ENGINE_METRICS_KV_CACHE_USAGE_PERCENT", 0.25),
		StatusOverride:                       envInt("FAKE_ENGINE_STATUS_OVERRIDE", 0),
		PD:                                   pdOptions(args),
	})
	stopSide, err := mux.ServeSideChannel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-engine: KV side channel: %v\n", err)
		os.Exit(1)
	}
	defer stopSide()
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// prepareOffloadTiers mirrors what vLLM's OffloadingConnector does to each
// filesystem secondary tier at startup: create root_dir and leave a
// config.json in it. An empty or non-filesystem configuration is a no-op.
func prepareOffloadTiers(transferConfig string) error {
	if strings.TrimSpace(transferConfig) == "" {
		return nil
	}
	var config struct {
		Extra struct {
			SecondaryTiers []struct {
				Type    string `json:"type"`
				RootDir string `json:"root_dir"`
			} `json:"secondary_tiers"`
		} `json:"kv_connector_extra_config"`
	}
	if err := json.Unmarshal([]byte(transferConfig), &config); err != nil {
		return fmt.Errorf("invalid --kv-transfer-config: %w", err)
	}
	for _, tier := range config.Extra.SecondaryTiers {
		if tier.Type != "fs" || tier.RootDir == "" {
			continue
		}
		if err := os.MkdirAll(tier.RootDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tier.RootDir, "config.json"), mustJSON(tier), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func envDuration(key string) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, _ := time.ParseDuration(v)
		return d
	}
	return 0
}
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, _ := strconv.Atoi(v)
		if n > 0 {
			return n
		}
	}
	return fallback
}

// envFloat is used only for the deterministic metrics fixture values, where
// an explicit 0 (e.g. FAKE_ENGINE_METRICS_PREFILL_SECONDS=0) is a meaningful
// override rather than "unset" — unlike envInt above, whose callers only ever
// want a positive count.
func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func valueArg(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"=")
			}
		}
	}
	return ""
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return append(b, '\n') }

// pdOptions reads the PD role from argv and the process environment. A
// malformed kv_transfer_params kills the engine, as it kills vLLM's EngineCore.
func pdOptions(args []string) *handler.PDOptions {
	env := map[string]string{}
	for _, key := range []string{"VLLM_NIXL_SIDE_CHANNEL_HOST", "VLLM_NIXL_SIDE_CHANNEL_PORT"} {
		env[key] = os.Getenv(key)
	}
	pd := handler.PDFromLaunch(args, env)
	if pd != nil {
		pd.Crash = func(reason string) {
			fmt.Fprintln(os.Stderr, "(EngineCore) "+reason)
			os.Exit(1)
		}
	}
	return pd
}
