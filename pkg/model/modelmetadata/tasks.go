package modelmetadata

import (
	"encoding/json"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Documents is also used when a repository API supplied metadata before its
// weights were downloaded. No model-directory discovery is implied.
type Documents struct {
	Config        string
	README        string
	Configuration string
	Modules       string
}

type TaskResult struct {
	Tasks    []string
	Priority int
	Evidence []Evidence
	Issues   []Issue
	Declared bool
}

type modelCard struct {
	PipelineTags []string
	Tags         []string
	Library      string
}

type taskClaim struct {
	tasks         []string
	priority      int
	source, field string
}

// A backbone only suggests a task; task-bearing modules and local metadata
// describe the entry itself and must not be displaced by an ancestor's card.
const (
	architectureTaskPriority  = 20
	configurationTaskPriority = 60
	tagTaskPriority           = 80
	moduleTaskPriority        = 90
	declaredTaskPriority      = 100
)

// InferTasks follows the AIOS distinction between declared metadata, structural
// evidence and an unknown task. Model names and README prose are not rules.
func InferTasks(d Documents) TaskResult {
	var claims []taskClaim
	out := TaskResult{}
	card, err := parseModelCard(d.README)
	if err != nil {
		out.Issues = append(out.Issues, issue("README.md", "invalid_metadata", err))
		out.Declared = true
		return out
	}
	add := func(tasks []string, priority int, source, field string) {
		tasks = normalizeTasks(tasks)
		if len(tasks) > 0 {
			claims = append(claims, taskClaim{tasks, priority, source, field})
		}
	}
	// AIOS consults model-card metadata before ModelScope configuration. Hub
	// exports often retain a generic backbone task in configuration.json.
	add(card.PipelineTags, declaredTaskPriority, "README.md", "pipeline_tag")
	if len(card.PipelineTags) > 0 {
		out.Declared = true
	}
	var ms struct {
		Task     string `json:"task"`
		Pipeline struct {
			Type string `json:"type"`
		} `json:"pipeline"`
	}
	if d.Configuration != "" {
		if err := json.Unmarshal([]byte(d.Configuration), &ms); err != nil {
			out.Issues = append(out.Issues, issue("configuration.json", "invalid_metadata", err))
		} else if ms.Task != "" {
			add([]string{ms.Task}, configurationTaskPriority, "configuration.json", "task")
			out.Declared = true
		} else if knownTask(ms.Pipeline.Type) {
			add([]string{ms.Pipeline.Type}, configurationTaskPriority, "configuration.json", "pipeline.type")
		}
	}
	var cfg struct {
		Architectures []string                   `json:"architectures"`
		NumLabels     *int                       `json:"num_labels"`
		ID2Label      map[string]string          `json:"id2label"`
		VisionConfig  map[string]json.RawMessage `json:"vision_config"`
		AudioConfig   map[string]json.RawMessage `json:"audio_config"`
	}
	if d.Config != "" {
		if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
			out.Issues = append(out.Issues, issue("config.json", "invalid_metadata", err))
		}
	}
	hasTag := func(tag string) bool {
		for _, t := range card.Tags {
			if strings.EqualFold(t, tag) {
				return true
			}
		}
		return false
	}
	if hasTag("reranker") || hasTag("text-ranking") {
		add([]string{"text-ranking"}, moduleTaskPriority, "README.md", "tags")
	}
	// A System One decision model scores the candidates of typed questions
	// in one forward pass. Its backbone is often a vision-language model
	// whose config reads as image-text-to-text, so the card's own tag has to
	// outrank the architecture: such a model cannot chat.
	if hasTag("decision-making") || hasTag("decision-model") {
		add([]string{"decision"}, moduleTaskPriority, "README.md", "tags")
	}
	for _, tag := range card.Tags {
		if knownTask(tag) {
			add([]string{tag}, tagTaskPriority, "README.md", "tags")
		}
	}
	for _, arch := range cfg.Architectures {
		switch {
		case strings.HasSuffix(arch, "ForRanking"):
			add([]string{"text-ranking"}, configurationTaskPriority, "config.json", "architectures")
		case strings.HasSuffix(arch, "ForSequenceClassification"):
			labels := len(cfg.ID2Label)
			if cfg.NumLabels != nil {
				labels = *cfg.NumLabels
			}
			if labels == 1 && (hasTag("sentence-transformers") || card.Library == "sentence-transformers") {
				add([]string{"text-ranking"}, moduleTaskPriority, "config.json", "architectures+labels+sentence-transformers")
			}
		case strings.HasSuffix(arch, "ForCausalLM"):
			add([]string{"text-generation"}, architectureTaskPriority, "config.json", "architectures")
		case strings.HasSuffix(arch, "ForConditionalGeneration") && cfg.VisionConfig != nil && cfg.AudioConfig == nil:
			add([]string{"image-text-to-text"}, architectureTaskPriority, "config.json", "architectures+vision_config")
			// ConditionalGeneration alone also includes speech models; it does
			// not establish a text generation API.
		}
	}
	var modules []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(d.Modules), &modules) == nil {
		for _, module := range modules {
			switch sentenceTransformerModuleClass(module.Type) {
			case "Pooling":
				add([]string{"feature-extraction"}, moduleTaskPriority, "modules.json", "type")
			case "LogitScore":
				add([]string{"text-ranking"}, moduleTaskPriority, "modules.json", "type")
			}
		}
	}
	// A broad classification declaration can be refined by the documented
	// cross-encoder/ranking evidence without calling ordinary classifiers chat.
	for i := range claims {
		if slices.Equal(claims[i].tasks, []string{"text-classification"}) {
			for _, c := range claims {
				if slices.Equal(c.tasks, []string{"text-ranking"}) && c.priority >= configurationTaskPriority {
					claims[i].tasks = []string{"text-ranking"}
					break
				}
			}
		}
	}
	best := -1
	conflict := false
	for _, c := range claims {
		out.Evidence = append(out.Evidence, Evidence{Source: c.source, Field: c.field, Value: strings.Join(c.tasks, ",")})
		if c.priority > best {
			best, out.Tasks, conflict = c.priority, slices.Clone(c.tasks), false
		} else if c.priority == best && !sameTasks(out.Tasks, c.tasks) {
			conflict = true
		}
	}
	if conflict {
		out.Tasks = nil
		out.Declared = true
		out.Issues = append(out.Issues, issue(".", "conflicting_tasks", nil))
	}
	out.Priority = max(best, 0)
	return out
}

func sameTasks(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func normalizeTasks(tasks []string) []string {
	var out []string
	for _, task := range tasks {
		if t := canonicalTask(task); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func canonicalTask(task string) string {
	switch t := strings.ToLower(strings.TrimSpace(task)); t {
	case "rerank", "reranker", "text-reranking":
		return "text-ranking"
	case "sentence-similarity", "text-embedding", "text-embeddings", "sentence-embedding", "multi-modal-embedding":
		return "feature-extraction"
	default:
		return t
	}
}

func knownTask(task string) bool {
	switch canonicalTask(task) {
	case "text-ranking", "feature-extraction", "decision", "text-generation", "text2text-generation", "image-text-to-text", "text-classification", "text-to-image", "image-to-image", "automatic-speech-recognition", "text-to-speech", "text-to-audio", "text-to-video", "image-to-video", "image-text-to-video":
		return true
	}
	return false
}

func parseModelCard(readme string) (modelCard, error) {
	readme = strings.ReplaceAll(strings.TrimLeft(readme, "\ufeff \t\r\n"), "\r\n", "\n")
	lines := strings.Split(readme, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return modelCard{}, nil
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		var doc struct {
			Pipeline yaml.Node `yaml:"pipeline_tag"`
			Tags     []string  `yaml:"tags"`
			Library  string    `yaml:"library_name"`
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &doc); err != nil {
			return modelCard{}, err
		}
		card := modelCard{Tags: doc.Tags, Library: doc.Library}
		switch doc.Pipeline.Kind {
		case yaml.ScalarNode:
			if doc.Pipeline.Tag == "!!str" {
				card.PipelineTags = []string{doc.Pipeline.Value}
			}
		case yaml.SequenceNode:
			for _, item := range doc.Pipeline.Content {
				if item.Kind == yaml.ScalarNode && item.Tag == "!!str" {
					card.PipelineTags = append(card.PipelineTags, item.Value)
				}
			}
		}
		return card, nil
	}
	return modelCard{}, errorsTruncatedCard
}
