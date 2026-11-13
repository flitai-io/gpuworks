package engines

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Feature identifies a requirement inferred from the engine's own command.
type Feature string

const (
	EngineSleep      Feature = "engine_sleep"
	MultiNode        Feature = "multi_node"
	PD               Feature = "prefill_decode_disaggregation"
	PipelineParallel Feature = "pipeline_parallelism"
	DataParallel     Feature = "data_parallelism"
)

// PipelineFlags and DataParallelFlags are exact CLI spellings, shared by
// topology validation and capability admission. vLLM's short PP flag is -pp.
func PipelineFlags(engine string) []string {
	switch engine {
	case VLLM:
		return []string{"--pipeline-parallel-size", "-pp"}
	case SGLang:
		return []string{"--pp-size", "--pp", "--pipeline-parallel-size"}
	}
	return nil
}
func DataParallelFlags(engine string) []string {
	switch engine {
	case VLLM:
		return []string{"--data-parallel-size", "-dp"}
	case SGLang:
		return []string{"--dp-size", "--dp", "--data-parallel-size"}
	}
	return nil
}

var pythonExecutable = regexp.MustCompile(`^python([0-9]+(\.[0-9]+)?)?$`)

func unwrapEnvCommand(argv []string) []string {
	if len(argv) == 0 || strings.ToLower(filepath.Base(argv[0])) != "env" {
		return argv
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return argv[i+1:]
		case arg == "-u" || arg == "--unset":
			if i+1 == len(argv) {
				return nil
			}
			i++
		case strings.HasPrefix(arg, "--unset="):
		case !strings.HasPrefix(arg, "-") && strings.Contains(arg, "="):
		default:
			return argv[i:]
		}
	}
	return nil
}

func serverArguments(engine string, argv []string) ([]string, bool) {
	if engine != VLLM && engine != SGLang || len(argv) == 0 {
		return nil, false
	}
	argv = unwrapEnvCommand(argv)
	if len(argv) == 0 {
		return nil, false
	}
	executable := strings.ToLower(filepath.Base(argv[0]))
	if executable == engine {
		if len(argv) > 1 && argv[1] == "serve" {
			return argv[2:], true
		}
		return nil, false
	}
	if !pythonExecutable.MatchString(executable) || len(argv) < 3 || argv[1] != "-m" {
		return nil, false
	}
	module := argv[2]
	if engine == VLLM && module == "vllm.entrypoints.cli.main" {
		if len(argv) > 3 && argv[3] == "serve" {
			return argv[4:], true
		}
		return nil, false
	}
	if engine == VLLM && module == "vllm.entrypoints.openai.api_server" || engine == SGLang && module == "sglang.launch_server" {
		return argv[3:], true
	}
	return nil, false
}

// CommandFeatures inspects actual server executable argv using its exact CLI
// vocabulary. Shells, unrecognized wrappers, other subcommands, and invalid
// feature values remain unknown. valueFlags comes from the caller's parameter
// dictionary so a flag-looking model name or other value cannot become an option here.
func CommandFeatures(engine string, argv []string, valueFlags ...string) ([]Feature, bool) {
	args, known := serverArguments(engine, argv)
	if !known {
		return nil, false
	}
	pp, dp := PipelineFlags(engine), DataParallelFlags(engine)
	groups := [][]string{pp, dp, {"--nnodes"}, {"--node-rank", "--node_rank"}}
	values := map[string]bool{"--model": true, "--model-path": true, "--served-model-name": true, "--tokenizer": true, "--disaggregation-mode": true, "--kv-transfer-config": true}
	for _, group := range append(groups, []string{"--tensor-parallel-size", "--tp-size", "--tp", "-tp"}) {
		for _, flag := range group {
			values[flag] = true
		}
	}
	for _, flag := range valueFlags {
		values[flag] = true
	}
	found := map[string]string{}
	sleep, valid := false, true
	for i := 0; i < len(args); i++ {
		flag, value, equal := strings.Cut(args[i], "=")
		if flag == "--" {
			break
		}
		if engine == VLLM && (flag == "--enable-sleep-mode" || flag == "--no-enable-sleep-mode") {
			sleep = flag == "--enable-sleep-mode"
			if equal {
				b, err := strconv.ParseBool(value)
				sleep = b
				valid = valid && err == nil
			} else if sleep && i+1 < len(args) {
				if b, err := strconv.ParseBool(args[i+1]); err == nil {
					sleep = b
					i++
				}
			}
			continue
		}
		if engine == VLLM && flag == "--pp" {
			valid = false
		}
		if !values[flag] {
			continue
		}
		if !equal {
			if i+1 >= len(args) {
				valid = false
				continue
			}
			i++
			value = args[i]
		}
		canonical := flag
		for _, group := range groups {
			for _, alias := range group {
				if flag == alias {
					canonical = group[0]
				}
			}
		}
		for _, group := range groups {
			if canonical == group[0] {
				minimum := 1
				if canonical == "--node-rank" {
					minimum = 0
				}
				number, err := strconv.Atoi(value)
				if err != nil || number < minimum {
					valid = false
				}
			}
		}
		found[canonical] = value
	}
	var out []Feature
	if sleep {
		out = append(out, EngineSleep)
	}
	for _, entry := range []struct {
		flags   []string
		minimum int
		feature Feature
	}{{pp, 1, PipelineParallel}, {dp, 1, DataParallel}, {[]string{"--nnodes"}, 1, MultiNode}, {[]string{"--node-rank"}, 0, MultiNode}} {
		raw, present := found[entry.flags[0]]
		if !present {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < entry.minimum {
			valid = false
			continue
		}
		if value > entry.minimum {
			out = append(out, entry.feature)
		}
	}
	if engine == SGLang {
		if mode, present := found["--disaggregation-mode"]; present {
			switch mode {
			case "prefill", "decode":
				out = append(out, PD)
			case "null":
			default:
				valid = false
			}
		}
	}
	if engine == VLLM {
		if raw, present := found["--kv-transfer-config"]; present {
			var config struct {
				Role string `json:"kv_role"`
			}
			if json.Unmarshal([]byte(raw), &config) != nil {
				valid = false
			} else {
				switch config.Role {
				case "kv_producer", "kv_consumer", "kv_both":
					out = append(out, PD)
				default:
					valid = false
				}
			}
		}
	}
	return out, valid
}
