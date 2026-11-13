package dataset

import (
	"bufio"
	"compress/gzip"
	crand "crypto/rand"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

//go:embed data/*
var embedded embed.FS

type Config struct {
	Kind                      string  `json:"kind"`
	Mode                      string  `json:"mode,omitempty"`
	Preset                    string  `json:"preset,omitempty"`
	GeneratorVersion          int     `json:"generator_version,omitempty"`
	InputTokens               int     `json:"input_tokens,omitempty"`
	MeanInputTokens           int     `json:"mean_input_tokens,omitempty"`
	FirstTurnInputTokens      int     `json:"first_turn_input_tokens,omitempty"`
	SubsequentTurnInputTokens int     `json:"subsequent_turn_input_tokens,omitempty"`
	PeakInputTokens           int     `json:"peak_input_tokens,omitempty"`
	RequiredContextTokens     int     `json:"required_context_tokens,omitempty"`
	DesignPrefixReuseRate     float64 `json:"design_prefix_reuse_rate,omitempty"`
	OutputTokens              int     `json:"output_tokens,omitempty"`
	Turns                     int     `json:"turns,omitempty"`
	Seed                      int64   `json:"seed,omitempty"`
	// IgnoreEOS asks the engine to keep generating to the requested output
	// length, so every request yields exactly its target OSL and runs compare
	// across models. Only synthetic workloads offer it (IgnoreEOSKinds); nil
	// is a run created before the switch existed, see EffectiveIgnoreEOS.
	IgnoreEOS           *bool    `json:"ignore_eos,omitempty"`
	WorkloadFingerprint string   `json:"workload_fingerprint,omitempty"`
	Samples             []Sample `json:"samples,omitempty"`
	Profile             *Profile `json:"profile,omitempty"`
}

type Profile struct {
	SampleCount     int `json:"sample_count"`
	MaxInputTokens  int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

const (
	LoadErrorCode               = "BENCHMARK_DATASET_LOAD_FAILED"
	DefaultImportedOutputTokens = 32
)

type Sample struct {
	Messages     []Message `json:"messages"`
	FollowUps    []Message `json:"follow_ups,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	WaitMillis   int       `json:"wait_millis,omitempty"`
}
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// IgnoreEOSKinds lists the dataset kinds whose requests may carry
// ignore_eos. Corpus datasets (multiturn, imported) end naturally.
func IgnoreEOSKinds() []string { return []string{"random", "agentic"} }

// EffectiveIgnoreEOS reports whether requests of this dataset carry
// ignore_eos. A run that stored no choice keeps the behaviour it was created
// with: agentic sent it, random did not.
func (c Config) EffectiveIgnoreEOS() bool {
	switch c.Kind {
	case "", "random":
		return c.IgnoreEOS != nil && *c.IgnoreEOS
	case "agentic":
		return c.IgnoreEOS == nil || *c.IgnoreEOS
	default:
		return false
	}
}

func Generate(c Config, n int) ([]Sample, error) {
	return GenerateWithTokenizer(c, n, nil)
}

func GenerateWithTokenizer(c Config, n int, tok *tokenizer.Tokenizer) ([]Sample, error) {
	if err := ValidateAllocation(c, n); err != nil {
		return nil, err
	}
	if c.Seed == 0 {
		c.Seed = randomSeed()
	}
	switch c.Kind {
	case "", "random":
		return random(c, n, tok), nil
	case "multiturn":
		return multiturn(c, n)
	case "agentic":
		return agentic(c, n, tok)
	case "imported":
		if len(c.Samples) == 0 {
			return nil, fmt.Errorf("imported dataset has no samples")
		}
		r := rand.New(rand.NewSource(c.Seed))
		order := r.Perm(len(c.Samples))
		out := make([]Sample, n)
		for i := range out {
			out[i] = c.Samples[order[i%len(order)]]
			if out[i].OutputTokens <= 0 {
				out[i].OutputTokens = DefaultImportedOutputTokens
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown dataset kind %q", c.Kind)
	}
}
func random(c Config, n int, tok *tokenizer.Tokenizer) []Sample {
	r := rand.New(rand.NewSource(c.Seed))
	out := make([]Sample, n)
	for i := range out {
		text := randomText(c.InputTokens, r)
		if tok != nil && c.Mode == "exact" {
			text = tok.Sample(r, c.InputTokens)
		}
		out[i] = Sample{Messages: []Message{{Role: "user", Content: text}}, InputTokens: c.InputTokens, OutputTokens: c.OutputTokens}
	}
	return out
}

func randomSeed() int64 {
	var raw [8]byte
	if _, err := crand.Read(raw[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(raw[:]))
	}
	return time.Now().UnixNano()
}

func randomText(tokens int, r *rand.Rand) string {
	if tokens < 1 {
		tokens = 1
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	// Estimate mode consistently uses four independently sampled alphanumeric
	// characters per target token. There is deliberately no shared corpus,
	// language preset, or fixed separator pattern that could inflate cache hits.
	target := tokens * 4
	buf := make([]byte, target)
	for i := range buf {
		buf[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(buf)
}
func multiturn(c Config, n int) ([]Sample, error) {
	return multiturnFS(c, n, embedded)
}

func multiturnFS(c Config, n int, fsys fs.FS) ([]Sample, error) {
	if c.Turns < 1 {
		c.Turns = 3
	}
	conversations, err := loadConversationSamplesFS(fsys, "data/multiturn.jsonl", c.Turns)
	if err != nil {
		return nil, fmt.Errorf("%s: multiturn: %w", LoadErrorCode, err)
	}
	if len(conversations) == 0 {
		return nil, fmt.Errorf("%s: multiturn: dataset contains no samples", LoadErrorCode)
	}
	r := rand.New(rand.NewSource(c.Seed))
	order := r.Perm(len(conversations))
	out := make([]Sample, n)
	for i := range out {
		sample := conversations[order[i%len(order)]]
		sample.InputTokens = c.InputTokens
		sample.OutputTokens = c.OutputTokens
		out[i] = sample
	}
	return out, nil
}
func loadConversationSamplesFS(fsys fs.FS, name string, maxTurns int) ([]Sample, error) {
	f, err := openEmbeddedFS(fsys, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var out []Sample
	for sc.Scan() {
		var row struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("decode conversation row: %w", err)
		}
		var users []Message
		for _, m := range row.Messages {
			if m.Role == "user" && m.Content != "" {
				users = append(users, Message{Role: "user", Content: m.Content})
				if len(users) == maxTurns {
					break
				}
			}
		}
		if len(users) > 0 {
			out = append(out, Sample{Messages: users[:1], FollowUps: users[1:]})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func openEmbedded(name string) (io.ReadCloser, error) {
	return openEmbeddedFS(embedded, name)
}

func openEmbeddedFS(fsys fs.FS, name string) (io.ReadCloser, error) {
	if f, err := fsys.Open(name + ".gz"); err == nil {
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &gzipReadCloser{Reader: gz, file: f}, nil
	}
	return fsys.Open(name)
}

type gzipReadCloser struct {
	*gzip.Reader
	file io.Closer
}

func (r *gzipReadCloser) Close() error {
	err := r.Reader.Close()
	if e := r.file.Close(); err == nil {
		err = e
	}
	return err
}
func ImportJSONL(rd io.Reader) ([]Sample, error) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 4096), 8<<20)
	var out []Sample
	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			v = raw
		}
		s, err := parseValue(v)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func parseValue(v any) (Sample, error) {
	switch x := v.(type) {
	case string:
		return Sample{Messages: []Message{{Role: "user", Content: x}}}, nil
	case []any:
		return parseMessages(x)
	case map[string]any:
		if m, ok := x["messages"].([]any); ok {
			sample, err := parseMessages(m)
			if err != nil {
				return sample, err
			}
			if followUps, ok := x["follow_ups"].([]any); ok {
				parsed, err := parseMessages(followUps)
				if err != nil {
					return sample, err
				}
				sample.FollowUps = parsed.Messages
			}
			sample.InputTokens = jsonInt(x["input_tokens"])
			sample.OutputTokens = jsonInt(x["output_tokens"])
			sample.WaitMillis = jsonInt(x["wait_millis"])
			return sample, nil
		}
		if p, ok := x["prompt"].(string); ok {
			return Sample{Messages: []Message{{Role: "user", Content: p}}}, nil
		}
		return Sample{}, fmt.Errorf("object must contain messages or prompt")
	default:
		return Sample{}, fmt.Errorf("expected string, messages array, or request object")
	}
}

func jsonInt(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}
func parseMessages(a []any) (Sample, error) {
	s := Sample{}
	for _, v := range a {
		m, ok := v.(map[string]any)
		if !ok {
			return s, fmt.Errorf("message must be object")
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "" || content == "" {
			return s, fmt.Errorf("message requires role and content")
		}
		s.Messages = append(s.Messages, Message{Role: role, Content: content})
	}
	if len(s.Messages) == 0 {
		return s, fmt.Errorf("messages must not be empty")
	}
	return s, nil
}
