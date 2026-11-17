package uxrecord

import "testing"

func TestStageOf(t *testing.T) {
	for kind, want := range map[SegmentKind]Stage{
		SegmentGateway: StageFirstToken, SegmentPDPrefill: StageFirstToken, SegmentPreprocess: StageFirstToken,
		SegmentQueue: StageFirstToken, SegmentWaiting: StageFirstToken, SegmentPrefill: StageFirstToken,
		SegmentThinking: StageDecode, SegmentAnswering: StageDecode, SegmentTail: StageTail,
	} {
		if got := StageOf(kind, true); got != want {
			t.Errorf("%s = %q, want %q", kind, got, want)
		}
		if got := StageOf(kind, false); got != "" {
			t.Errorf("%s unstaged = %q", kind, got)
		}
	}
	if len(SegmentKinds()) != 9 {
		t.Fatal("a new segment kind needs a stage")
	}
}

func TestFirstTokenOf(t *testing.T) {
	for _, tc := range []struct{ think, answer, want int32 }{
		{31, 400, 31}, {500, 40, 40}, {Unknown, 40, 40}, {31, Unknown, 31}, {Unknown, Unknown, Unknown}, {0, 5, 0},
	} {
		if got := FirstTokenOf(tc.think, tc.answer); got != tc.want {
			t.Errorf("FirstTokenOf(%d, %d) = %d, want %d", tc.think, tc.answer, got, tc.want)
		}
	}
}

// The segments up to the first token add up to it, whatever the engine
// reported: the TTFT the detail shows is their sum.
func TestFirstTokenSegmentsAddUpToTTFT(t *testing.T) {
	for _, r := range []Request{
		{Stream: true, DispatchMS: 2, FirstThinkMS: 31, FirstAnswerMS: 400, LastChunkMS: 900, EndMS: 901, EngineQueueMS: 5, EnginePrefillMS: Unknown, EngineDecodeMS: 800},
		{Stream: true, DispatchMS: 1, FirstThinkMS: Unknown, FirstAnswerMS: 2130, LastChunkMS: 5600, EndMS: 5700, EngineQueueMS: 0, EnginePrefillMS: 2100, EngineDecodeMS: 3500},
		{Stream: true, DispatchMS: 1, FirstThinkMS: Unknown, FirstAnswerMS: 368, LastChunkMS: 6500, EndMS: 6600, EngineQueueMS: Unknown, EnginePrefillMS: Unknown, EngineDecodeMS: Unknown},
	} {
		sum := 0.0
		for _, s := range SegmentsOf(r) {
			if StageOf(s.Kind, true) == StageFirstToken {
				sum += s.MS
			}
		}
		if sum != float64(r.FirstTokenMS()) {
			t.Errorf("first-token segments = %v, want %d: %v", sum, r.FirstTokenMS(), SegmentsOf(r))
		}
	}
}

func TestSpeedsOf(t *testing.T) {
	base := Request{Usage: true, PromptTokens: 4323, CachedTokens: 0, CompletionTokens: 1024, ReasoningTokens: Unknown,
		FirstThinkMS: Unknown, FirstAnswerMS: 2130, LastChunkMS: 5630, EnginePrefillMS: 2100, EngineDecodeMS: Unknown}
	got := SpeedsOf(base)
	// Prefill: 4323 uncached tokens in 2.1 s; decode: the 1023 tokens after
	// the first over the 3.5 s stream.
	if int(got.PrefillTokS) != 2058 || got.DecodeTokS < 292 || got.DecodeTokS > 293 || got.TPOTMS < 3.42 || got.TPOTMS > 3.43 {
		t.Fatalf("speeds = %+v", got)
	}
	engine := base
	engine.EngineDecodeMS = 2048
	if got := SpeedsOf(engine); got.DecodeTokS != 500 || got.TPOTMS != 2 {
		t.Fatalf("engine decode = %+v", got)
	}
	pd := base
	pd.PD = &PDStage{PrefillComputeMS: 1000}
	if got := SpeedsOf(pd); int(got.PrefillTokS) != 4323 {
		t.Fatalf("a PD handoff's prefill is the prefill instance's: %+v", got)
	}
	for name, r := range map[string]Request{
		"no usage":      {Usage: false, CompletionTokens: 1024, FirstAnswerMS: 1, LastChunkMS: 100, EngineDecodeMS: Unknown, EnginePrefillMS: Unknown},
		"one token":     {Usage: true, CompletionTokens: 1, CachedTokens: Unknown, FirstAnswerMS: 1, LastChunkMS: 100, EngineDecodeMS: Unknown, EnginePrefillMS: Unknown},
		"no stream":     {Usage: true, CompletionTokens: 10, CachedTokens: Unknown, FirstThinkMS: Unknown, FirstAnswerMS: Unknown, LastChunkMS: Unknown, EngineDecodeMS: Unknown, EnginePrefillMS: Unknown},
		"cache unknown": {Usage: true, PromptTokens: 100, CachedTokens: Unknown, CompletionTokens: 0, EnginePrefillMS: 50, EngineDecodeMS: Unknown},
	} {
		if got := SpeedsOf(r); got != (Speeds{}) {
			t.Errorf("%s = %+v, want none", name, got)
		}
	}
}
