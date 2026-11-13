package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func parseTransformers(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		Architectures []string `json:"architectures"`
		ModelType     string   `json:"model_type"`
		Class         string   `json:"_class_name"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "config.json"), &cfg)
	if err != nil {
		return RuleResult{}, err
	}
	if !ok {
		items, err := files.ReadDir(ctx, in.Path)
		if err != nil {
			return RuleResult{}, metadataError(in.Path, err)
		}
		if len(checkpointIndexes(in.Path, items)) > 0 || len(checkpointWeights(in.Path, items)) > 0 {
			return unrecognized(path.Join(in.Path, "config.json"), "checkpoint files are present, but the Transformers model configuration is absent"), nil
		}
		return RuleResult{}, nil
	}
	// A Diffusers component's config is not a Transformers model contract.
	if cfg.Class != "" && len(cfg.Architectures) == 0 && cfg.ModelType == "" {
		return unrecognized(path.Join(in.Path, "config.json"), "configuration describes a Diffusers component, not an independent Transformers model"), nil
	}
	if len(cfg.Architectures) == 0 && cfg.ModelType == "" {
		return unrecognized(path.Join(in.Path, "config.json"), "Transformers identity is absent: neither model_type nor architectures is declared"), nil
	}
	entry, issues, err := checkpoint(ctx, in.Path, "transformers", files)
	if err != nil {
		return RuleResult{}, err
	}
	if len(entry.Files) == 0 {
		return unrecognized(path.Join(in.Path, "config.json"), "Transformers configuration was read, but no supported checkpoint weights or index were found in this directory"), nil
	}
	return RuleResult{Entries: []Entry{entry}, Issues: issues}, nil
}

// Checkpoint indices are authoritative: an unrelated weight file cannot mask
// a missing shard. Unindexed weights must be directly in this directory.
func checkpointWithNames(ctx context.Context, dir, format string, files FileView, names []string) (Entry, []Issue, error) {
	e := Entry{Path: dir, Format: format}
	items, err := files.ReadDir(ctx, dir)
	if err != nil {
		return e, nil, err
	}
	for _, name := range names {
		indexPath := path.Join(dir, name+".index.json")
		shards, ok, err := readCheckpointIndex(ctx, files, indexPath)
		if err != nil {
			return e, nil, &fs.PathError{Op: "read checkpoint index", Path: indexPath, Err: err}
		}
		if !ok {
			continue
		}
		if len(shards) == 0 {
			return e, nil, &fs.PathError{Op: "read checkpoint index", Path: indexPath, Err: errors.New("empty weight_map")}
		}
		for _, target := range shards {
			p, err := childPath(dir, target)
			if err != nil {
				return e, nil, &fs.PathError{Op: "resolve checkpoint shard", Path: indexPath, Err: err}
			}
			if !slices.Contains(e.Files, p) {
				e.Files = append(e.Files, p)
			}
		}
		slices.Sort(e.Files)
		issues, err := checkRequired(ctx, &e, files)
		return e, issues, err
	}
	for _, f := range items {
		if f.IsDir {
			continue
		}
		for _, name := range names {
			ext := path.Ext(name)
			if f.Name == name || (strings.HasPrefix(f.Name, strings.TrimSuffix(name, ext)+"-") && strings.HasSuffix(f.Name, ext)) {
				e.Files = append(e.Files, path.Join(dir, f.Name))
				break
			}
		}
	}
	// Some manually copied checkpoints have lost their index; the standard
	// shard names still provide explicit evidence of missing parts.
	for _, name := range slices.Clone(e.Files) {
		match := checkpointShard.FindStringSubmatch(path.Base(name))
		if match == nil {
			continue
		}
		count, _ := strconv.Atoi(match[2])
		if count > 10000 {
			return e, nil, ErrReadLimit
		}
		for part := 1; part <= count; part++ {
			want := path.Join(dir, fmt.Sprintf("%s-%05d-of-%05d.%s", match[1], part, count, match[3]))
			if !slices.Contains(e.Files, want) {
				e.Files = append(e.Files, want)
			}
		}
		break
	}
	issues, err := checkRequired(ctx, &e, files)
	return e, issues, err
}

var checkpointShard = regexp.MustCompile(`^(model|pytorch_model|diffusion_pytorch_model)-\d{5}-of-(\d{5})\.(safetensors|bin)$`)

func checkRequired(ctx context.Context, e *Entry, files FileView) ([]Issue, error) {
	var issues []Issue
	for _, name := range e.Files {
		err := requiredFileError(ctx, files, name)
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return issues, err
		}
		e.Incomplete = true
		if errors.Is(err, fs.ErrNotExist) {
			e.Missing = append(e.Missing, name)
		} else {
			issues = append(issues, issue(name, "metadata_unreadable", err))
		}
	}
	return issues, nil
}

func parseMistral(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var params map[string]json.RawMessage
	ok, err := readJSON(ctx, files, path.Join(in.Path, "params.json"), &params)
	if err != nil {
		return RuleResult{}, err
	}
	if !ok {
		items, err := files.ReadDir(ctx, in.Path)
		if err != nil {
			return RuleResult{}, metadataError(in.Path, err)
		}
		if hasName(items, "tekken.json") {
			return unrecognized(path.Join(in.Path, "params.json"), "Mistral tokenizer is present, but model parameters are absent"), nil
		}
		return RuleResult{}, nil
	}
	if len(params) == 0 {
		return unrecognized(path.Join(in.Path, "params.json"), "Mistral model parameters are empty"), nil
	}
	if err := requiredFileError(ctx, files, path.Join(in.Path, "tekken.json")); err != nil {
		return unrecognized(path.Join(in.Path, "tekken.json"), "Mistral tokenizer: "+err.Error()), nil //nolint:nilerr // An unavailable tokenizer is returned as an unrecognized-model result.
	}
	items, err := files.ReadDir(ctx, in.Path)
	if err != nil {
		return RuleResult{}, err
	}
	e := Entry{Path: in.Path, Format: "mistral", Supersedes: []string{"transformers"}}
	for _, item := range items {
		if !item.IsDir && strings.HasPrefix(item.Name, "consolidated") && strings.HasSuffix(item.Name, ".safetensors") {
			e.Files = append(e.Files, path.Join(in.Path, item.Name))
		}
	}
	if len(e.Files) == 0 {
		return unrecognized(in.Path, "Mistral configuration was read, but no consolidated*.safetensors weights were found in this directory"), nil
	}
	issues, err := checkRequired(ctx, &e, files)
	return RuleResult{Entries: []Entry{e}, Issues: issues}, err
}

func parseAdapter(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		PEFTType string `json:"peft_type"`
		Base     string `json:"base_model_name_or_path"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "adapter_config.json"), &cfg)
	if err != nil || !ok {
		return RuleResult{}, err
	}
	if cfg.PEFTType == "" {
		return unrecognized(path.Join(in.Path, "adapter_config.json"), "adapter identity is absent: peft_type is not declared"), nil
	}
	return RuleResult{Components: []Reference{{Path: in.Path, Role: "adapter", Repository: cfg.Base, Source: "adapter_config.json"}}}, nil
}

func parseModelScope(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		Task  string                     `json:"task"`
		Model map[string]json.RawMessage `json:"model"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "configuration.json"), &cfg)
	if err != nil || !ok {
		return RuleResult{}, err
	}
	if cfg.Task == "" || len(cfg.Model) == 0 {
		return unrecognized(path.Join(in.Path, "configuration.json"), "ModelScope identity requires both a task and a non-empty model definition"), nil
	}
	e, issues, err := checkpoint(ctx, in.Path, "modelscope", files)
	if err != nil {
		return RuleResult{}, err
	}
	if len(e.Files) == 0 && exists(ctx, files, path.Join(in.Path, "pytorch_model.pt")) {
		e.Files = []string{path.Join(in.Path, "pytorch_model.pt")}
	}
	if len(e.Files) == 0 {
		return unrecognized(path.Join(in.Path, "configuration.json"), "ModelScope configuration was read, but no supported checkpoint weights or index were found in this directory"), nil
	}
	e.Supersedes = []string{"transformers"}
	return RuleResult{Entries: []Entry{e}, Issues: issues}, nil
}
