package deploy

import (
	"fmt"
	"strconv"
	"strings"
)

// StartupProbeMarginSeconds leaves the outer HTTP probe time to observe an
// engine whose own readiness deadline has not yet expired.
const StartupProbeMarginSeconds = 30

// StartupBudgetLimitSeconds is the bounded automatic startup policy. Explicit
// deployment values are validated against the same one-day API bound.
const StartupBudgetLimitSeconds = 24 * 60 * 60

const vllmReadyTimeoutEnv = "VLLM_ENGINE_READY_TIMEOUT_S"

// resolveStartupBudget runs after command and environment precedence. It only
// respects explicit vLLM engine-mode deadlines through template wrappers.
// Automatic injection requires its known executable; manual commands stay independent.
func resolveStartupBudget(input ResolveLaunchInput, launch *ResolvedLaunch) error {
	seconds, source := input.DefaultStartTimeoutSec, "cluster"
	if input.StartTimeoutSec > 0 {
		seconds, source = input.StartTimeoutSec, "deployment"
	}
	managedVLLM := managedVLLMStartup(input.Engine.Name(), *launch)
	engineSeconds := 0
	envRemoved := false
	for _, name := range input.ContainerOverrides.EnvUnset {
		if strings.TrimSpace(name) == vllmReadyTimeoutEnv {
			envRemoved = true
		}
	}
	if knownVLLMStartup(input.Engine.Name(), *launch) {
		var err error
		engineSeconds, err = explicitEngineReadyTimeout(launch.Env)
		if err != nil {
			return err
		}
		withMargin := int64(engineSeconds) + StartupProbeMarginSeconds
		if engineSeconds > 0 && input.StartTimeoutSec == 0 && withMargin > int64(seconds) {
			seconds, source = int(min(withMargin, StartupBudgetLimitSeconds)), "engine"
		}
	}
	if seconds <= 0 {
		return nil // An allocation-only caller has not supplied a policy.
	}
	if input.StartTimeoutSec == 0 && seconds > StartupBudgetLimitSeconds {
		seconds = StartupBudgetLimitSeconds
	}
	launch.StartTimeoutSeconds, launch.StartTimeoutSource = seconds, source
	if managedVLLM && engineSeconds == 0 && !envRemoved {
		engineSeconds = max(seconds-StartupProbeMarginSeconds, 1)
		if launch.Env == nil {
			launch.Env = map[string]string{}
		}
		launch.Env[vllmReadyTimeoutEnv] = strconv.Itoa(engineSeconds)
		if launch.EnvLayers.System == nil {
			launch.EnvLayers.System = map[string]string{}
		}
		launch.EnvLayers.System[vllmReadyTimeoutEnv] = strconv.Itoa(engineSeconds)
	}
	launch.EngineStartTimeoutSeconds = engineSeconds
	launch.StartTimeoutConflict = startupBudgetConflict(seconds, engineSeconds)
	return nil
}

// ExplainRecordedStartupBudget interprets only what an older desired state
// recorded. It never changes its budget or environment, and never guesses the
// source of the budget or an unstated engine default.
func ExplainRecordedStartupBudget(engine string, launch ResolvedLaunch, seconds int) (engineSeconds int, conflict bool) {
	if seconds <= 0 || !knownVLLMStartup(engine, launch) {
		return 0, false
	}
	engineSeconds, err := explicitEngineReadyTimeout(launch.Env)
	if err != nil {
		return 0, false
	}
	return engineSeconds, startupBudgetConflict(seconds, engineSeconds)
}

// An explicit deadline in an engine-mode vLLM template retains its meaning
// through a toolkit wrapper. Only the known executable gets automatic injection.
func knownVLLMStartup(engine string, launch ResolvedLaunch) bool {
	return engine == EngineVLLM && launch.CommandMode == string(CommandEngine)
}

func managedVLLMStartup(engine string, launch ResolvedLaunch) bool {
	return engine == EngineVLLM && launch.CommandMode == string(CommandEngine) &&
		len(launch.Entrypoint) >= 2 && launch.Entrypoint[0] == EngineVLLM && launch.Entrypoint[1] == "serve"
}

func startupBudgetConflict(seconds, engineSeconds int) bool {
	return engineSeconds > 0 && int64(seconds) < int64(engineSeconds)+StartupProbeMarginSeconds
}

func explicitEngineReadyTimeout(env map[string]string) (int, error) {
	raw, stated := env[vllmReadyTimeoutEnv]
	if !stated {
		return 0, nil
	}
	if raw == "" || strings.Trim(raw, "0123456789") != "" {
		return 0, fmt.Errorf("%s must be a positive integer in seconds", vllmReadyTimeoutEnv)
	}
	value, err := strconv.ParseUint(raw, 10, 31)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("%s must be a positive integer of at most 2147483647 seconds", vllmReadyTimeoutEnv)
	}
	return int(value), nil
}
