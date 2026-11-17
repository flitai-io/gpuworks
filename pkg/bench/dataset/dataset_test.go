package dataset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

func TestAgenticDefaultShape(t *testing.T) {
	c, err := ExpandAgenticConfig(Config{Kind: "agentic", Preset: AgenticDefaultPreset})
	if err != nil {
		t.Fatal(err)
	}
	if c.MeanInputTokens != 80_000 || c.FirstTurnInputTokens != 74_162 || c.PeakInputTokens != 85_838 || c.RequiredContextTokens != 90_000 || c.Turns != 13 {
		t.Fatalf("unexpected default shape: %+v", c)
	}
	if c.DesignPrefixReuseRate < .92 || c.DesignPrefixReuseRate > .921 || len(c.WorkloadFingerprint) != 64 {
		t.Fatalf("unexpected reuse/fingerprint: %+v", c)
	}
}

// Every preset row must be exactly what the generator derives from it. The
// table is what the UI offers and what a run records, while the generator
// computes the first turn, peak and context itself, so a hand-edited row that
// drifts from the formula would advertise one workload and measure another.
func TestAgenticPresetsMatchTheGeneratorFormula(t *testing.T) {
	presets := AgenticPresets()
	if len(presets) == 0 {
		t.Fatal("no agentic presets")
	}
	seen := make(map[string]bool, len(presets))
	previousMean := 0
	for _, preset := range presets {
		if seen[preset.Value] {
			t.Fatalf("duplicate preset %q", preset.Value)
		}
		seen[preset.Value] = true
		if preset.MeanInputTokens <= previousMean {
			t.Fatalf("preset %s is out of mean order: %d after %d", preset.Value, preset.MeanInputTokens, previousMean)
		}
		previousMean = preset.MeanInputTokens
		// Only the preset name is given: the row's own mean must reproduce the
		// remaining columns, which is what makes the table a fixed point.
		expanded, err := ExpandAgenticConfig(Config{Kind: "agentic", Preset: preset.Value})
		if err != nil {
			t.Fatalf("%s: %v", preset.Value, err)
		}
		if expanded.MeanInputTokens != preset.MeanInputTokens || expanded.FirstTurnInputTokens != preset.FirstTurnInputTokens ||
			expanded.PeakInputTokens != preset.PeakInputTokens || expanded.RequiredContextTokens != preset.RequiredContextTokens {
			t.Fatalf("%s: table=%+v derived mean=%d first=%d peak=%d context=%d", preset.Value, preset,
				expanded.MeanInputTokens, expanded.FirstTurnInputTokens, expanded.PeakInputTokens, expanded.RequiredContextTokens)
		}
	}
	if !seen[AgenticDefaultPreset] {
		t.Fatalf("default preset %s is missing from the table", AgenticDefaultPreset)
	}
	if !seen["agentic-20k-v1"] {
		t.Fatal("the 20K preset is missing from the table")
	}
}

func TestAgenticLengthDriftWarningAllowsTenPercent(t *testing.T) {
	for _, actual := range []float64{9_000, 10_000, 11_000} {
		if AgenticLengthDriftExceeded(actual, 10_000) {
			t.Fatalf("actual %.0f should be within the 10%% tolerance", actual)
		}
	}
	for _, actual := range []float64{8_999, 11_001} {
		if !AgenticLengthDriftExceeded(actual, 10_000) {
			t.Fatalf("actual %.0f should exceed the 10%% tolerance", actual)
		}
	}
}

func TestAgenticGenerationRequiresTokenizerAndIsDeterministic(t *testing.T) {
	c := Config{Kind: "agentic", Preset: "agentic-10k-v1", Turns: 3, Seed: 42}
	if _, err := GenerateWithTokenizer(c, 1, nil); err == nil {
		t.Fatal("agentic generation unexpectedly accepted estimate mode")
	}
	tok, err := tokenizer.Load(bytes.NewBufferString(`{"model":{"vocab":{"a":0,"b":1,"c":2,"d":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := GenerateWithTokenizer(c, 2, tok)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := GenerateWithTokenizer(c, 2, tok)
	if len(first) != 2 || len(first[0].FollowUps) != 2 || first[0].OutputTokens != AgenticOutputTokens {
		t.Fatalf("unexpected generated shape: %+v", first)
	}
	if first[0].Messages[0].Content != second[0].Messages[0].Content || first[0].Messages[0].Content == first[1].Messages[0].Content {
		t.Fatal("sessions are not reproducible and unique")
	}
}

func TestEmbeddedBenchmarkAssetsArePresentAndBounded(t *testing.T) {
	for _, name := range []string{"data/multiturn.jsonl.gz"} {
		f, err := embedded.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || len(b) == 0 {
			t.Fatalf("asset %s len=%d err=%v", name, len(b), err)
		}
	}
	var total int
	for _, name := range []string{"data/multiturn.jsonl.gz"} {
		f, _ := embedded.Open(name)
		b, _ := io.ReadAll(f)
		_ = f.Close()
		total += len(b)
	}
	if total >= 5*1024*1024 {
		t.Fatalf("embedded assets too large: %d", total)
	}
}

func TestBuiltinDatasetLoadFailureDoesNotFallBackToRandom(t *testing.T) {
	for name, generate := range map[string]func() error{
		"multiturn": func() error { _, err := multiturnFS(Config{Kind: "multiturn", Seed: 1}, 1, fstest.MapFS{}); return err },
		"multiturn malformed row": func() error {
			_, err := multiturnFS(Config{Kind: "multiturn", Seed: 1}, 1, fstest.MapFS{"data/multiturn.jsonl": &fstest.MapFile{Data: []byte("{bad json\n")}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := generate()
			var loadErr *LoadError
			if !errors.As(fmt.Errorf("point 1: %w", err), &loadErr) || loadErr.Code() != LoadErrorCode || loadErr.Params()["dataset"] != "multiturn" {
				t.Fatalf("error = %v, want a *LoadError with %s", err, LoadErrorCode)
			}
			// The code is not spelled into the sentence any more.
			if strings.Contains(err.Error(), LoadErrorCode) || !strings.HasPrefix(err.Error(), "load the built-in multiturn dataset: ") {
				t.Fatalf("text = %q", err)
			}
		})
	}
}

func TestMultiturnAssetHasRealisticScaleAndTurns(t *testing.T) {
	f, err := openEmbedded("data/multiturn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	rows, multiTurn := 0, 0
	for scanner.Scan() {
		var row struct {
			Messages []Message `json:"messages"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			rows++
			userTurns := 0
			for _, message := range row.Messages {
				if message.Role == "user" {
					userTurns++
				}
			}
			if userTurns >= 2 {
				multiTurn++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if rows < 1000 || multiTurn < 100 {
		t.Fatalf("multiturn asset is only a smoke fixture: rows=%d multi_turn=%d", rows, multiTurn)
	}
}

func TestCompressedAssetsAreReadable(t *testing.T) {
	for _, name := range []string{"data/multiturn.jsonl"} {
		f, err := openEmbedded(name)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || len(b) == 0 {
			t.Fatalf("asset %s len=%d err=%v", name, len(b), err)
		}
	}
}

func TestGenerateDatasets(t *testing.T) {
	for _, kind := range []string{"random", "multiturn"} {
		got, err := Generate(Config{Kind: kind, InputTokens: 8, OutputTokens: 4, Turns: 2, Seed: 7}, 3)
		if err != nil || len(got) != 3 || len(got[0].Messages) == 0 {
			t.Fatalf("%s: got=%+v err=%v", kind, got, err)
		}
	}
}

func TestRealDatasetsDriveRuntimeSamples(t *testing.T) {
	multiturn, err := Generate(Config{Kind: "multiturn", InputTokens: 8, OutputTokens: 4, Turns: 3, Seed: 7}, 20)
	if err != nil {
		t.Fatal(err)
	}
	followUps := 0
	for _, sample := range multiturn {
		followUps += len(sample.FollowUps)
	}
	if followUps == 0 {
		t.Fatal("OASST1 runtime samples discarded all real follow-up turns")
	}

}

func TestRandomDatasetUsesFreshEntropyByDefault(t *testing.T) {
	config := Config{Kind: "random", InputTokens: 32, OutputTokens: 4}
	first, err := Generate(config, 4)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(config, 4)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Messages[0].Content == second[0].Messages[0].Content {
		t.Fatalf("independent random generations replayed the same prompt: %q", first[0].Messages[0].Content)
	}
	config.Seed = 17
	repeated, _ := Generate(config, 4)
	repeatedAgain, _ := Generate(config, 4)
	if repeated[0].Messages[0].Content != repeatedAgain[0].Messages[0].Content {
		t.Fatal("an explicit seed must remain reproducible")
	}
	if strings.Contains(repeated[0].Messages[0].Content, "benchmark") || len(repeated[0].Messages[0].Content) != 128 || !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(repeated[0].Messages[0].Content) {
		t.Fatalf("random prompt is not a pure 4-char/token synthetic payload: %q", repeated[0].Messages[0].Content)
	}
}

func TestImportJSONLThreeForms(t *testing.T) {
	data := "hello\n[{\"role\":\"user\",\"content\":\"hi\"}]\n{\"messages\":[{\"role\":\"user\",\"content\":\"hey\"}]}\n"
	got, err := ImportJSONL(strings.NewReader(data))
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestImportedDatasetRandomizesWithoutReplacementAndPreservesShape(t *testing.T) {
	samples := []Sample{
		{Messages: []Message{{Role: "user", Content: "zero"}}, FollowUps: []Message{{Role: "user", Content: "zero-follow-up"}}, InputTokens: 10, OutputTokens: 20, WaitMillis: 30},
		{Messages: []Message{{Role: "user", Content: "one"}}, OutputTokens: 21},
		{Messages: []Message{{Role: "user", Content: "two"}}, OutputTokens: 22},
	}
	got, err := Generate(Config{Kind: "imported", Seed: 17, Samples: samples}, 5)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sample := range got[:len(samples)] {
		content := sample.Messages[0].Content
		if seen[content] {
			t.Fatalf("sample repeated before corpus exhaustion: %+v", got)
		}
		seen[content] = true
	}
	if got[3].Messages[0].Content != got[0].Messages[0].Content || got[4].Messages[0].Content != got[1].Messages[0].Content {
		t.Fatalf("overflow did not reuse the randomized corpus order: %+v", got)
	}
	var preserved *Sample
	for i := range got {
		if got[i].Messages[0].Content == "zero" {
			preserved = &got[i]
			break
		}
	}
	if preserved == nil || len(preserved.FollowUps) != 1 || preserved.InputTokens != 10 || preserved.OutputTokens != 20 || preserved.WaitMillis != 30 {
		t.Fatalf("imported sample shape was not preserved: %+v", preserved)
	}
}

func TestImportedDatasetDefaultsMissingOutputLimit(t *testing.T) {
	got, err := Generate(Config{Kind: "imported", Seed: 1, Samples: []Sample{{Messages: []Message{{Role: "user", Content: "hello"}}}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].OutputTokens != DefaultImportedOutputTokens {
		t.Fatalf("output_tokens = %d, want default %d", got[0].OutputTokens, DefaultImportedOutputTokens)
	}
}
