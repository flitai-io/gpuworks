package traceexport

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The OTLP/JSON shapes (opentelemetry-proto, OTLP/HTTP JSON encoding): ids are
// hex strings, 64-bit integers are decimal strings, enums are numbers. They
// are written by encode, field names as the tags say.

type span struct {
	TraceID      string      `json:"traceId"`
	SpanID       string      `json:"spanId"`
	TraceState   string      `json:"traceState,omitempty"`
	ParentSpanID string      `json:"parentSpanId,omitempty"`
	Flags        uint32      `json:"flags,omitempty"`
	Name         string      `json:"name"`
	Kind         int         `json:"kind"`
	Start        string      `json:"startTimeUnixNano"`
	End          string      `json:"endTimeUnixNano"`
	Attributes   []attribute `json:"attributes"`
	Events       []event     `json:"events,omitempty"`
	Status       spanStatus  `json:"status"`
}

type event struct {
	Time string `json:"timeUnixNano"`
	Name string `json:"name"`
}

type spanStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type attribute struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is one attribute value: a string, an int, a double, a bool or
// an array of strings, as kind says.
type anyValue struct {
	kind valueKind
	s    string
	i    int64
	f    float64
	b    bool
	ss   []string
}

type valueKind uint8

const (
	kindString valueKind = iota
	kindInt
	kindDouble
	kindBool
	kindStrings
)

const (
	spanKindServer  = 2
	spanKindClient  = 3
	statusCodeError = 2
	// flagsHasRemote marks SpanFlags' "is the parent remote" bits known
	// (0x100) and set (0x200) for a span whose parent came in a traceparent.
	flagsHasRemote = 0x100
	flagsRemote    = 0x200
	// scopeName names the instrumentation: the gateway.
	scopeName = "gpuworks.gateway"
)

func str(v string) anyValue      { return anyValue{kind: kindString, s: v} }
func integer(v int64) anyValue   { return anyValue{kind: kindInt, i: v} }
func double(v float64) anyValue  { return anyValue{kind: kindDouble, f: v} }
func boolean(v bool) anyValue    { return anyValue{kind: kindBool, b: v} }
func strs(vs ...string) anyValue { return anyValue{kind: kindStrings, ss: vs} }

func nanos(ms int64) string { return strconv.FormatInt(ms*1_000_000, 10) }

// operations maps a gateway path to the GenAI operation it is
// (gen_ai.operation.name); a path not listed has none.
var operations = map[string]string{
	"/v1/chat/completions": "chat",
	"/v1/completions":      "text_completion",
	"/v1/embeddings":       "embeddings",
}

// resultErrors are the results a span marks as an error: what failed on the
// platform's side. A client that left or a reply cut at max_tokens is not an
// error of the service.
var resultErrors = map[uxrecord.Result]bool{uxrecord.ResultError: true, uxrecord.ResultInterrupted: true}

// engineSegments are the request view's segments the engine reports itself;
// the span carries them under vLLM's names (gen_ai.latency.*).
var engineSegments = map[uxrecord.SegmentKind]bool{uxrecord.SegmentQueue: true, uxrecord.SegmentPrefill: true}

// spanOf is a finished request's span. Every value comes from the record;
// a moment that did not happen and a count the engine did not report are
// left out.
func spanOf(r uxrecord.Request) span {
	op := operations[r.Path]
	name := op
	if name == "" {
		name = r.Path
	}
	if r.Model != "" {
		name += " " + r.Model
	}
	end := max(r.EndMS, 0)
	s := span{
		TraceID: r.Trace.Trace.String(), SpanID: r.Trace.ID.String(), TraceState: r.Trace.State,
		Name: name, Kind: spanKindServer,
		Start: nanos(r.ReceivedMS), End: nanos(r.ReceivedMS + int64(end)),
	}
	if !r.Trace.Parent.IsZero() {
		s.ParentSpanID = r.Trace.Parent.String()
		s.Flags = flagsHasRemote | flagsRemote
	} else {
		s.Flags = flagsHasRemote
	}
	s.Flags |= uint32(r.Trace.Flags & tracecontext.FlagSampled)

	s.Attributes = make([]attribute, 0, 40)
	add := func(key string, v anyValue) { s.Attributes = append(s.Attributes, attribute{key, v}) }
	if op != "" {
		add("gen_ai.operation.name", str(op))
	}
	if r.Model != "" {
		add("gen_ai.request.model", str(r.Model))
	}
	// What the client asked for (the engine's defaults fill in the rest).
	if sp := r.Sampling; sp.HasMaxTokens {
		add("gen_ai.request.max_tokens", integer(int64(sp.MaxTokens)))
	}
	if sp := r.Sampling; sp.HasTemperature {
		add("gen_ai.request.temperature", double(sp.Temperature))
	}
	if sp := r.Sampling; sp.HasTopP {
		add("gen_ai.request.top_p", double(sp.TopP))
	}
	if sp := r.Sampling; sp.HasN {
		add("gen_ai.request.choice.count", integer(int64(sp.N)))
	}
	add("gen_ai.request.stream", boolean(r.Stream))
	if r.Usage {
		add("gen_ai.usage.input_tokens", integer(int64(r.PromptTokens)))
		add("gen_ai.usage.output_tokens", integer(int64(r.CompletionTokens)))
		if r.CachedTokens >= 0 {
			add("gen_ai.usage.cache_read.input_tokens", integer(int64(r.CachedTokens)))
		}
		if r.ReasoningTokens >= 0 {
			add("gen_ai.usage.reasoning.output_tokens", integer(int64(r.ReasoningTokens)))
		}
	}
	if r.FinishReason != "" {
		add("gen_ai.response.finish_reasons", strs(r.FinishReason))
	}
	if first := r.FirstTokenMS(); r.Stream && first >= 0 {
		add("gen_ai.response.time_to_first_chunk", double(float64(first)/1000))
	}
	add("url.path", str(r.Path))
	if r.Status > 0 {
		add("http.response.status_code", integer(int64(r.Status)))
	}
	result := uxrecord.ResultOf(r)
	if resultErrors[result] {
		errType := strconv.Itoa(r.Status)
		if r.Outcome == uxrecord.OutcomeUpstreamError || r.Outcome == uxrecord.OutcomeTruncated || r.Status <= 0 {
			errType = r.Outcome.String()
		}
		add("error.type", str(errType))
		s.Status = spanStatus{Code: statusCodeError, Message: string(result)}
	}
	if pd := r.PD; pd != nil {
		if pd.PrefillInstance != "" {
			add("gpuworks.pd.prefill_instance", str(pd.PrefillInstance))
		}
		if pd.Fallback != "" {
			add("gpuworks.pd.fallback", str(pd.Fallback))
		}
	}
	add("gpuworks.request.id", str(r.ID))
	add("gpuworks.request.source", str(r.Source.String()))
	add("gpuworks.request.result", str(string(result)))
	add("gpuworks.request.body_bytes", integer(r.BodyBytes))
	if r.Images > 0 {
		add("gpuworks.request.images", integer(int64(r.Images)))
	}
	add("gpuworks.deployment.id", integer(int64(r.DeploymentID)))
	if r.InstanceUUID != "" {
		add("gpuworks.instance.uuid", str(r.InstanceUUID))
	}
	if r.APIKeyID != 0 {
		add("gpuworks.api_key.id", integer(int64(r.APIKeyID)))
	}
	if r.UserID != 0 {
		add("gpuworks.user.id", integer(int64(r.UserID)))
	}
	if r.Events > 0 {
		add("gpuworks.stream.events", integer(int64(r.Events)))
		if r.MaxGapMS >= 0 {
			add("gpuworks.stream.max_gap_ms", integer(int64(r.MaxGapMS)))
		}
	}
	// Durations vLLM names on its own span (vllm/tracing SpanAttributes), in
	// its unit, seconds: the request's end to end, and what the engine
	// reported of its queue, prefill and decode, as reported.
	add("gen_ai.latency.e2e", double(float64(end)/1000))
	if r.EngineQueueMS >= 0 {
		add("gen_ai.latency.time_in_queue", double(float64(r.EngineQueueMS)/1000))
	}
	if r.EnginePrefillMS >= 0 {
		add("gen_ai.latency.time_in_model_prefill", double(float64(r.EnginePrefillMS)/1000))
	}
	if r.EngineDecodeMS >= 0 {
		add("gen_ai.latency.time_in_model_decode", double(float64(r.EngineDecodeMS)/1000))
	}
	if r.EnginePrefillMS >= 0 && r.EngineDecodeMS >= 0 {
		add("gen_ai.latency.time_in_model_inference", double(float64(r.EnginePrefillMS+r.EngineDecodeMS)/1000))
	}
	// The gateway's side of the phases the request view draws
	// (uxrecord.SegmentsOf, the one place the split is made), in
	// milliseconds. The engine's queue and prefill are the gen_ai.latency
	// values above.
	for _, seg := range uxrecord.SegmentsOf(r) {
		if seg.MS > 0 && !engineSegments[seg.Kind] {
			add("gpuworks.phase."+string(seg.Kind)+"_ms", double(math.Round(seg.MS*1000)/1000))
		}
	}
	for _, ev := range []struct {
		name string
		ms   int32
	}{
		{"gpuworks.dispatch", r.DispatchMS},
		{"gpuworks.first_byte", r.FirstByteMS},
		{"gpuworks.first_reasoning", r.FirstThinkMS},
		{"gpuworks.first_answer", r.FirstAnswerMS},
		{"gpuworks.last_chunk", r.LastChunkMS},
	} {
		if ev.ms >= 0 {
			s.Events = append(s.Events, event{Time: nanos(r.ReceivedMS + int64(ev.ms)), Name: ev.name})
		}
	}
	return s
}

// resourceAttributes describe the control plane.
func (res Resource) attributes() []attribute {
	attrs := []attribute{{"service.name", str("gpuworks")}}
	if res.ServiceVersion != "" {
		attrs = append(attrs, attribute{"service.version", str(res.ServiceVersion)})
	}
	if res.InstanceID != "" {
		attrs = append(attrs, attribute{"service.instance.id", str(res.InstanceID)})
	}
	if res.Name != "" {
		attrs = append(attrs, attribute{"gpuworks.control_plane.name", str(res.Name)})
	}
	return attrs
}

// encode is one export request holding spans, as OTLP/JSON. It is written
// by hand rather than through encoding/json: it runs for every exported
// request, and the reflection walk over these small nested values cost
// several times the bytes it produced.
func encode(res Resource, spans []span) ([]byte, error) {
	b := make([]byte, 0, 512+len(spans)*3200) // a span of a streamed chat is about 3 KiB
	b = append(b, `{"resourceSpans":[{"resource":{"attributes":`...)
	b = appendAttributes(b, res.attributes())
	b = append(b, `},"scopeSpans":[{"scope":{"name":`...)
	b = appendString(b, scopeName)
	if res.ServiceVersion != "" {
		b = append(b, `,"version":`...)
		b = appendString(b, res.ServiceVersion)
	}
	b = append(b, `},"spans":[`...)
	for i := range spans {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendSpan(b, &spans[i])
	}
	return append(b, `]}]}]}`...), nil
}

func appendSpan(b []byte, s *span) []byte {
	b = append(b, `{"traceId":"`...)
	b = append(b, s.TraceID...)
	b = append(b, `","spanId":"`...)
	b = append(b, s.SpanID...)
	b = append(b, '"')
	if s.TraceState != "" {
		b = append(b, `,"traceState":`...)
		b = appendString(b, s.TraceState)
	}
	if s.ParentSpanID != "" {
		b = append(b, `,"parentSpanId":"`...)
		b = append(b, s.ParentSpanID...)
		b = append(b, '"')
	}
	if s.Flags != 0 {
		b = append(b, `,"flags":`...)
		b = strconv.AppendUint(b, uint64(s.Flags), 10)
	}
	b = append(b, `,"name":`...)
	b = appendString(b, s.Name)
	b = append(b, `,"kind":`...)
	b = strconv.AppendInt(b, int64(s.Kind), 10)
	b = append(b, `,"startTimeUnixNano":"`...)
	b = append(b, s.Start...)
	b = append(b, `","endTimeUnixNano":"`...)
	b = append(b, s.End...)
	b = append(b, `","attributes":`...)
	b = appendAttributes(b, s.Attributes)
	if len(s.Events) > 0 {
		b = append(b, `,"events":[`...)
		for i, ev := range s.Events {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"timeUnixNano":"`...)
			b = append(b, ev.Time...)
			b = append(b, `","name":`...)
			b = appendString(b, ev.Name)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	b = append(b, `,"status":{`...)
	if s.Status.Code != 0 {
		b = append(b, `"code":`...)
		b = strconv.AppendInt(b, int64(s.Status.Code), 10)
		if s.Status.Message != "" {
			b = append(b, `,"message":`...)
			b = appendString(b, s.Status.Message)
		}
	}
	return append(b, `}}`...)
}

func appendAttributes(b []byte, attrs []attribute) []byte {
	b = append(b, '[')
	for i, a := range attrs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"key":`...)
		b = appendString(b, a.Key)
		b = append(b, `,"value":`...)
		b = appendValue(b, a.Value)
		b = append(b, '}')
	}
	return append(b, ']')
}

func appendValue(b []byte, v anyValue) []byte {
	switch v.kind {
	case kindString:
		b = append(b, `{"stringValue":`...)
		b = appendString(b, v.s)
	case kindInt:
		// OTLP/JSON writes 64-bit integers as decimal strings.
		b = append(b, `{"intValue":"`...)
		b = strconv.AppendInt(b, v.i, 10)
		b = append(b, '"')
	case kindDouble:
		b = append(b, `{"doubleValue":`...)
		b = appendFloat(b, v.f)
	case kindBool:
		b = append(b, `{"boolValue":`...)
		b = strconv.AppendBool(b, v.b)
	case kindStrings:
		b = append(b, `{"arrayValue":{"values":[`...)
		for i, e := range v.ss {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"stringValue":`...)
			b = appendString(b, e)
			b = append(b, '}')
		}
		b = append(b, `]}`...)
	}
	return append(b, '}')
}

// appendFloat writes a finite double as encoding/json does; a NaN or an
// infinity (never produced from a record) becomes 0 rather than invalid JSON.
func appendFloat(b []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, '0')
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	return strconv.AppendFloat(b, f, format, -1, 64)
}

// appendString writes s as a JSON string. Request ids, model names and
// tracestate come from clients, so everything JSON requires is escaped.
func appendString(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			raw, _ := json.Marshal(s)
			return append(b, raw...)
		}
	}
	b = append(b, '"')
	b = append(b, s...)
	return append(b, '"')
}

// pdPrefillSpanOf is a PD request's prefill stage as the gateway span's
// CLIENT child, when the gateway waited for the stage (sequential handoff):
// the prefill copy carried this span as its parent, so the prefill engine's
// own span nests under it. Its queue and prefill are the prefill instance's
// own account, under vLLM's names.
func pdPrefillSpanOf(r uxrecord.Request) (span, bool) {
	pd := r.PD
	if pd == nil || pd.PrefillMS < 0 || pd.SpanID.IsZero() {
		return span{}, false
	}
	start := r.ReceivedMS + int64(max(pd.StartMS, 0))
	s := span{
		TraceID: r.Trace.Trace.String(), SpanID: pd.SpanID.String(), ParentSpanID: r.Trace.ID.String(),
		TraceState: r.Trace.State, Flags: flagsHasRemote | uint32(r.Trace.Flags&tracecontext.FlagSampled),
		Name: "pd.prefill", Kind: spanKindClient,
		Start: nanos(start), End: nanos(start + int64(pd.PrefillMS)),
		Attributes: make([]attribute, 0, 5),
	}
	add := func(key string, v anyValue) { s.Attributes = append(s.Attributes, attribute{key, v}) }
	if pd.PrefillInstance != "" {
		add("gpuworks.pd.prefill_instance", str(pd.PrefillInstance))
	}
	if pd.PrefillQueueMS >= 0 {
		add("gen_ai.latency.time_in_queue", double(float64(pd.PrefillQueueMS)/1000))
	}
	if pd.PrefillComputeMS >= 0 {
		add("gen_ai.latency.time_in_model_prefill", double(float64(pd.PrefillComputeMS)/1000))
	}
	if pd.Fallback != "" {
		add("gpuworks.pd.fallback", str(pd.Fallback))
	}
	return s, true
}

// encodeRecords is encode over request records: each record's span, and a
// PD request's prefill stage span. It returns how many spans it wrote.
func encodeRecords(res Resource, records []uxrecord.Request) ([]byte, int, error) {
	spans := make([]span, 0, len(records))
	for _, r := range records {
		spans = append(spans, spanOf(r))
		if child, ok := pdPrefillSpanOf(r); ok {
			spans = append(spans, child)
		}
	}
	body, err := encode(res, spans)
	return body, len(spans), err
}

// trimBody keeps an error reply short enough for a status line.
func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
