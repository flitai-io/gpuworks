package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

var evidenceColumns = []string{
	"point_seq", "point_state", "point_repeat", "target_isl", "target_osl", "successful_input_tokens", "successful_output_tokens", "execution_started_at", "execution_ended_at", "evidence_version", "planned_requests", "successful_requests", "failed_requests", "canceled_requests", "skipped_requests", "canceled_before_start",
	"completed_sessions", "failed_sessions", "canceled_sessions", "session_completion_rate", "retry_count",
	"output_budget", "output_policy", "actual_peak_osl", "actual_peak_context", "server_mean_isl", "local_mean_isl", "server_mean_osl", "local_mean_osl",
	"ttft_samples", "tpot_samples", "last_output_latency_samples", "stream_completion_samples", "stream_completion_p50_ms", "stream_completion_p99_ms",
	"cache_reported_requests", "cache_unknown_requests", "cache_ratio_requests", "cache_input_tokens", "cached_tokens", "cache_coverage_scope",
	"measurement_start", "measurement_end", "measurement_seconds", "completed_sessions_per_minute", "session_latency_samples", "session_latency_p50_ms", "session_latency_p99_ms",
	"first_turn_ttft_samples", "first_turn_ttft_p50_ms", "first_turn_ttft_p99_ms", "first_turn_tpot_samples", "first_turn_tpot_p50_ms", "first_turn_tpot_p99_ms", "first_turn_completion_p50_ms", "first_turn_cache_reported", "first_turn_cache_unknown", "first_turn_cache_ratio",
	"followup_ttft_samples", "followup_ttft_p50_ms", "followup_ttft_p99_ms", "followup_tpot_samples", "followup_tpot_p50_ms", "followup_tpot_p99_ms", "followup_completion_p50_ms", "followup_cache_reported", "followup_cache_unknown", "followup_cache_ratio",
	"effective_seed", "seed_derivation", "cache_policy", "cache_state", "retry_policy", "detail_status", "retained_detail_records",
}

func pointEvidence(point Point) *metrics.PointEvidence {
	var document struct {
		Evidence *metrics.PointEvidence `json:"Evidence"`
	}
	if json.Unmarshal(point.LongTail, &document) != nil || document.Evidence == nil || document.Evidence.Version != 1 {
		return nil
	}
	return document.Evidence
}

func fraction(numerator, denominator int) *float64 {
	if denominator <= 0 {
		return nil
	}
	ratio := float64(numerator) / float64(denominator)
	return &ratio
}

func cacheRatio(cache metrics.CacheEvidence) *float64 {
	if cache.RatioRequests <= 0 || cache.InputTokens <= 0 {
		return nil
	}
	ratio := float64(cache.CachedTokens) / float64(cache.InputTokens)
	return &ratio
}

func metricValue(summary metrics.MetricSummary, p99 bool) string {
	if summary.Count == 0 {
		return ""
	}
	if p99 {
		return number(summary.P99Ms)
	}
	return number(summary.P50Ms)
}

func tokenMean(distribution metrics.TokenDistribution) string {
	if distribution.Count == 0 {
		return ""
	}
	return number(float64(distribution.Total) / float64(distribution.Count))
}

func tokenMax(distribution metrics.TokenDistribution) string {
	if distribution.Count == 0 {
		return ""
	}
	return strconv.Itoa(distribution.Max)
}

func timeValue(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func measurementSeconds(e *metrics.PointEvidence) float64 {
	if e == nil || e.Timing.StartedAt == nil || e.Timing.FinishedAt == nil {
		return 0
	}
	return e.Timing.FinishedAt.Sub(*e.Timing.StartedAt).Seconds()
}

func detailStatus(point Point) string {
	if point.DetailStatus != "" {
		return point.DetailStatus
	}
	return "unavailable"
}

func evidenceValues(row pointRow) []string {
	values := map[string]string{"point_seq": strconv.Itoa(row.Seq), "cache_coverage_scope": "successful_requests (legacy)", "detail_status": detailStatus(row.Point), "retained_detail_records": strconv.Itoa(len(row.Details)), "point_state": row.State, "point_repeat": strconv.Itoa(row.Repeat), "target_isl": optionalInt(row.TargetISL), "target_osl": optionalInt(row.TargetOSL), "successful_input_tokens": strconv.FormatInt(row.InputTokens, 10), "successful_output_tokens": strconv.FormatInt(row.OutputTokens, 10), "execution_started_at": timeValue(row.StartedAt), "execution_ended_at": timeValue(row.EndedAt)}
	if row.CacheCoverage == nil {
		values["cache_coverage_scope"] = "unknown"
	}
	e := row.Evidence
	if e != nil {
		if e.All.Success > 0 && e.All.Tokens.Input.Count == 0 {
			values["successful_input_tokens"] = ""
		}
		if e.All.Success > 0 && e.All.Tokens.Output.Count == 0 {
			values["successful_output_tokens"] = ""
		}
		ints := map[string]int{
			"evidence_version": e.Version, "planned_requests": e.Plan.PlannedRequests, "successful_requests": e.All.Success, "failed_requests": e.All.Failed,
			"canceled_requests": e.All.Canceled, "skipped_requests": e.Outcomes.Skipped, "canceled_before_start": e.Outcomes.CanceledBeforeStart,
			"completed_sessions": e.Outcomes.CompletedSessions, "failed_sessions": e.Outcomes.FailedSessions, "canceled_sessions": e.Outcomes.CanceledSessions,
			"retry_count": e.Outcomes.RetryCount, "output_budget": e.Plan.OutputBudget,
			"ttft_samples": e.All.TTFT.Count, "tpot_samples": e.All.TPOT.Count, "last_output_latency_samples": e.All.LastOutputLatency.Count, "stream_completion_samples": e.All.Completion.Count,
			"cache_reported_requests": e.All.Cache.Reported, "cache_unknown_requests": e.All.Cache.Unknown, "cache_ratio_requests": e.All.Cache.RatioRequests, "session_latency_samples": e.Timing.CompletedSessionLatency.Count,
		}
		for key, value := range ints {
			values[key] = strconv.Itoa(value)
		}
		values["session_completion_rate"] = optionalPercentage(fraction(e.Outcomes.CompletedSessions, e.Plan.PlannedSessions))
		values["output_policy"] = e.Plan.OutputPolicy
		values["actual_peak_osl"], values["actual_peak_context"] = tokenMax(e.All.Tokens.Output), tokenMax(e.All.Tokens.Context)
		values["server_mean_isl"], values["local_mean_isl"] = tokenMean(e.All.Tokens.ServerInput), tokenMean(e.All.Tokens.LocalInput)
		values["server_mean_osl"], values["local_mean_osl"] = tokenMean(e.All.Tokens.ServerOutput), tokenMean(e.All.Tokens.LocalOutput)
		values["stream_completion_p50_ms"], values["stream_completion_p99_ms"] = metricValue(e.All.Completion, false), metricValue(e.All.Completion, true)
		values["cache_input_tokens"], values["cached_tokens"] = strconv.FormatInt(e.All.Cache.InputTokens, 10), strconv.FormatInt(e.All.Cache.CachedTokens, 10)
		values["cache_coverage_scope"] = "all_attempted_requests"
		values["measurement_start"], values["measurement_end"] = timeValue(e.Timing.StartedAt), timeValue(e.Timing.FinishedAt)
		if seconds := measurementSeconds(e); seconds > 0 {
			values["measurement_seconds"] = number(seconds)
			values["completed_sessions_per_minute"] = number(float64(e.Outcomes.CompletedSessions) * 60 / seconds)
		}
		values["session_latency_p50_ms"], values["session_latency_p99_ms"] = metricValue(e.Timing.CompletedSessionLatency, false), metricValue(e.Timing.CompletedSessionLatency, true)
		for prefix, group := range map[string]metrics.RequestEvidence{"first_turn": e.FirstTurn, "followup": e.FollowupTurns} {
			values[prefix+"_ttft_samples"], values[prefix+"_tpot_samples"] = strconv.Itoa(group.TTFT.Count), strconv.Itoa(group.TPOT.Count)
			values[prefix+"_ttft_p50_ms"], values[prefix+"_ttft_p99_ms"] = metricValue(group.TTFT, false), metricValue(group.TTFT, true)
			values[prefix+"_tpot_p50_ms"], values[prefix+"_tpot_p99_ms"] = metricValue(group.TPOT, false), metricValue(group.TPOT, true)
			values[prefix+"_completion_p50_ms"] = metricValue(group.Completion, false)
			values[prefix+"_cache_reported"], values[prefix+"_cache_unknown"] = strconv.Itoa(group.Cache.Reported), strconv.Itoa(group.Cache.Unknown)
			values[prefix+"_cache_ratio"] = optionalPercentage(cacheRatio(group.Cache))
		}
		values["effective_seed"] = strconv.FormatInt(e.Execution.Seed, 10)
		values["seed_derivation"], values["cache_policy"], values["cache_state"], values["retry_policy"] = e.Execution.SeedDerivation, e.Execution.CachePolicy, e.Execution.CacheState, e.Execution.RetryPolicy
	}
	out := make([]string, 0, len(evidenceColumns))
	for _, column := range evidenceColumns {
		out = append(out, values[column])
	}
	return out
}

func jsonValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Optional database JSON may be an empty, non-nil slice. Marshal it as null
// rather than letting it invalidate the entire report context. Nonempty JSON is
// retained unchanged; malformed data must not be replaced with invented facts.
func optionalContextJSON(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return raw
}

func reportContext(run Run, recommendation string, rows []pointRow) string {
	var target map[string]any
	_ = json.Unmarshal(run.TargetSnapshot, &target)
	if env, ok := target["env"].(map[string]any); ok {
		for key := range env {
			if sensitiveEnv.MatchString(key) {
				env[key] = "[REDACTED]"
			}
		}
	}
	return jsonValue(map[string]any{"run_id": run.ID, "state": run.State, "failure_diagnostic": run.Error, "warmup_attempts": run.Warmup, "run_name": run.Name, "model": run.Model, "mode": run.Mode, "created_at": run.CreatedAt, "started_at": run.StartedAt, "ended_at": run.EndedAt, "detail_configured": run.KeepDetail, "detail_cap_mb": run.DetailCapMB, "dataset": datasetContext(run.Dataset), "search": optionalContextJSON(run.Search), "tokenizer": run.TokenizerRef, "priority_class": run.PriorityClass, "target_snapshot": target, "warnings": run.Warnings,
		"point_eligibility": recommendationProperties(run, rows), "recommendation": recommendation, "persisted_verdict": optionalContextJSON(run.Verdict), "recommendation_scope": "best measured point in this run; no capacity upper bound", "repeats": "see point repeat; no uncertainty estimate"})
}

func evidenceJSON(row pointRow) string {
	if row.Evidence == nil {
		return ""
	}
	return jsonValue(row.Evidence)
}

func evidenceGroups(e *metrics.PointEvidence) []struct {
	Name  string
	Group metrics.RequestEvidence
} {
	out := []struct {
		Name  string
		Group metrics.RequestEvidence
	}{{"all", e.All}, {"first_turn", e.FirstTurn}, {"followup_turns", e.FollowupTurns}}
	turns := make([]int, 0, len(e.ByTurn))
	for turn := range e.ByTurn {
		turns = append(turns, turn)
	}
	sort.Ints(turns)
	for _, turn := range turns {
		out = append(out, struct {
			Name  string
			Group metrics.RequestEvidence
		}{fmt.Sprintf("turn_%d", turn), e.ByTurn[turn]})
	}
	return out
}

func groupProperties(group metrics.RequestEvidence) [][2]string {
	out := [][2]string{{"Attempted / successful / failed / canceled", fmt.Sprintf("%d / %d / %d / %d", group.Attempted, group.Success, group.Failed, group.Canceled)}}
	for _, metric := range []struct {
		Name    string
		Summary metrics.MetricSummary
	}{{"TTFT", group.TTFT}, {"First content", group.FirstContent}, {"TPOT", group.TPOT}, {"Last output latency", group.LastOutputLatency}, {"Stream completion", group.Completion}} {
		value := "unknown (n=0)"
		if metric.Summary.Count > 0 {
			value = fmt.Sprintf("n=%d; mean=%s; P50=%s; P99=%s ms", metric.Summary.Count, number(metric.Summary.MeanMs), metricValue(metric.Summary, false), metricValue(metric.Summary, true))
		}
		out = append(out, [2]string{metric.Name, value})
	}
	out = append(out, [2]string{"Successful with output but no answer text", strconv.Itoa(group.NoContent)})
	for _, token := range []struct {
		Name         string
		Distribution metrics.TokenDistribution
	}{{"Selected input", group.Tokens.Input}, {"Selected output", group.Tokens.Output}, {"Same-request context", group.Tokens.Context}, {"Server input", group.Tokens.ServerInput}, {"Server output", group.Tokens.ServerOutput}, {"Local input estimate", group.Tokens.LocalInput}, {"Local output estimate", group.Tokens.LocalOutput}, {"Reasoning usage", group.Tokens.Reasoning}, {"Failed partial output", group.Tokens.FailedOutput}} {
		value := "unknown (n=0)"
		if d := token.Distribution; d.Count > 0 {
			value = fmt.Sprintf("n=%d; total=%d; mean=%s; min=%d; max=%d; P50=%s; P99=%s tokens", d.Count, d.Total, tokenMean(d), d.Min, d.Max, number(d.P50), number(d.P99))
		}
		out = append(out, [2]string{token.Name, value})
	}
	out = append(out, [2]string{"Input token sources", jsonValue(group.Tokens.InputSources)}, [2]string{"Output token sources", jsonValue(group.Tokens.OutputSources)},
		[2]string{"Cache reporting", fmt.Sprintf("reported=%d / attempted=%d; unknown=%d; coverage=%s", group.Cache.Reported, group.Attempted, group.Cache.Unknown, unknown(optionalPercentage(fraction(group.Cache.Reported, group.Attempted))))},
		[2]string{"Cache ratio", fmt.Sprintf("%s; cached=%d / input=%d; ratio n=%d; source=server usage", unknown(optionalPercentage(cacheRatio(group.Cache))), group.Cache.CachedTokens, group.Cache.InputTokens, group.Cache.RatioRequests)},
		[2]string{"Finish reasons", jsonValue(group.FinishReasons)}, [2]string{"Error stages", jsonValue(group.ErrorStages)})
	return out
}

func unknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func writeEvidenceMarkdown(out *strings.Builder, rows []pointRow) {
	out.WriteString("\n## Request and session evidence\n\n")
	for _, row := range rows {
		fmt.Fprintf(out, "### Point %d (%s)\n\n", row.Seq, pointLabel(row.Point))
		if row.Evidence == nil {
			out.WriteString("Evidence version 1 unavailable: session outcomes, round groups, effective policies and sample counts are unknown. Historical values are not backfilled.\n\n")
		} else {
			out.WriteString("| Point evidence | Value |\n|---|---|\n")
			for _, property := range evidenceSummary(row) {
				fmt.Fprintf(out, "| %s | %s |\n", property[0], markdownCell(property[1]))
			}
			out.WriteByte('\n')
			out.WriteString("| Execution evidence | Value |\n|---|---|\n")
			for _, property := range executionProperties(row) {
				fmt.Fprintf(out, "| %s | %s |\n", property[0], markdownCell(property[1]))
			}
			out.WriteByte('\n')
			for _, group := range evidenceGroups(row.Evidence) {
				fmt.Fprintf(out, "#### %s\n\n| Metric | Value |\n|---|---|\n", group.Name)
				for _, property := range groupProperties(group.Group) {
					fmt.Fprintf(out, "| %s | %s |\n", property[0], markdownCell(property[1]))
				}
				out.WriteByte('\n')
			}
			out.WriteString("#### Sessions\n\n| Session | State | Planned turns | Attempted | Successful | Failed | Canceled | Skipped | Started | Finished |\n|---|---|---|---|---|---|---|---|---|---|\n")
			for _, session := range row.Evidence.Sessions {
				fmt.Fprintf(out, "| %s | %s | %d | %d | %d | %d | %d | %d | %s | %s |\n", markdownCell(session.SessionID), markdownCell(session.State), session.PlannedTurns, session.Attempted, session.Success, session.Failed, session.Canceled, session.Skipped, unknown(timeValue(session.StartedAt)), unknown(timeValue(session.FinishedAt)))
			}
			if row.Evidence.SessionsTruncated {
				out.WriteString("\nSession summaries truncated; aggregate counts remain complete.\n")
			}
		}
		fmt.Fprintf(out, "\nRequest detail: %s; retained records=%d. %s\n\n", detailStatus(row.Point), len(row.Details), markdownCell(row.DetailNote))
		writeDetailMarkdown(out, row.Point)
	}
}

func executionProperties(row pointRow) [][2]string {
	e := row.Evidence
	if e == nil {
		return nil
	}
	return [][2]string{
		{"Plan sessions / logical requests / turns", fmt.Sprintf("%d / %d / %d (zero turns means variable)", e.Plan.PlannedSessions, e.Plan.PlannedRequests, e.Plan.TurnsPerSession)},
		{"Output policy / budget", fmt.Sprintf("%s / %d tokens; requested policy does not prove endpoint compliance", e.Plan.OutputPolicy, e.Plan.OutputBudget)},
		{"Measurement window", unknown(timeValue(e.Timing.StartedAt)) + " → " + unknown(timeValue(e.Timing.FinishedAt))},
		{"Effective seed / derivation", fmt.Sprintf("%d / %s", e.Execution.Seed, unknown(e.Execution.SeedDerivation))},
		{"Cache policy / observed state", unknown(e.Execution.CachePolicy) + " / " + unknown(e.Execution.CacheState)},
		{"Retry policy / count", fmt.Sprintf("%s / %d", unknown(e.Execution.RetryPolicy), e.Outcomes.RetryCount)},
		{"Tokenizer source / local method", unknown(e.Execution.TokenizerSource) + " / " + unknown(e.Execution.LocalTokenCountMethod)},
		{"Configured / successful warmup", fmt.Sprintf("%d / %d", e.Execution.WarmupRequests, e.Execution.WarmupSucceeded)},
		{"Detail policy", unknown(e.Execution.DetailPolicy)},
		{"Successful-request input / output token totals", successfulTokenTotals(row)},
	}
}

func successfulTokenTotals(row pointRow) string {
	input, output := strconv.FormatInt(row.InputTokens, 10), strconv.FormatInt(row.OutputTokens, 10)
	if row.Evidence.All.Success > 0 {
		if row.Evidence.All.Tokens.Input.Count == 0 {
			input = "unknown"
		}
		if row.Evidence.All.Tokens.Output.Count == 0 {
			output = "unknown"
		}
	}
	return input + " / " + output
}

func evidenceSummary(row pointRow) [][2]string {
	values := evidenceValues(row)
	out := make([][2]string, 0, len(values))
	for i, value := range values {
		out = append(out, [2]string{evidenceColumns[i], unknown(value)})
	}
	return out
}
