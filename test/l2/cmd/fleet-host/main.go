// Command fleet-host runs a whole fleet of agents in one process against a
// control plane that is running in another one.
//
// It exists so the fleet load case can be measured with the server separated
// from the agents. In the single-process shape, the server's RSS, CPU and
// scheduler are the fleet's too, so no measurement can say what the control
// plane alone costs. Here the server is its own process and everything this
// binary does is fleet-side; the two are read from /proc separately.
//
// The agents are the real agent runtime with the harness's fake GPU prober and
// fake container runtime, the same ones the in-process shape uses, so the two
// shapes differ in process topology and in nothing else.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/integrationtest/fleet"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// node is one entry of the roster the test process hands over: the identity it
// created through the control plane's own API, which is the only way a separate
// process can enrol a node.
type node struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
	Churn bool   `json:"churn"`
}

func main() {
	roster := flag.String("roster", "", "JSON file listing the nodes to host: name, uuid, token, churn")
	nextRoster := flag.String("next-roster", "", "optional second roster file; start those agents when the file appears")
	server := flag.String("server", "", "control-plane base URL")
	stateRoot := flag.String("state-root", "", "directory for per-node agent state")
	modelDir := flag.String("model-dir", "", "directory holding the preloaded model every node registers")
	ready := flag.String("ready", "", "file to create once every agent has been started")
	pullLines := flag.Int("pull-lines", 2000, "lines of pull output a redeploying node replays")
	logPath := flag.String("log", "", "agent log file; empty logs agent internals to stderr")
	logRate := flag.Float64("log-rate", 0, "synthetic instance log lines per second per running instance (0 disables log load)")
	gpus := flag.Int("gpus", 1, "GPUs each node reports")
	ttft := flag.Duration("engine-ttft", 0, "fake engine delay before the first streamed delta")
	tpot := flag.Duration("engine-tpot", 0, "fake engine delay after each streamed delta")
	chunks := flag.Int("engine-chunks", 1, "content deltas per streamed chat completion")
	flag.Parse()

	testenv.SetSharedEngineOptions(fakehandler.Options{TTFTDelay: *ttft, TPOTDelay: *tpot, StreamChunks: *chunks, CompletionTokens: *chunks})
	if err := run(*roster, *nextRoster, *server, *stateRoot, *modelDir, *ready, *logPath, *pullLines, *logRate, *gpus); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-host:", err)
		os.Exit(1)
	}
}

func run(rosterPath, nextRosterPath, server, stateRoot, modelDir, readyPath, logPath string, pullLines int, logRate float64, gpus int) error {
	if rosterPath == "" || server == "" || stateRoot == "" || modelDir == "" {
		return fmt.Errorf("roster, server, state-root and model-dir are all required")
	}
	amdPercent, err := fleetAMDPercent(os.Getenv("GPUWORKS_FLEET_AMD_PERCENT"))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(rosterPath)
	if err != nil {
		return fmt.Errorf("read roster: %w", err)
	}
	var nodes []node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return fmt.Errorf("parse roster: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("roster is empty")
	}
	if err := writePreloadedModel(modelDir); err != nil {
		return fmt.Errorf("write the preloaded model: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open agent log: %w", err)
		}
		defer file.Close()
		log = slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	transcript := fleet.PullTranscript(pullLines)
	startNodes := func(nodes []node) error {
		for _, n := range nodes {
			amd := fleetAMDNode(n.Name, amdPercent)
			// A synthetic authorized runtime, not a daemon.
			driverName := "docker"
			driver := testenv.NewFakeDriver(driverName)
			// Every node is a separate machine as far as the scheduler is
			// concerned, so they all get the same engine port. On one host that is
			// one bind; the fake attaches to a shared listener instead.
			driver.ShareEnginePorts()
			if n.Churn {
				driver.OnPullImage(func(_ string, onLine func(string)) (error, bool) {
					for _, line := range transcript {
						onLine(line)
					}
					return nil, true
				})
			}
			if logRate > 0 {
				// The same synthetic stream the in-process shape injects, so the
				// two shapes differ in process topology and in nothing else.
				rate := logRate
				driver.OnLogs(func(id string, follow bool, tail int) (io.ReadCloser, error, bool) {
					return fleet.NewRateLogStream(rate, id, follow, tail), nil, true
				})
			}
			vendor, name, arch := "nvidia", "A100", "sm_80"
			if amd {
				vendor, name, arch = "amd", "AMD Instinct MI300X", "gfx942"
			}
			devices := make([]proto.GPUInfo, 0, max(gpus, 1))
			for i := 0; i < max(gpus, 1); i++ {
				devices = append(devices, proto.GPUInfo{
					Index: i, UUID: fmt.Sprintf("GPU-%s-%d", n.Name, i), Name: name, ArchID: arch,
					MemoryTotalBytes: int64(81920) << 20, Vendor: vendor, Healthy: true,
				})
			}
			prober := testenv.NewFakeProber(devices)
			if os.Getenv("GPUWORKS_FLEET_GPU_METRICS") == "1" {
				metrics := make([]gpu.Metric, 0, len(devices))
				for _, device := range devices {
					metrics = append(metrics, gpu.Metric{
						Index: device.Index, UUID: device.UUID, Vendor: device.Vendor,
						Values: map[string]float64{
							"gpuworks_gpu_utilization":       fleet.GPUReadTestUtilization,
							"gpuworks_gpu_memory_used_bytes": fleet.GPUReadTestMemoryBytes,
						},
					})
				}
				prober.SetMetrics(metrics)
			}
			// Synthetic AMD nodes exercise admission and control-plane load, while
			// the dedicated injection fixtures validate physical device mapping.
			prober.DisableRuntimeInjection = amd
			state := filepath.Join(stateRoot, n.Name)
			if err := os.MkdirAll(state, 0o755); err != nil {
				return fmt.Errorf("create agent state for %s: %w", n.Name, err)
			}
			agent := agentapp.New(agentapp.Config{
				ServerURL: server, Token: n.Token, StateDir: state,
				Driver: driver, ForceDriver: driverName, Prober: prober, DataLane: true,
				NetworkInterfaceResolver: func(address string) (string, error) {
					if address == "" {
						return "", fmt.Errorf("fake network address is empty")
					}
					return "fake0", nil
				},
			}, log)
			go func() { _ = agent.Run(ctx) }()
		}

		return nil
	}
	if err := startNodes(nodes); err != nil {
		return err
	}

	if readyPath != "" {
		if err := os.WriteFile(readyPath, []byte(fmt.Sprintf("%d\n", len(nodes))), 0o644); err != nil {
			return fmt.Errorf("write the ready file: %w", err)
		}
	}
	if nextRosterPath != "" {
		raw, err := waitForRoster(ctx, nextRosterPath)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var next []node
		if err := json.Unmarshal(raw, &next); err != nil {
			return fmt.Errorf("parse next roster: %w", err)
		}
		if len(next) == 0 {
			return fmt.Errorf("next roster is empty")
		}
		if err := startNodes(next); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

// writePreloadedModel lays down the weights the fleet deploys. They are already
// on the node, which is a shape the product supports directly: the control
// plane reads the directory over node RPC and registers it, so the case never
// downloads a gigabyte per node just to have something to serve.
//
// Every node shares the one directory because every node is the same machine
// here; on a real fleet each would have its own copy.
func writePreloadedModel(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	config := `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":256,` +
		`"num_hidden_layers":1,"num_attention_heads":8,"num_key_value_heads":8,"head_dim":32,` +
		`"max_position_embeddings":2048,"vocab_size":1024,"torch_dtype":"bfloat16"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o644); err != nil {
		return err
	}
	tokenizer := `{"version":"1.0","model":{"type":"BPE","vocab":{},"merges":[]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(tokenizer), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer_config.json"), []byte(`{"model_type":"qwen3"}`+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "model.safetensors"), make([]byte, 1<<20), 0o644)
}

// waitForRoster waits for the test to enroll the next batch. The test publishes
// the file atomically; all batches share one process's fake engine listeners.
func waitForRoster(ctx context.Context, path string) ([]byte, error) {
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read next roster: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// The default remains the historical NVIDIA-only fleet. The stable identity
// hash of the fixture name keeps paired runs and reconnects on the same mix.
func fleetAMDPercent(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 100 {
		return 0, fmt.Errorf("GPUWORKS_FLEET_AMD_PERCENT must be an integer from 0 to 100")
	}
	return value, nil
}
func fleetAMDNode(name string, percent int) bool {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(name))
	return int(hash.Sum32()%100) < percent
}
