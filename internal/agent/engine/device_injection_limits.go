package engine

import "strings"

// mergeDeviceDockerArgs makes the agent's scheduled accelerator selection
// authoritative over image- or user-provided singleton runtime options. In
// particular, an image-level `--gpus all` must not remain alongside the
// scheduler-selected `--gpus '"device=0,1"'`. Repeatable options such as
// --device are intentionally kept. Named ulimits are defaults: a limit already
// resolved by the server (from the deployment, template or compute network)
// takes precedence over the same limit supplied by a device backend.
func mergeDeviceDockerArgs(existing, injected []string) []string {
	result := append([]string(nil), existing...)
	for _, option := range []string{"--gpus", "--runtime"} {
		if hasDockerOption(injected, option) {
			result = removeDockerOption(result, option)
		}
	}
	// The existing NVIDIA/Huawei injectors supply no ulimits. Keep their
	// append-only path, including arguments owned by the runtime itself.
	hasLimits := false
	for i := 0; i < len(injected); {
		name, width := dockerUlimitAt(injected, i)
		if name != "" {
			hasLimits = true
			break
		}
		i += width
	}
	if !hasLimits {
		return append(result, injected...)
	}
	limits := make(map[string]bool)
	for i := 0; i < len(existing); {
		name, width := dockerUlimitAt(existing, i)
		if name != "" {
			limits[name] = true
		}
		i += width
	}
	for i := 0; i < len(injected); {
		name, width := dockerUlimitAt(injected, i)
		if name == "" || !limits[name] {
			result = append(result, injected[i:i+width]...)
		}
		if name != "" {
			limits[name] = true
		}
		i += width
	}
	return result
}

// dockerUlimitAt reads just a named ulimit, preserving argument boundaries.
// Other or malformed options remain untouched for the runtime to validate.
func dockerUlimitAt(args []string, index int) (string, int) {
	token := strings.TrimSpace(args[index])
	value, width := "", 1
	switch {
	case token == "--ulimit" && index+1 < len(args):
		value, width = strings.TrimSpace(args[index+1]), 2
	case strings.HasPrefix(token, "--ulimit="):
		value = strings.TrimPrefix(token, "--ulimit=")
	case strings.HasPrefix(token, "--ulimit "):
		value = strings.TrimSpace(strings.TrimPrefix(token, "--ulimit "))
	default:
		return "", 1
	}
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" || strings.HasPrefix(name, "-") {
		return "", 1
	}
	return name, width
}
