package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
	benchsearch "gpuworks.ai/gpuworks/pkg/bench/search"
)

type Run struct {
	Error         string
	Warmup        []loadgen.DetailRecord
	ID            uint
	Name          string
	EvaluationURL string
	State         string
	Mode          string
	Model         string
	TokenizerRef  string
	// PriorityClass is the request priority class the run asked the gateway
	// for, the configuration; each request's detail says the class it ran at.
	PriorityClass  string
	Dataset        json.RawMessage
	Search         json.RawMessage
	TargetSnapshot json.RawMessage
	Verdict        json.RawMessage
	KeepDetail     bool
	DetailCapMB    int
	Warnings       []string
	CreatedAt      time.Time
	StartedAt      *time.Time
	EndedAt        *time.Time
	Points         []Point
}

type Point struct {
	State                                                    string
	TargetISL, TargetOSL, Repeat                             int
	InputTokens, OutputTokens                                int64
	AvgTTFT, AvgTPOT, AvgLatency                             float64
	StartedAt, EndedAt                                       *time.Time
	Details                                                  []loadgen.DetailRecord
	DetailStatus, DetailNote                                 string
	Seq, Concurrency, Success, Errors                        int
	RequestRate                                              float64
	ActualISL, ActualOSL, OutputTput, TotalTput, RequestTput float64
	P50TTFT, P99TTFT, P50TPOT, P99TPOT                       float64
	P50Latency, P99Latency                                   float64
	LongTail                                                 json.RawMessage
}

type Document struct {
	Body        []byte
	ContentType string
	Filename    string
}

type datasetConfig struct {
	Kind                  string  `json:"kind"`
	InputTokens           int     `json:"input_tokens"`
	MeanInputTokens       int     `json:"mean_input_tokens"`
	OutputTokens          int     `json:"output_tokens"`
	Turns                 int     `json:"turns"`
	PeakInputTokens       int     `json:"peak_input_tokens"`
	RequiredContextTokens int     `json:"required_context_tokens"`
	DesignReuse           float64 `json:"design_prefix_reuse_rate"`
	Fingerprint           string  `json:"workload_fingerprint"`
	Profile               *struct {
		SampleCount     int `json:"sample_count"`
		MaxInputTokens  int `json:"max_input_tokens"`
		MaxOutputTokens int `json:"max_output_tokens"`
	} `json:"profile"`
}

type searchConfig = benchsearch.Document

type pointRow struct {
	Point
	Sessions, Requests int
	PeakISL            float64
	Interactivity      float64
	DesignReuse        float64
	Agentic            bool
	CacheHit           *float64
	CacheCoverage      *float64
	SuccessRate        float64
	Evidence           *metrics.PointEvidence
	ContextJSON        string
}

var unsafeFilename = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
var sensitiveEnv = regexp.MustCompile(`(?i)(secret|password|passwd|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|authorization|(?:^|[_-])(?:key|auth)(?:$|[_-]))`)

func Render(format string, run Run) (Document, error) {
	run.Points = sanitizedDetailPoints(run.Points)
	run.Error = loadgen.ResponseErrorText(run.Error)
	run.Warmup = safeWarmup(run.Warmup)
	run.TargetSnapshot = sanitizedSnapshot(run.TargetSnapshot)
	datasetConfig, search, target := decodeConfig(run)
	run.Warnings = effectiveWarnings(run, datasetConfig)
	run.Warnings = append(run.Warnings, comparisonWarnings(run, datasetConfig)...)
	rows := rows(run, datasetConfig, search)
	base := filenameBase(run, datasetConfig)
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "md", "markdown":
		return Document{Body: markdown(run, datasetConfig, target, rows), ContentType: "text/markdown; charset=utf-8", Filename: base + ".md"}, nil
	case "csv":
		body, err := csvReport(run, rows)
		return Document{Body: body, ContentType: "text/csv; charset=utf-8", Filename: base + ".csv"}, err
	case "html":
		return Document{Body: htmlReport(run, datasetConfig, target, rows), ContentType: "text/html; charset=utf-8", Filename: base + ".html"}, nil
	default:
		return Document{}, errors.New("report format must be markdown, csv, or html")
	}
}

func effectiveWarnings(run Run, config datasetConfig) []string {
	const agenticDrift = "BENCHMARK_AGENTIC_LENGTH_DRIFT"
	observed, exceeded := false, false
	for _, point := range run.Points {
		if point.ActualISL <= 0 {
			continue
		}
		observed = true
		exceeded = exceeded || dataset.AgenticLengthDriftExceeded(point.ActualISL, config.MeanInputTokens)
	}
	if config.Kind != "agentic" || !observed || exceeded {
		return run.Warnings
	}
	out := make([]string, 0, len(run.Warnings))
	for _, warning := range run.Warnings {
		if warning != agenticDrift {
			out = append(out, warning)
		}
	}
	return out
}

func decodeConfig(run Run) (datasetConfig, searchConfig, targetSnapshot) {
	var dataset datasetConfig
	var search searchConfig
	var target targetSnapshot
	_ = json.Unmarshal(run.Dataset, &dataset)
	search, _ = benchsearch.ParseDocument(run.Search)
	_ = json.Unmarshal(run.TargetSnapshot, &target)
	return dataset, search, target
}

func rows(run Run, dataset datasetConfig, search searchConfig) []pointRow {
	out := make([]pointRow, 0, len(run.Points))
	for _, point := range run.Points {
		requests := point.Success + point.Errors
		sessions := search.SessionsPerPoint[strconv.Itoa(point.Concurrency)]
		if sessions == 0 && isConversationDataset(dataset.Kind) {
			multiplier := search.RequestsPerPointMultiplier
			if multiplier <= 0 {
				multiplier = 10
			}
			sessions = int(math.Ceil(float64(maxInt(point.Concurrency, 1)) * multiplier))
		}
		var tail struct {
			KVHitRate       *float64 `json:"KVHitRate"`
			CacheCoverage   *float64 `json:"CacheReportCoverage"`
			PeakInputTokens float64  `json:"PeakInputTokens"`
		}
		_ = json.Unmarshal(point.LongTail, &tail)
		row := pointRow{Point: point, Sessions: sessions, Requests: requests, PeakISL: tail.PeakInputTokens, DesignReuse: dataset.DesignReuse, Agentic: dataset.Kind == "agentic", CacheHit: tail.KVHitRate, CacheCoverage: tail.CacheCoverage}
		row.Evidence = pointEvidence(point)
		if row.Evidence != nil {
			row.Sessions = row.Evidence.Plan.PlannedSessions
			row.Success = row.Evidence.All.Success
			row.Errors = row.Evidence.All.Failed + row.Evidence.All.Canceled
			row.Requests = row.Evidence.All.Attempted
			row.CacheHit = cacheRatio(row.Evidence.All.Cache)
			row.CacheCoverage = fraction(row.Evidence.All.Cache.Reported, row.Evidence.All.Attempted)
		}
		row.Interactivity = metrics.Interactivity(point.P50TPOT)
		if row.Evidence != nil {
			row.Interactivity = 0
			if row.Evidence.All.TPOT.Count > 0 {
				row.Interactivity = metrics.Interactivity(row.Evidence.All.TPOT.P50Ms)
			}
		}
		if row.Requests > 0 {
			row.SuccessRate = float64(row.Success) / float64(row.Requests)
		}
		out = append(out, row)
	}
	context := reportContext(run, recommend(run, out), out)
	for i := range out {
		out[i].ContextJSON = context
	}
	return out
}

func filenameBase(run Run, dataset datasetConfig) string {
	name := strings.Trim(unsafeFilename.ReplaceAllString(strings.TrimSpace(run.Name), "-"), "-._")
	if name == "" {
		name = fmt.Sprintf("benchmark-%d", run.ID)
	}
	fingerprint := strings.Trim(unsafeFilename.ReplaceAllString(dataset.Fingerprint, "-"), "-._")
	if fingerprint == "" {
		fingerprint = fmt.Sprintf("run-%d", run.ID)
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name + "-" + fingerprint
}

var columns = append([]string{
	"concurrency", "request_rate_req_s", "sessions", "requests", "actual_mean_isl", "actual_peak_isl", "actual_mean_osl",
	"interactivity_tok_s_user", "total_tok_s", "output_tok_s", "ttft_p50_ms", "ttft_p99_ms",
	"tpot_p50_ms", "tpot_p99_ms", "e2el_p50_ms", "e2el_p99_ms", "designed_prefix_reuse_rate", "observed_cache_hit_rate",
	"cache_report_coverage", "success_rate",
}, evidenceColumns...)

// metricDefinitions explain the derived columns; markdown and HTML render the
// same list.
var metricDefinitions = []string{
	"`interactivity_tok_s_user = 1000 / tpot_p50_ms`: the per-user output speed once decoding has started, excluding TTFT (InferenceMAX interactivity).",
	"`output_tok_s` and `total_tok_s` divide all generated (and input) tokens by the point's wall time from the first request sent to the last request finished.",
	"`e2el` is the end-to-end request latency, from sending the request to its last generated token.",
	"`request_rate_req_s` is the open-loop arrival rate; for those points `concurrency` is the in-flight cap and is empty when unlimited.",
	"`sessions` is planned sessions and `requests` is actually attempted logical requests; skipped rounds are separate and retries would be additional attempts. Run done means execution ended, not that every session completed.",
	"Run input/context targets and output budgets are planned configuration. Service context limits and achieved fixed output length require independent observation.",
	"Cache fields missing from a response are unknown. For evidence version 1, cache coverage divides reported requests by all attempted requests; legacy coverage divides by successful requests.",
	"Cache ratio sums cached and server input tokens over the same explicitly reported requests; it is not an average of request percentages.",
	"TTFT includes reasoning; first visible content is separate. TPOT is (last output - first output) / (output tokens - 1), excluding failures and responses without an observable decode interval. Chunk intervals are not per-token intervals.",
	"Throughput counts successful-request tokens; failed partial output is reported separately. Its window includes failed attempts, round waits and stream completion.",
	"Empty CSV cells and unknown in narrative sections mean unavailable evidence, never a measured zero. Timing sample counts determine whether a percentile is observable.",
	"CSV evidence_json and report_context_json preserve omitted grouped facts and configuration; request bodies and timeline records are only retained when detail collection is enabled.",
}

func csvReport(run Run, rows []pointRow) ([]byte, error) {
	var out bytes.Buffer
	w := csv.NewWriter(&out)
	if err := w.Write(append(append([]string{}, columns...), "evidence_json", "report_context_json")); err != nil {
		return nil, err
	}
	if len(rows) == 0 && (run.Error != "" || len(run.Warmup) > 0) {
		// Context-only row: empty measured cells are unknown, not a fake point.
		fields := make([]string, len(columns)+2)
		fields[len(fields)-1] = reportContext(run, "", nil)
		if err := w.Write(fields); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		if err := w.Write(append(rowValues(row), evidenceJSON(row), row.ContextJSON)); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return out.Bytes(), w.Error()
}

func rowValues(row pointRow) []string {
	concurrency := strconv.Itoa(row.Concurrency)
	if row.RequestRate > 0 {
		concurrency = optionalInt(row.Concurrency)
	}
	values := []string{
		concurrency, positiveNumber(row.RequestRate), optionalInt(row.Sessions), strconv.Itoa(row.Requests), positiveNumber(row.ActualISL), positiveNumber(row.PeakISL), positiveNumber(row.ActualOSL),
		positiveNumber(row.Interactivity), number(row.TotalTput), number(row.OutputTput), positiveNumber(row.P50TTFT), positiveNumber(row.P99TTFT), positiveNumber(row.P50TPOT), positiveNumber(row.P99TPOT),
		positiveNumber(row.P50Latency), positiveNumber(row.P99Latency),
		optionalDesignReuse(row), optionalPercentage(row.CacheHit), optionalPercentage(row.CacheCoverage), optionalPercentage(fraction(row.Success, row.Requests)),
	}
	if row.Evidence != nil {
		values[4] = tokenMean(row.Evidence.All.Tokens.Input)
		values[5] = tokenMax(row.Evidence.All.Tokens.Input)
		values[6] = tokenMean(row.Evidence.All.Tokens.Output)
		values[7] = positiveNumber(metrics.Interactivity(row.Evidence.All.TPOT.P50Ms))
		values[10] = metricValue(row.Evidence.All.TTFT, false)
		values[11] = metricValue(row.Evidence.All.TTFT, true)
		values[12] = metricValue(row.Evidence.All.TPOT, false)
		values[13] = metricValue(row.Evidence.All.TPOT, true)
		values[14] = metricValue(row.Evidence.All.LastOutputLatency, false)
		values[15] = metricValue(row.Evidence.All.LastOutputLatency, true)
		if row.Evidence.All.TPOT.Count == 0 {
			values[7] = ""
		}
		if tokenAggregatesUnavailable(row) {
			values[8], values[9] = "", ""
		}
	}
	return append(values, evidenceValues(row)...)
}

func tokenAggregatesUnavailable(row pointRow) bool {
	return row.Evidence != nil && row.Evidence.All.Success > 0 &&
		(row.Evidence.All.Tokens.Input.Count == 0 || row.Evidence.All.Tokens.Output.Count == 0)
}

func markdown(run Run, dataset datasetConfig, target targetSnapshot, rows []pointRow) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", markdownCell(run.Name))
	fmt.Fprintf(&out, "> %s\n\n", markdownCell(summary(run, dataset, rows)))
	if run.EvaluationURL != "" {
		fmt.Fprintf(&out, "**Evaluation:** <%s>\n\n", markdownCell(run.EvaluationURL))
	}
	out.WriteString("## Run\n\n")
	markdownProperties(&out, run, dataset, target)
	out.WriteString("\n## Recommendation conditions\n\n| Point | Eligibility |\n|---|---|\n")
	for _, property := range recommendationProperties(run, rows) {
		fmt.Fprintf(&out, "| %s | %s |\n", markdownCell(property[0]), markdownCell(property[1]))
	}
	out.WriteString("\n## Point results\n\n|")
	for _, column := range columns[:20] {
		fmt.Fprintf(&out, " %s |", column)
	}
	out.WriteString("\n|")
	for range columns[:20] {
		out.WriteString("---|")
	}
	out.WriteByte('\n')
	for _, row := range rows {
		out.WriteByte('|')
		for _, value := range rowValues(row)[:20] {
			fmt.Fprintf(&out, " %s |", markdownCell(value))
		}
		out.WriteByte('\n')
	}
	writeWarmupMarkdown(&out, run)
	writeEvidenceMarkdown(&out, rows)
	writeTargetMarkdown(&out, target)
	out.WriteString("\n## Metric definitions\n\n")
	for _, definition := range metricDefinitions {
		fmt.Fprintf(&out, "- %s\n", definition)
	}
	if dataset.Kind == "agentic" {
		out.WriteString("- The designed prefix reuse rate describes the synthetic workload; it is not the observed cache hit rate.\n")
	}
	writeWarningsMarkdown(&out, run.Warnings)
	return []byte(out.String())
}

func markdownProperties(out *strings.Builder, run Run, dataset datasetConfig, target targetSnapshot) {
	out.WriteString("| Field | Value |\n|---|---|\n")
	for _, row := range runProperties(run, dataset, target) {
		fmt.Fprintf(out, "| %s | %s |\n", row[0], markdownCell(orDash(row[1])))
	}
}

func writeTargetMarkdown(out *strings.Builder, target targetSnapshot) {
	properties := targetProperties(target)
	if len(properties) == 0 && len(target.Env) == 0 {
		return
	}
	out.WriteString("\n## Target snapshot\n\n")
	out.WriteString("| Field | Value |\n|---|---|\n")
	for _, property := range properties {
		fmt.Fprintf(out, "| %s | %s |\n", property[0], markdownCell(property[1]))
	}
	if len(target.Env) == 0 {
		return
	}
	out.WriteString("\n### Environment\n\n```text\n")
	for _, pair := range redactedEnv(target.Env) {
		fmt.Fprintf(out, "%s=%s\n", pair[0], pair[1])
	}
	out.WriteString("```\n")
}

func writeWarningsMarkdown(out *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	out.WriteString("\n## Warnings\n\n")
	for _, warning := range warnings {
		fmt.Fprintf(out, "- `%s`\n", markdownCell(warning))
	}
}

func summary(run Run, dataset datasetConfig, rows []pointRow) string {
	parts := []string{fmt.Sprintf("%s benchmark for %s", first(dataset.Kind, "Benchmark"), run.Model)}
	if dataset.Kind == "agentic" && dataset.MeanInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("mean input %d tokens/request", dataset.MeanInputTokens))
	}
	if dataset.Kind == "agentic" && dataset.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns/session", dataset.Turns))
	}
	if dataset.Kind == "agentic" && dataset.DesignReuse > 0 {
		parts = append(parts, fmt.Sprintf("designed prefix reuse %s", percentage(dataset.DesignReuse)))
	}
	if len(rows) > 0 {
		if recommendation := recommend(run, rows); recommendation != "" {
			parts = append(parts, recommendation)
		}
	}
	return strings.Join(parts, "; ") + "."
}

func runProperties(run Run, dataset datasetConfig, target targetSnapshot) [][2]string {
	properties := [][2]string{
		{"State", run.State}, {"Mode", run.Mode}, {"Model", first(target.ServedName, target.Model, run.Model)},
		{"Created", run.CreatedAt.UTC().Format(time.RFC3339)}, {"Workload", dataset.Kind},
	}
	if run.PriorityClass != "" {
		properties = append(properties, [2]string{"Requested priority class", run.PriorityClass})
	}
	if run.Error != "" {
		properties = append(properties, [2]string{"Failure diagnostic", run.Error})
	}
	switch dataset.Kind {
	case "agentic":
		properties = append(properties,
			[2]string{"Mean input", tokenCount(dataset.MeanInputTokens)}, [2]string{"Peak input", tokenCount(dataset.PeakInputTokens)},
			[2]string{"Required context", tokenCount(dataset.RequiredContextTokens)}, [2]string{"Turns per session", optionalInt(dataset.Turns)},
			[2]string{"Output budget per turn", tokenCount(dataset.OutputTokens)}, [2]string{"Designed prefix reuse", percentage(dataset.DesignReuse)},
			[2]string{"Workload fingerprint", dataset.Fingerprint},
		)
	case "random", "":
		properties = append(properties,
			[2]string{"Input length", tokenCount(dataset.InputTokens)}, [2]string{"Output limit", tokenCount(dataset.OutputTokens)},
		)
	case "multiturn":
		turns := dataset.Turns
		if turns <= 0 {
			turns = 3
		}
		properties = append(properties, [2]string{"Corpus", "OASST1 multi-turn"}, [2]string{"Turns per conversation", "up to " + strconv.Itoa(turns)}, [2]string{"Output limit", tokenCount(dataset.OutputTokens)})
	case "imported":
		properties = append(properties, [2]string{"Corpus", "Imported JSONL"})
		if dataset.Profile != nil {
			properties = append(properties,
				[2]string{"Samples", optionalInt(dataset.Profile.SampleCount)},
				[2]string{"Maximum declared input", tokenCount(dataset.Profile.MaxInputTokens)},
				[2]string{"Maximum declared output", tokenCount(dataset.Profile.MaxOutputTokens)},
			)
		}
	case "agenttrace":
		properties = append(properties, [2]string{"Dataset status", "Retired; historical Run only"})
	}
	tokenizer := run.TokenizerRef
	if tokenizer == "" {
		tokenizer = "Estimation mode"
	}
	properties = append(properties, [2]string{"Tokenizer", tokenizer})
	doc, _ := benchsearch.ParseDocument(run.Search)
	properties = append(properties, [2]string{"Objective", first(doc.Objective, "output_tps (default)")}, [2]string{"Experience constraints", constraintDescription(doc)}, [2]string{"Measurement regime", "fixed session count"}, [2]string{"Configured warmup requests", strconv.Itoa(doc.WarmupRequests)}, [2]string{"Configured request timeout", configuredRequestTimeout(doc)}, [2]string{"Detail configured", strconv.FormatBool(run.KeepDetail)}, [2]string{"Repetition evidence", "point repeat IDs retained; no uncertainty estimate"})
	if doc.OpenLoop() {
		for i := range properties {
			if properties[i][0] == "Measurement regime" {
				properties[i][1] = "arrival window " + number(doc.EffectivePointDuration()) + " seconds; all dispatched work drained"
			}
		}
	}
	return properties
}

// pointLabel names a point by its load: C=<concurrency> for closed loop,
// R=<rate> for open loop.
func pointLabel(p Point) string {
	if p.RequestRate > 0 {
		return "R=" + strconv.FormatFloat(p.RequestRate, 'f', -1, 64)
	}
	return "C=" + strconv.Itoa(p.Concurrency)
}

func recommendedLoad(p Point) string {
	if p.RequestRate > 0 {
		return "arrival rate " + strconv.FormatFloat(p.RequestRate, 'f', -1, 64) + " req/s"
	}
	return "concurrency " + strconv.Itoa(p.Concurrency)
}

func redactedEnv(env map[string]string) [][2]string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, key := range keys {
		value := env[key]
		if sensitiveEnv.MatchString(key) {
			value = "[REDACTED]"
		}
		out = append(out, [2]string{key, value})
	}
	return out
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func number(value float64) string     { return strconv.FormatFloat(value, 'f', 3, 64) }
func percentage(value float64) string { return strconv.FormatFloat(value*100, 'f', 2, 64) + "%" }
func optionalPercentage(value *float64) string {
	if value == nil {
		return ""
	}
	return percentage(*value)
}
func optionalDesignReuse(row pointRow) string {
	if !row.Agentic {
		return ""
	}
	return percentage(row.DesignReuse)
}
func positiveNumber(value float64) string {
	if value <= 0 {
		return ""
	}
	return number(value)
}
func optionalInt(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}
func tokenCount(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value) + " tokens"
}
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

func isConversationDataset(kind string) bool {
	return kind == "multiturn" || kind == "imported" || kind == "agenttrace"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// configuredRequestTimeout names the run's stored per-request timeout. A run
// stored without one predates the setting, and what bounded its requests is
// not recorded.
func configuredRequestTimeout(doc benchsearch.Document) string {
	if doc.RequestTimeoutSeconds <= 0 {
		return "not recorded"
	}
	return strconv.Itoa(doc.RequestTimeoutSeconds) + " seconds"
}
