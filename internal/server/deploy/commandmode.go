package deploy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CommandMode controls which part of a container's command is owned by the
// platform. Empty values are normalized by source at the API boundary.
type CommandMode string

const (
	CommandEngine     CommandMode = "engine"
	CommandImageEntry CommandMode = "image-entry"
	CommandManual     CommandMode = "manual"
)

// ImageCommand is the deployment's command choice: its mode, the effective
// entrypoint and an explicit command argv, if any.
type ImageCommand struct {
	Mode       CommandMode
	Entrypoint []string
	Cmd        []string
	// KeepSystemEnv keeps the engine and compute-network environment under a
	// manual command. A rank of a multi-node group or a PD instance needs it:
	// that environment holds facts of one rank on one node (its address, its
	// interfaces, its side channel) that one script shared by every rank
	// cannot write down.
	KeepSystemEnv bool
}

// AssembleCommand applies the image command contract to an engine result. The
// engine owns the complete command, image-entry preserves the image entrypoint
// and appends the engine's parameter portion, and manual leaves the image
// command untouched. The engine is passed in because it is the only thing that
// knows how its own command splits into executable and parameters.
func AssembleCommand(engine Engine, built ResolvedLaunch, image ImageCommand) ResolvedLaunch {
	mode := image.Mode
	if mode == "" {
		mode = CommandEngine
	}
	out := built
	out.Entrypoint = append([]string(nil), built.Entrypoint...)
	out.Argv = append([]string(nil), built.Argv...)
	out.Env = cloneEnv(built.Env)
	out.EnvLayers.Template = cloneEnv(built.EnvLayers.Template)
	switch mode {
	case CommandImageEntry:
		out.Entrypoint = append([]string(nil), image.Entrypoint...)
		_, params := hooks(engine).SplitCommand(built)
		out.Argv = append(append([]string(nil), image.Cmd...), params...)
	case CommandManual:
		out.Entrypoint = append([]string(nil), image.Entrypoint...)
		out.Argv = append([]string(nil), image.Cmd...)
		if !image.KeepSystemEnv {
			out.Env = map[string]string{}
		}
	}
	return out
}

// WithEntrypoint replaces the executable portion of an assembled engine
// command while retaining its parameter vector. Some engines (notably SGLang
// and modern vLLM) encode both portions in ResolvedLaunch.Entrypoint, so assigning the
// field directly would silently discard the model, topology and API flags.
// Catalog preview and deployment must both use this helper.
func WithEntrypoint(engine Engine, built ResolvedLaunch, entrypoint []string) ResolvedLaunch {
	if len(entrypoint) == 0 {
		return built
	}
	_, params := hooks(engine).SplitCommand(built)
	built.Argv = params
	built.Entrypoint = append([]string(nil), entrypoint...)
	return built
}

func ParseCommandDisplay(display string) ([]string, error) {
	display = strings.TrimSpace(display)
	if display == "" {
		return nil, fmt.Errorf("command must not be empty")
	}
	if strings.HasPrefix(display, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(display), &argv); err != nil {
			return nil, fmt.Errorf("expected JSON string array: %w", err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("command array must not be empty")
		}
		return argv, nil
	}
	var out []string
	for _, part := range strings.Fields(display) {
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("command must not be empty")
	}
	return out, nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
