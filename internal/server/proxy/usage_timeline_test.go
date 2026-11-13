package proxy

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// What the gateway reads from a real vLLM 0.30 reply (Qwen3-0.6B, reasoning
// parser on, --enable-prompt-tokens-details --enable-per-request-metrics):
// the reasoning streams under "reasoning", the answer under "content", the
// usage carries the cache hit and the reasoning tokens, and the per-request
// metrics carry the queue, prefill and decode times.
func TestObserverReadsVLLMTimelineUsageAndRequestStats(t *testing.T) {
	rules := inferenceapi.ResolveRequestStats("vllm", nil)
	for _, tc := range []struct {
		fixture                              string
		think, answer                        bool
		prompt, completion, cached, thinking uint64
		finish                               string
		prefillMS, decodeMS                  float64
	}{
		{"vllm-0.30-think-usage.sse", true, true, 21, 362, 16, 347, "stop", 60.37, 11327.5},
		{"vllm-0.30-usage.sse", true, false, 21, 200, 16, 199, "length", 58.66, 5843.5},
		{"vllm-0.30-nothink-usage.sse", false, true, 0, 0, 0, 0, "", 0, 0},
	} {
		body := readFixture(t, tc.fixture)
		for seed := int64(1); seed <= 10; seed++ {
			o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(100, 0))
			o.reading.rules = rules
			feed(t, o, body, seed)
			stats := o.stats(true)
			if stats.hasThink != tc.think || stats.hasAnswer != tc.answer || !stats.usage {
				t.Fatalf("%s seed %d: think=%v answer=%v usage=%v", tc.fixture, seed, stats.hasThink, stats.hasAnswer, stats.usage)
			}
			if tc.think && tc.answer && !(stats.firstThink < stats.firstAnswer) {
				t.Fatalf("%s: reasoning must come before the answer: %v vs %v", tc.fixture, stats.firstThink, stats.firstAnswer)
			}
			if !stats.cachedKnown || !stats.reasoningKnown {
				t.Fatalf("%s: cache/reasoning figures not read: %+v", tc.fixture, stats)
			}
			if stats.events < stats.thinkEvents || stats.lastEvent < stats.firstToken {
				t.Fatalf("%s: inconsistent timeline %+v", tc.fixture, stats)
			}
			if tc.prompt == 0 {
				continue
			}
			if stats.prompt != tc.prompt || stats.completion != tc.completion || stats.cached != tc.cached || stats.reasoning != tc.thinking || stats.finishReason != tc.finish {
				t.Fatalf("%s seed %d: usage %+v", tc.fixture, seed, stats)
			}
			prefill := stats.engine[inferencemetrics.RequestPrefillSeconds] * 1000
			decode := stats.engine[inferencemetrics.RequestDecodeSeconds] * 1000
			if _, ok := stats.engine[inferencemetrics.RequestQueueSeconds]; !ok || prefill < tc.prefillMS-0.1 || prefill > tc.prefillMS+0.1 || decode < tc.decodeMS-0.1 || decode > tc.decodeMS+0.1 {
				t.Fatalf("%s: engine stats %+v", tc.fixture, stats.engine)
			}
		}
	}
}

// A plain stream (the client did not ask for usage, the gateway did not
// inject) still times the stream; it has no usage to read.
func TestObserverTimesAStreamWithoutUsage(t *testing.T) {
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	o.reading.rules = inferenceapi.ResolveRequestStats("vllm", nil)
	feed(t, o, readFixture(t, "vllm-0.30-think-plain.sse"), 2)
	stats := o.stats(true)
	if stats.usage || !stats.hasThink || !stats.hasAnswer || stats.events < 300 || len(stats.engine) != 0 || stats.finishReason != "stop" {
		t.Fatalf("plain stream = %+v", stats)
	}
}

// A plain JSON reply carries usage and metrics in its body.
func TestObserverReadsVLLMPlainReply(t *testing.T) {
	o := newResponseObserver(chatPath, 200, jsonHeader(), time.Unix(0, 0))
	o.reading.rules = inferenceapi.ResolveRequestStats("vllm", nil)
	feed(t, o, readFixture(t, "vllm-0.30-plain.json"), 4)
	stats := o.stats(true)
	if !stats.usage || stats.cached != 16 || stats.reasoning != 199 || stats.hasFirstToken || stats.finishReason != "length" {
		t.Fatalf("plain reply = %+v", stats)
	}
	if got := stats.engine[inferencemetrics.RequestPrefillSeconds]; got < 0.058 || got > 0.059 {
		t.Fatalf("prefill seconds = %v", got)
	}
}

// TensorFold and Strata report their own stats in the reply; a template
// declares where (api.request_stats).
func TestObserverReadsDeclaredStatsOfCallableEngines(t *testing.T) {
	for _, tc := range []struct {
		fixture         string
		declared        inferenceapi.MetricMap
		prefill, decode float64
	}{
		{"tensorfold-gb10-stream.sse", inferenceapi.MetricMap{"tensorfold.prefill_s": {To: "prefill_seconds"}, "tensorfold.decode_s": {To: "decode_seconds"}}, 0.1112, 2.272},
		{"strata-stream.sse", inferenceapi.MetricMap{"timings.prompt_ms": {To: "prefill_seconds", Scale: 0.001}, "timings.predicted_ms": {To: "decode_seconds", Scale: 0.001}}, 0.0354, 0.8973},
	} {
		o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
		o.reading.rules = inferenceapi.ResolveRequestStats("callable", tc.declared)
		feed(t, o, readFixture(t, tc.fixture), 9)
		stats := o.stats(true)
		near := func(a, b float64) bool { return a > b-1e-6 && a < b+1e-6 }
		if !near(stats.engine["prefill_seconds"], tc.prefill) || !near(stats.engine["decode_seconds"], tc.decode) {
			t.Fatalf("%s: engine stats %+v", tc.fixture, stats.engine)
		}
	}
}

func TestFieldNonEmpty(t *testing.T) {
	for payload, want := range map[string]bool{
		`{"delta":{"content":"x"}}`:                    true,
		`{"delta":{"content": " "}}`:                   true,
		`{"delta":{"content":""}}`:                     false,
		`{"delta":{"content":null}}`:                   false,
		`{"delta":{"tool_calls":[{"id":"1"}]}}`:        true,
		`{"delta":{"tool_calls":[ ]}}`:                 false,
		`{"delta":{"reasoning_content":"x"}}`:          false, // the key asked is content
		`{"delta":{"role":"a","x":"\"content\":\"y"}}`: false,
	} {
		if got := fieldNonEmpty([]byte(payload), "content"); got != want && payload != `{"delta":{"tool_calls":[{"id":"1"}]}}` && payload != `{"delta":{"tool_calls":[ ]}}` {
			t.Errorf("content in %s: got %v, want %v", payload, got, want)
		}
	}
	if !fieldNonEmpty([]byte(`{"delta":{"tool_calls":[{"id":"1"}]}}`), "tool_calls") || fieldNonEmpty([]byte(`{"delta":{"tool_calls":[ ]}}`), "tool_calls") {
		t.Fatal("tool_calls arrays")
	}
}

func TestDeltaTextReadsTheDeltaMembers(t *testing.T) {
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	defer o.stats(false)
	for delta, want := range map[string][2]bool{
		`:{"role":"assistant","content":""}`:                     {false, false},
		`:{"role":"assistant","content":"hi"}`:                   {false, true},
		`: {"reasoning": " step"}`:                               {true, false},
		`:{"reasoning_content":"a \"quoted\" \\ word"}`:          {true, false},
		`:{"content":"x \"reasoning\":\"y\"","reasoning":null}`:  {false, true},
		`:{"content":null,"tool_calls":[{"index":0,"id":"c1"}]}`: {false, true},
		`:{"tool_calls":[]}`:                                     {false, false},
		`:{"reasoning":"r","content":"c"}`:                       {true, true},
		`:{"index":0,"reasoning":"r"}`:                           {true, false},
		`:{}`:                                                    {false, false},
		`:null`:                                                  {false, false},
		`:{"content":"unterminated`:                              {false, true},
	} {
		think, answer := o.deltaText([]byte(delta))
		if think != want[0] || answer != want[1] {
			t.Errorf("%s: think=%v answer=%v, want %v", delta, think, answer, want)
		}
	}
}

// What the gateway reads from a real SGLang 0.5.21 reply (Qwen3.5-0.8B,
// reasoning parser qwen3, --enable-cache-report): reasoning streams under
// "reasoning_content", the reasoning tokens sit at the top of usage, and the
// cache figure is left out when nothing was cached - which reads as zero only
// for a deployment whose cache report is on.
func TestObserverReadsSGLangUsage(t *testing.T) {
	for _, tc := range []struct {
		fixture                              string
		cacheOmitsZero                       bool
		think, answer                        bool
		prompt, completion, thinking, cached uint64
		cachedKnown                          bool
	}{
		{"sglang-0.5.21-think-usage.sse", true, true, false, 25, 3, 3, 0, true},
		{"sglang-0.5.21-think-usage.sse", false, true, false, 25, 3, 3, 0, false},
		{"sglang-0.5.21-nothink-usage.sse", true, false, true, 0, 0, 0, 0, true},
		{"sglang-0.5.21-cached-usage.sse", false, false, true, 1712, 4, 0, 1664, true},
	} {
		body := readFixture(t, tc.fixture)
		for seed := int64(1); seed <= 10; seed++ {
			o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(100, 0))
			o.reading.cacheOmitsZero = tc.cacheOmitsZero
			feed(t, o, body, seed)
			stats := o.stats(true)
			if !stats.usage || stats.hasThink != tc.think || stats.hasAnswer != tc.answer || stats.finishReason != "stop" {
				t.Fatalf("%s seed %d: %+v", tc.fixture, seed, stats)
			}
			if stats.cachedKnown != tc.cachedKnown || stats.cached != tc.cached || !stats.reasoningKnown {
				t.Fatalf("%s (omits zero %v): cached %d known %v, reasoning known %v", tc.fixture, tc.cacheOmitsZero, stats.cached, stats.cachedKnown, stats.reasoningKnown)
			}
			if tc.prompt != 0 && (stats.prompt != tc.prompt || stats.completion != tc.completion || stats.reasoning != tc.thinking) {
				t.Fatalf("%s: usage %+v", tc.fixture, stats)
			}
		}
	}
}

func TestRequestTimingNullIsUnknownAndExplicitZeroIsMeasured(t *testing.T) {
	for _, queue := range []string{"null", "0", "20300"} {
		t.Run(queue, func(t *testing.T) {
			body := strings.Replace(readFixture(t, "vllm-0.30-plain.json"), `"queue_time_ms":0.00895373523235321`, `"queue_time_ms":`+queue, 1)
			o := newResponseObserver(chatPath, 200, jsonHeader(), time.Unix(0, 0))
			o.reading.rules = inferenceapi.ResolveRequestStats("vllm", nil)
			feed(t, o, body, 4)
			got := buildRecord(requestFacts{}, "instance", 200, time.Second, o.stats(true), uxrecord.OutcomeOK, time.Time{})
			want := map[string]int32{"null": uxrecord.Unknown, "0": 0, "20300": 20300}[queue]
			if got.EngineQueueMS != want {
				t.Fatalf("queue %s: got %d, want %d", queue, got.EngineQueueMS, want)
			}
		})
	}
}
