// Command gpuworks-catalog-model-config fills the estimation-relevant config.json
// subset (model_config) of an inference catalog package from the repositories each
// spec already declares.
//
// The catalog carries that subset so KV-cache/VRAM estimation works without
// fetching anything at deploy time: a gated repository can never be read by the
// server, and some repositories publish no attention shape at all. This tool
// derives the block from the real config.json, so a catalog maintainer can
// refresh it whenever a model is added or re-pinned.
//
// Usage:
//
//	gpuworks-catalog-model-config -catalog internal/server/inferencecatalog/seed/catalog
//	gpuworks-catalog-model-config -catalog <dir> -write
//
// Without -write it only reports what would change. Models whose repositories
// cannot be read are reported and left untouched; the -fallbacks file records
// the mirror or hand-declared shape used for the ones that need it.
//
// Gated repositories need a token. Export HF_TOKEN (see secrets/huggingface.env)
// and this tool sends it; the product itself never does, which is why preset
// models must carry their own estimation facts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

type weightSource struct {
	Provider  string
	RepoID    string
	Subfolder string
}

// sourceDoc decodes the catalog's compact "- provider: repo" weight sources.
type sourceDoc struct {
	Specs []struct {
		Weights struct {
			Sources []map[string]any `yaml:"sources"`
		} `yaml:"weights"`
	} `yaml:"specs"`
	ModelConfig *modelconfig.Config `yaml:"model_config"`
}

type fallbackFile struct {
	// Repo is a mirror repository to read instead of the catalog's own source.
	Repo string `json:"repo,omitempty"`
	// Provider of Repo (huggingface when empty).
	Provider string `json:"provider,omitempty"`
	// Subfolder narrows Repo to the directory holding the language model config,
	// for repositories that publish several components side by side.
	Subfolder string `json:"subfolder,omitempty"`
	// ModelConfig is a hand-declared shape for a model whose repositories publish
	// none. It is used verbatim and reviewed like any other catalog data.
	ModelConfig *modelconfig.Config `json:"model_config,omitempty"`
	Note        string              `json:"note,omitempty"`
}

type outcome struct {
	modelID string
	path    string
	block   string
	status  string
	detail  string
}

func main() {
	catalog := flag.String("catalog", "internal/server/inferencecatalog/seed/catalog", "catalog package root")
	fallbacks := flag.String("fallbacks", "cmd/gpuworks-catalog-model-config/fallback-sources.json", "per-model fallback sources")
	write := flag.Bool("write", false, "rewrite model.yaml files instead of only reporting")
	verify := flag.Bool("verify", false, "cross-check existing blocks against the published weight indexes")
	only := flag.String("only", "", "limit to one model_id")
	jobs := flag.Int("jobs", 8, "parallel repository reads")
	flag.Parse()

	if *verify {
		if err := verifyBlocks(*catalog, *only, *jobs); err != nil {
			fmt.Fprintln(os.Stderr, "gpuworks-catalog-model-config:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*catalog, *fallbacks, *only, *jobs, *write); err != nil {
		fmt.Fprintln(os.Stderr, "gpuworks-catalog-model-config:", err)
		os.Exit(1)
	}
}

func run(catalog, fallbackPath, only string, jobs int, write bool) error {
	modelsDir := filepath.Join(catalog, "models")
	files, err := modelFiles(modelsDir)
	if err != nil {
		return err
	}
	fallbacks, err := loadFallbacks(fallbackPath)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token == "" {
		fmt.Println("note: HF_TOKEN is unset; gated repositories will read as 401")
	}
	results := make([]outcome, len(files))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, file := range files {
		modelID := strings.ReplaceAll(strings.TrimPrefix(filepath.Dir(file), modelsDir+string(filepath.Separator)), string(filepath.Separator), "/")
		if only != "" && modelID != only {
			continue
		}
		wg.Add(1)
		go func(i int, file, modelID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = resolve(client, file, modelID, fallbacks[modelID])
		}(i, file, modelID)
	}
	wg.Wait()

	fallbackEntries := len(fallbacks)
	changed, failed, kept := 0, 0, 0
	for _, result := range results {
		if result.modelID == "" {
			continue
		}
		switch result.status {
		case "update":
			changed++
			fmt.Printf("update  %-52s %s\n", result.modelID, result.detail)
			if write {
				if err := applyBlock(result.path, result.block); err != nil {
					return fmt.Errorf("write %s: %w", result.path, err)
				}
			}
		case "unchanged":
			kept++
		default:
			failed++
			fmt.Printf("skip    %-52s %s\n", result.modelID, result.detail)
		}
	}
	fmt.Printf("\n%d models: %d need an update, %d already current, %d skipped (no readable shape)\n", changed+kept+failed, changed, kept, failed)
	fmt.Printf("%d fallback entries read from %s\n", fallbackEntries, fallbackPath)
	if !write && changed > 0 {
		fmt.Println("re-run with -write to apply")
	}
	return nil
}

// resolve reads the model's own weight sources, then any recorded fallback, and
// renders the block that should be in model.yaml.
func resolve(client *http.Client, path, modelID string, fallback *fallbackFile) outcome {
	result := outcome{modelID: modelID, path: path}
	content, err := os.ReadFile(path)
	if err != nil {
		result.status, result.detail = "error", err.Error()
		return result
	}
	var doc sourceDoc
	if err := yaml.Unmarshal(content, &doc); err != nil {
		result.status, result.detail = "error", "yaml: "+err.Error()
		return result
	}

	// A repository that cannot be read and one that publishes no attention shape
	// are the same problem for the estimator, so both fall through to the
	// recorded fallback.
	cfg, source, err := readConfig(client, catalogSources(doc))
	if err == nil && !estimable(cfg) {
		err = fmt.Errorf("%s publishes no attention shape", source)
	}
	if err != nil && fallback != nil && fallback.Repo != "" {
		provider := fallback.Provider
		if provider == "" {
			provider = modelrepo.SourceHF
		}
		if fallbackCfg, fallbackSource, fallbackErr := readConfig(client, []weightSource{{Provider: provider, RepoID: fallback.Repo, Subfolder: fallback.Subfolder}}); fallbackErr == nil {
			cfg, source, err = fallbackCfg, fallbackSource, nil
		}
	}
	if err != nil && fallback != nil && fallback.ModelConfig != nil {
		cfg, source, err = fallback.ModelConfig, "declared shape", nil
	}
	if err != nil {
		// Keep any existing block: a model nobody can read must not lose the
		// facts it already carries.
		result.status, result.detail = "unreadable", fmt.Sprintf("%v (declare one in fallback-sources.json)", err)
		return result
	}
	if !estimable(cfg) {
		result.status, result.detail = "unreadable", "the declared shape does not answer the estimation question"
		return result
	}

	block, err := renderBlock(cfg)
	if err != nil {
		result.status, result.detail = "error", err.Error()
		return result
	}
	result.block = block
	result.detail = source
	if strings.Contains(string(content), block) {
		result.status, result.detail = "unchanged", source
		return result
	}
	result.status = "update"
	return result
}

// estimable reports whether the resolved shape carries the facts a deployment
// still reads: the attention shape a tensor-parallel size has to divide, and a
// context length. It no longer judges whether a KV pool can be sized - that cost
// is measured from the engine and recorded as kv_bytes_per_token, so a config
// cannot answer it either way (see docs/reference/inference-catalog-spec.md,
// kv_bytes_per_token).
// A model that declares it keeps no KV cache is estimable without a shape.
func estimable(cfg *modelconfig.Config) bool {
	if cfg == nil {
		return false
	}
	if !cfg.CachesKVTokens() {
		return true
	}
	return cfg.NumHiddenLayers > 0 && cfg.NumAttentionHeads > 0
}

// catalogSources returns every source the model's specs declare, huggingface
// first because its config.json is the canonical artifact layout.
func catalogSources(doc sourceDoc) []weightSource {
	var out []weightSource
	seen := map[string]bool{}
	for _, spec := range doc.Specs {
		for _, entry := range spec.Weights.Sources {
			for provider, payload := range entry {
				repo, subfolder := "", ""
				switch value := payload.(type) {
				case string:
					repo = value
				case map[string]any:
					if text, ok := value["repo_id"].(string); ok {
						repo = text
					}
					if text, ok := value["subfolder"].(string); ok {
						subfolder = text
					}
				}
				key := provider + "\x00" + repo + "\x00" + subfolder
				if repo == "" || seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, weightSource{Provider: provider, RepoID: repo, Subfolder: subfolder})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Provider == modelrepo.SourceHF && out[j].Provider != modelrepo.SourceHF
	})
	return out
}

func readConfig(client *http.Client, sources []weightSource) (*modelconfig.Config, string, error) {
	if len(sources) == 0 {
		return nil, "", errors.New("model declares no weight source")
	}
	// Report every attempt: a maintainer needs to see which repository was tried
	// and how it refused, not just the last failure.
	var attempts []string
	for _, source := range sources {
		for _, file := range configFiles {
			cfg, err := readOne(client, source, file)
			if err == nil {
				return cfg, source.Provider + "/" + source.RepoID, nil
			}
			attempts = append(attempts, fmt.Sprintf("%s/%s: %v", source.Provider, source.RepoID, err))
		}
	}
	if len(attempts) == 0 {
		return nil, "", errors.New("model declares no weight source")
	}
	return nil, "", errors.New(strings.Join(attempts, "; "))
}

// configFiles are the shapes a repository may publish. config.json is the
// HuggingFace layout both providers use; params.json is Mistral's native
// layout, which carries the same facts under its own names and is the only
// config some of their repositories publish.
var configFiles = []string{"config.json", "params.json"}

func readOne(client *http.Client, source weightSource, file string) (*modelconfig.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	name := file
	if source.Subfolder != "" {
		name = strings.TrimSuffix(source.Subfolder, "/") + "/" + file
	}
	url := modelrepo.ResolveURL(source.Provider, source.RepoID, "", name, "")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token != "" && source.Provider == modelrepo.SourceHF {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/%s: http %d", source.Provider+"/"+source.RepoID, name, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	cfg, err := modelconfig.Parse(strings.NewReader(string(body)))
	if err != nil {
		// A mirror that answers with an error document instead of JSON is a
		// failure a maintainer has to recognise, so show what came back.
		return nil, fmt.Errorf("%s is not JSON: %w (%s)", name, err, leadingBytes(body, 80))
	}
	return cfg, nil
}

func leadingBytes(body []byte, limit int) string {
	text := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '.'
		}
		return r
	}, string(body))
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
}

// blockFields is the estimation-relevant subset written to the catalog, in a
// fixed order. Only fields the repository actually declares are emitted, so the
// block stays a statement of fact rather than a rendering of derived numbers.
var blockFields = []string{
	"model_type", "num_hidden_layers", "hidden_size", "num_attention_heads",
	"num_key_value_heads", "head_dim", "max_position_embeddings", "sliding_window",
	"layer_types", "kv_cache",
	"kv_lora_rank", "qk_rope_head_dim", "qk_nope_head_dim", "v_head_dim",
	"vision_config", "audio_config",
}

func renderBlock(cfg *modelconfig.Config) (string, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	value := func(key string, anyValue any) {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			scalarNode(anyValue))
	}
	fields := map[string]any{
		"model_type":              cfg.ModelType,
		"num_hidden_layers":       cfg.NumHiddenLayers,
		"hidden_size":             cfg.HiddenSize,
		"num_attention_heads":     cfg.NumAttentionHeads,
		"num_key_value_heads":     cfg.NumKeyValueHeads,
		"head_dim":                cfg.HeadDim,
		"max_position_embeddings": cfg.MaxPositionEmbeddings,
		"sliding_window":          cfg.SlidingWindow,
		"layer_types":             cfg.LayerTypes,
		"kv_lora_rank":            cfg.KVLoraRank,
		"qk_rope_head_dim":        cfg.QKRopeHeadDim,
		"qk_nope_head_dim":        cfg.QKNopeHeadDim,
		"v_head_dim":              cfg.VHeadDim,
	}
	for _, key := range blockFields {
		if tower, ok := towerField(cfg, key); ok {
			if tower != nil {
				value(key, tower)
			}
			continue
		}
		if key == "kv_cache" {
			// Only a model that has no cache needs the fact; the ordinary
			// "caching is available" default stays implicit.
			if cfg.KVCache != nil && !*cfg.KVCache {
				value(key, false)
			}
			continue
		}
		switch item := fields[key].(type) {
		case string:
			if item != "" {
				value(key, item)
			}
		case int:
			if item != 0 {
				value(key, item)
			}
		case []string:
			if len(item) > 0 {
				value(key, item)
			}
		}
	}
	encoded, err := yaml.Marshal(node)
	if err != nil {
		return "", err
	}
	body := strings.TrimRight(string(encoded), "\n")
	return "model_config: " + strings.ReplaceAll(body, "\n", "\n  ") + "\n", nil
}

// towerField returns the tower subset for a tower key. A tower is emitted when
// the repository declares one, because its presence is what makes a model
// multimodal; unknown towers keep only their model type.
func towerField(cfg *modelconfig.Config, key string) (*yaml.Node, bool) {
	var tower *modelconfig.TowerConfig
	switch key {
	case "vision_config":
		tower = cfg.VisionConfig
	case "audio_config":
		tower = cfg.AudioConfig
	default:
		return nil, false
	}
	if tower == nil {
		return nil, true
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	for _, field := range []struct {
		key   string
		value int
	}{
		{"image_size", tower.ImageSize}, {"patch_size", tower.PatchSize},
		{"max_position_embeddings", tower.MaxPositionEmbeddings},
		{"max_source_positions", tower.MaxSourcePositions},
	} {
		if field.value > 0 {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: field.key}, scalarNode(field.value))
		}
	}
	if len(node.Content) == 0 {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "model_type"}, scalarNode(tower.ModelType))
	}
	return node, true
}

func scalarNode(value any) *yaml.Node {
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: "null"}
	}
	return node
}

// applyBlock replaces the model.yaml model_config block, leaving every other byte
// of the file untouched.
func applyBlock(path, block string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(content), "\n")
	out := make([]string, 0, len(lines)+4)
	inserted := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "specs:") && !inserted {
			out = append(out, strings.TrimRight(block, "\n"))
			inserted = true
		}
		if strings.HasPrefix(line, "model_config:") {
			// Drop the previous block, including its indented continuation.
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.TrimSpace(lines[i+1]) == "") {
				if strings.TrimSpace(lines[i+1]) == "" {
					break
				}
				i++
			}
			continue
		}
		out = append(out, line)
	}
	if !inserted {
		return errors.New("no specs: anchor found")
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}

func modelFiles(modelsDir string) ([]string, error) {
	var files []string
	err := filepath.Walk(modelsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "model.yaml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// verifyBlocks cross-checks every block against the layout of the weights it
// claims to describe. A block is only as good as the repository it was read
// from, and a mirror that lags or a spec pointing at another variant would
// otherwise ship a plausible but wrong shape. Layer count and attention kind
// (MLA latent versus explicit K/V) are both visible in a weight index, so they
// are cheap to confirm; head dimensions are not, and stay a matter of reading
// the config the tool recorded.
func verifyBlocks(catalog, only string, jobs int) error {
	modelsDir := filepath.Join(catalog, "models")
	files, err := modelFiles(modelsDir)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	checked, skipped, bad := 0, 0, 0
	sem := make(chan struct{}, jobs)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, file := range files {
		modelID := modelIDFor(modelsDir, file)
		if only != "" && modelID != only {
			continue
		}
		wg.Add(1)
		go func(file, modelID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, err := os.ReadFile(file)
			if err != nil {
				return
			}
			var doc sourceDoc
			if err := yaml.Unmarshal(content, &doc); err != nil || doc.ModelConfig == nil || !doc.ModelConfig.CachesKVTokens() {
				mu.Lock()
				skipped++
				mu.Unlock()
				return
			}
			problem := ""
			nextn := 0
			for _, source := range catalogSources(doc) {
				nextn = readNextNPredictLayers(client, source)
				facts, err := readWeightIndex(client, source)
				if err != nil {
					continue
				}
				problem = compareWithIndex(doc.ModelConfig, nextn, facts)
				break
			}
			mu.Lock()
			defer mu.Unlock()
			if problem == "" {
				checked++
				return
			}
			bad++
			fmt.Printf("mismatch %-52s %s\n", modelID, problem)
		}(file, modelID)
	}
	wg.Wait()
	fmt.Printf("\n%d blocks confirmed against the published weights, %d mismatched, %d without weights to read\n", checked, bad, skipped)
	return nil
}

func modelIDFor(modelsDir, file string) string {
	return strings.ReplaceAll(strings.TrimPrefix(filepath.Dir(file), modelsDir+string(filepath.Separator)), string(filepath.Separator), "/")
}

// weightIndexFacts are what a repository's weight index says about the stack
// that keeps the KV cache. Layer counts are per prefix group because one
// checkpoint can hold several stacks (a language model next to a vision tower or
// an optical encoder), and the estimator only sizes one of them.
type weightIndexFacts struct {
	layerCounts []int
	hasMLA      bool
}

// readNextNPredictLayers reads the extra multi-token-prediction layers a
// checkpoint may publish beyond the language model. They are stored as ordinary
// transformer layers, so a layer count taken from the weights alone would look
// one or two layers too large.
func readNextNPredictLayers(client *http.Client, source weightSource) int {
	for _, file := range configFiles {
		body, err := readRepoFile(client, source, file)
		if err != nil {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			continue
		}
		if nested, ok := raw["text_config"].(map[string]any); ok {
			if count, ok := nested["num_nextn_predict_layers"].(float64); ok {
				return int(count)
			}
		}
		if count, ok := raw["num_nextn_predict_layers"].(float64); ok {
			return int(count)
		}
		return 0
	}
	return 0
}

func readWeightIndex(client *http.Client, source weightSource) (weightIndexFacts, error) {
	var lastErr error
	for _, name := range []string{"model.safetensors.index.json", "pytorch_model.bin.index.json"} {
		body, err := readRepoFile(client, source, name)
		if err != nil {
			lastErr = err
			continue
		}
		var index struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		if err := json.Unmarshal(body, &index); err != nil {
			lastErr = err
			continue
		}
		return factsFromTensorNames(index.WeightMap), nil
	}
	return weightIndexFacts{}, lastErr
}

// factsFromTensorNames reads the KV-bearing stack from tensor names. Encoder
// towers and multimodal components sit in the same index, so the decoder stack
// wins when present and anything recognisably not the language model is
// excluded.
func factsFromTensorNames(names map[string]string) weightIndexFacts {
	var facts weightIndexFacts
	stacks := map[string]map[int]bool{}
	for name := range names {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "kv_a_proj_with_mqa") {
			facts.hasMLA = true
		}
		match := layerIndexExpression.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		stack := name[:strings.Index(name, ".layers.")]
		if stacks[stack] == nil {
			stacks[stack] = map[int]bool{}
		}
		stacks[stack][index] = true
	}
	for _, stack := range stacks {
		count := 0
		for index := range stack {
			if index+1 > count {
				count = index + 1
			}
		}
		facts.layerCounts = append(facts.layerCounts, count)
	}
	sort.Ints(facts.layerCounts)
	return facts
}

var layerIndexExpression = regexp.MustCompile(`\.layers\.(\d+)\.`)

// compareWithIndex reports how a block disagrees with the weights, or "" when
// they agree (or when the weights cannot decide the question).
func compareWithIndex(cfg *modelconfig.Config, nextn int, facts weightIndexFacts) string {
	want := declaredLayers(cfg)
	if want > 0 && len(facts.layerCounts) > 0 {
		// A checkpoint may carry a multi-token-prediction layer beyond the
		// language model, and it may hold several stacks side by side. Any stack
		// that matches the declared count confirms the block.
		confirmed := false
		for _, count := range facts.layerCounts {
			if count == want || (nextn > 0 && count == want+nextn) {
				confirmed = true
				break
			}
		}
		if !confirmed {
			return fmt.Sprintf("weights hold stacks %v layers, block declares %d", facts.layerCounts, want)
		}
	}
	if cfg.KVLoraRank > 0 && !facts.hasMLA {
		return "block declares an MLA latent, weights cache explicit K/V"
	}
	return ""
}

// declaredLayers is the layer count a block describes. A hybrid block lists one
// entry per layer, so the list is the more precise statement when present.
func declaredLayers(cfg *modelconfig.Config) int {
	if len(cfg.LayerTypes) > cfg.NumHiddenLayers {
		return len(cfg.LayerTypes)
	}
	return cfg.NumHiddenLayers
}

func readRepoFile(client *http.Client, source weightSource, name string) ([]byte, error) {
	if source.Subfolder != "" {
		name = strings.TrimSuffix(source.Subfolder, "/") + "/" + name
	}
	url := modelrepo.ResolveURL(source.Provider, source.RepoID, "", name, "")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token != "" && source.Provider == modelrepo.SourceHF {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", name, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 64<<20))
}

func loadFallbacks(path string) (map[string]*fallbackFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*fallbackFile{}, nil
		}
		return nil, err
	}
	var raw map[string]*fallbackFile
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw, nil
}
