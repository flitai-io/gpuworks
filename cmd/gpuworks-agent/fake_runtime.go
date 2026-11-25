package main

import (
	"log/slog"
	"os"
	"strings"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	recordingruntime "gpuworks.ai/gpuworks/internal/agent/runtime/recording"
)

// applyFakeRuntime applies the process-test stand-ins the environment asks
// for: the recording runtime (GPUWORKS_FAKE_RUNTIME_RECORD, optionally running
// a fake engine with GPUWORKS_FAKE_RUNTIME_ENGINE) and an injected network
// inventory (GPUWORKS_FAKE_NETWORK). Production startup sets none of them.
func applyFakeRuntime(cfg *agentapp.Config, log *slog.Logger) error {
	if record := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_RUNTIME_RECORD")); record != "" {
		driver := recordingruntime.New(record)
		if engine := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_RUNTIME_ENGINE")); engine != "" {
			driver.WithEngine(engine)
		}
		// A process test that has to observe an image arriving needs the node to
		// start out without it; the recording runtime otherwise reports every
		// image as already present.
		for _, ref := range strings.Split(os.Getenv("GPUWORKS_FAKE_RUNTIME_ABSENT_IMAGES"), ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				driver.InjectMissingImage(ref)
			}
		}
		// A process test of a refusal by runtime names the runtime the node
		// reports; the recorder stands in for docker otherwise.
		if name := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_RUNTIME_NAME")); name != "" {
			driver.As(name)
		}
		cfg.Driver = driver
		cfg.ForceDriver = driver.Name()
		log.Warn("recording runtime enabled for process test", "path", record)
	}
	if path := strings.TrimSpace(os.Getenv(fakeNetworkEnv)); path != "" {
		if err := applyFakeNetwork(cfg, path); err != nil {
			return err
		}
		log.Warn("fake network inventory enabled for process test", "path", path)
	}
	return nil
}
