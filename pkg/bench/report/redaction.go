package report

import (
	"encoding/json"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// Historical JSONL may predate collection-time redaction. Copy both slices so
// every renderer sees safe errors without changing caller-owned detail records.
func sanitizedDetailPoints(points []Point) []Point {
	out := append([]Point(nil), points...)
	for i := range out {
		out[i].Details = append([]loadgen.DetailRecord(nil), points[i].Details...)
		for j := range out[i].Details {
			out[i].Details[j].ErrorText = loadgen.RedactErrorText(out[i].Details[j].ErrorText)
		}
	}
	return out
}

// Dataset documents can contain imported request bodies. Only the established
// configuration and a numeric profile belong in an ordinary report context.
func datasetContext(raw json.RawMessage) json.RawMessage {
	var config dataset.Config
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	if config.Profile == nil && len(config.Samples) > 0 {
		profile := dataset.ProfileSamples(config.Samples)
		config.Profile = &profile
	}
	config.Samples = nil
	out, _ := json.Marshal(config)
	return out
}

// Retain known snapshot facts without forwarding arbitrary future fields or
// credential-bearing command arguments to an exported artifact.
func sanitizedSnapshot(raw json.RawMessage) json.RawMessage {
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for _, key := range []string{"target_kind", "entry", "role", "deployment_id", "deployment_name", "instance_uuid", "served_name", "model", "model_source_id", "base_url", "load_origin", "engine", "priority_scheduling", "image", "command_mode", "entrypoint", "argv", "env", "docker_args", "gpu_indexes", "gpus", "num_gpus", "tensor_parallel", "node_id", "node_name"} {
		value, ok := input[key]
		if !ok {
			continue
		}
		switch key {
		case "base_url":
			if text, ok := value.(string); ok {
				value = redactURL(text)
			}
		case "argv", "entrypoint", "docker_args":
			if list, ok := value.([]any); ok {
				args := make([]string, 0, len(list))
				for _, item := range list {
					if text, ok := item.(string); ok {
						args = append(args, text)
					}
				}
				value = redactArguments(args)
			}
		case "env":
			if env, ok := value.(map[string]any); ok {
				for name, item := range env {
					if sensitiveEnv.MatchString(name) {
						env[name] = "[REDACTED]"
					} else if text, ok := item.(string); ok {
						// An ordinary variable name may carry an engine argument,
						// header or JSON credential assignment as its value.
						env[name] = redactArgument(text)
					}
				}
				value = env
			}
		}
		out[key] = value
	}
	encoded, _ := json.Marshal(out)
	return encoded
}

func redactArguments(args []string) []string {
	out := make([]string, len(args))
	redactNext := false
	for i, arg := range args {
		if redactNext {
			out[i] = "[REDACTED]"
			redactNext = false
			continue
		}
		out[i] = redactArgument(arg)
		if strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") && sensitiveEnv.MatchString(arg) {
			redactNext = true
		}
	}
	return out
}

func redactArgument(arg string) string {
	if safe, ok := redactJSONArgument(arg); ok {
		return safe
	}
	// A shell command or a header can bundle quoting and several assignments
	// into one argument. Conservatively omit that argument rather than claim
	// to parse arbitrary shell syntax and accidentally retain a credential.
	if strings.ContainsAny(arg, " \t\n") && sensitiveEnv.MatchString(arg) {
		return "[REDACTED argument containing credential references]"
	}
	if sensitiveEnv.MatchString(arg) && strings.ContainsAny(arg, "'\"`{}[]()\\;$") {
		// Shell substitutions need not contain literal whitespace. Do not try
		// to interpret shell syntax to locate the end of its credential.
		return "[REDACTED argument containing credential references]"
	}
	if scheme, _, ok := strings.Cut(arg, "://"); ok {
		// Recognize the scheme independently: malformed userinfo must reach
		// URL redaction before query '=' is mistaken for an argument assignment.
		if parsed, err := url.Parse(scheme + "://placeholder"); err == nil && parsed.Host != "" {
			return redactURL(arg)
		}
	}
	if key, value, ok := strings.Cut(arg, "="); ok {
		if sensitiveEnv.MatchString(key) {
			return key + "=[REDACTED]"
		}
		return key + "=" + redactArgument(value)
	}
	if key, _, ok := strings.Cut(arg, ":"); ok && sensitiveEnv.MatchString(key) {
		return key + ":[REDACTED]"
	}
	return redactURL(arg)
}

func redactJSONArgument(arg string) (string, bool) {
	if !json.Valid([]byte(arg)) {
		return "", false
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(arg))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return "", false
	}
	safe, changed := redactJSONValue(value)
	if !changed {
		return arg, true
	}
	raw, _ := json.Marshal(safe)
	return string(raw), true
}

func redactJSONValue(value any) (any, bool) {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if sensitiveEnv.MatchString(key) {
				value[key], changed = "[REDACTED]", true
				continue
			}
			safe, itemChanged := redactJSONValue(item)
			value[key], changed = safe, changed || itemChanged
		}
		return value, changed
	case []any:
		for i, item := range value {
			safe, itemChanged := redactJSONValue(item)
			value[i], changed = safe, changed || itemChanged
		}
		return value, changed
	case string:
		safe := redactArgument(value)
		return safe, safe != value
	default:
		return value, false
	}
}

func redactURL(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	return loadgen.RedactURL(raw)
}
