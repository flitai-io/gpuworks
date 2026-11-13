"""Independent usage and exact CSV oracles for the single-point external sweep.

E2E standard: test/e2e/browser-e2e-testing-standard.md
The literal public schema is reviewed here; never read it from the report writer.
"""
import csv
from datetime import datetime
from decimal import Decimal
import io
import json
import math
import re

CSV_COLUMNS = """
concurrency request_rate_req_s sessions requests actual_mean_isl actual_peak_isl actual_mean_osl
interactivity_tok_s_user total_tok_s output_tok_s ttft_p50_ms ttft_p99_ms tpot_p50_ms tpot_p99_ms
e2el_p50_ms e2el_p99_ms designed_prefix_reuse_rate observed_cache_hit_rate cache_report_coverage
success_rate point_seq point_state point_repeat target_isl target_osl successful_input_tokens
successful_output_tokens execution_started_at execution_ended_at evidence_version planned_requests
successful_requests failed_requests canceled_requests skipped_requests canceled_before_start
completed_sessions failed_sessions canceled_sessions session_completion_rate retry_count
output_budget output_policy actual_peak_osl actual_peak_context server_mean_isl local_mean_isl
server_mean_osl local_mean_osl ttft_samples tpot_samples last_output_latency_samples
stream_completion_samples stream_completion_p50_ms stream_completion_p99_ms cache_reported_requests
cache_unknown_requests cache_ratio_requests cache_input_tokens cached_tokens cache_coverage_scope
measurement_start measurement_end measurement_seconds completed_sessions_per_minute
session_latency_samples session_latency_p50_ms session_latency_p99_ms first_turn_ttft_samples
first_turn_ttft_p50_ms first_turn_ttft_p99_ms first_turn_tpot_samples first_turn_tpot_p50_ms
first_turn_tpot_p99_ms first_turn_completion_p50_ms first_turn_cache_reported
first_turn_cache_unknown first_turn_cache_ratio followup_ttft_samples followup_ttft_p50_ms
followup_ttft_p99_ms followup_tpot_samples followup_tpot_p50_ms followup_tpot_p99_ms
followup_completion_p50_ms followup_cache_reported followup_cache_unknown followup_cache_ratio
effective_seed seed_derivation cache_policy cache_state retry_policy detail_status
retained_detail_records evidence_json report_context_json
""".split()


def validate_usage(point, usage):
    assert usage["total_tokens"] == usage["prompt_tokens"] + usage["completion_tokens"]
    assert point["input_tokens"] == usage["prompt_tokens"], "input usage differs"
    assert point["output_tokens"] == usage["completion_tokens"], "output usage differs"
    assert point["success"] == 1 and point["errors"] == 0
    assert point["actual_isl"] == usage["prompt_tokens"], "actual ISL differs"
    assert point["actual_osl"] == usage["completion_tokens"], "actual OSL differs"
    assert point["long_tail"]["approximate_tokens"] is False, "usage was approximated"



def _seconds(timestamp):
    # Keep RFC3339 nanoseconds: rounding to Python microseconds corrupts the
    # independently derived sessions/minute on short measurement windows.
    match = re.fullmatch(r"(.*?)(?:\.(\d+))?Z", timestamp)
    assert match, "CSV evidence time must be UTC RFC3339"
    whole = datetime.fromisoformat(match[1] + "+00:00").timestamp()
    return Decimal(int(whole)) + Decimal("0." + (match[2] or "0"))


def _metric(group, key, percentile):
    summary = group[key]
    return float(summary[percentile + "_ms"]) if summary["count"] > 0 else None


def _token(group, key, peak=False):
    distribution = group["tokens"][key]
    if distribution["count"] == 0:
        return None
    return distribution["max"] if peak else distribution["total"] / distribution["count"]


def _ratio(numerator, denominator):
    return numerator / denominator * 100 if denominator > 0 else None


def _context(value, run, point):
    # state, failure_diagnostic and warmup_attempts joined the context when a run could be held
    # waiting for a recoverable inference service; they are part of the schema now, so a report that
    # stopped carrying them would be a regression rather than a simplification.
    expected_keys = """created_at dataset detail_cap_mb detail_configured ended_at failure_diagnostic mode model
        persisted_verdict point_eligibility recommendation recommendation_scope repeats run_id run_name
        search started_at state target_snapshot tokenizer warmup_attempts warnings""".split()
    assert set(value) == set(expected_keys), (
        "CSV report context schema differs: extra=" + str(sorted(set(value) - set(expected_keys)))
        + " absent=" + str(sorted(set(expected_keys) - set(value))))
    assert value["recommendation_scope"] == "best measured point in this run; no capacity upper bound"
    assert value["repeats"] == "see point repeat; no uncertainty estimate"
    assert isinstance(value["warnings"], list) and all(isinstance(item, str) for item in value["warnings"])
    # The three added keys are asserted rather than merely counted: a finished, successful run
    # reports its own state and no failure diagnostic, so a context that leaked another run's
    # failure or reported a different state would fail here.
    assert value["state"] in ("done", "canceled", "failed"), f"unexpected state {value['state']}"
    assert value["failure_diagnostic"] in (None, ""), (
        f"a successful run carries a failure diagnostic: {value['failure_diagnostic']}")
    assert isinstance(value["warmup_attempts"], (list, type(None))), (
        f"warmup attempts is not a record list: {type(value['warmup_attempts'])}")
    if run is None:
        return
    assert value["state"] == run["state"]
    assert value["failure_diagnostic"] == run.get("error", "")
    assert not value["warmup_attempts"] and run["search"]["warmup_requests"] == 0
    for field, source in {"run_id": "id", "run_name": "name", "model": "model", "mode": "mode",
            "created_at": "created_at", "started_at": "started_at", "ended_at": "ended_at",
            "detail_configured": "keep_detail", "detail_cap_mb": "detail_cap_mb", "dataset": "dataset",
            "search": "search", "target_snapshot": "target_snapshot", "persisted_verdict": "verdict"}.items():
        assert json.dumps(value[field], sort_keys=True) == json.dumps(run.get(source), sort_keys=True), f"CSV report context {field} differs from independent GET"
    assert value["tokenizer"] == run.get("tokenizer_ref", ""), "CSV tokenizer differs"
    assert all(item in value["warnings"] for item in run.get("warnings", [])), "CSV lost persisted warnings"
    # This case is one successful closed-loop grid point without SLA gates.
    assert value["point_eligibility"] == [[f"Point {point['seq']} (C={point['concurrency']})", "eligible under declared constraints"]]
    tpot = _metric(point["long_tail"]["evidence"]["all"], "tpot", "p50")
    interaction = f"{1000 / tpot:.3f}" if tpot and tpot > 0 else "unknown"
    expected = (f"best measured point in this run: concurrency {point['concurrency']}; objective=output_tps; "
                f"output throughput {point['output_tput']:.3f} tok/s; interactivity {interaction} tok/s/user (decode only); "
                "comparison warnings and declared constraints apply; no capacity upper bound")
    assert value["recommendation"] == expected, "CSV recommendation differs from measured point"


def validate_csv(text, point, run=None):
    rows = list(csv.reader(io.StringIO(text)))
    assert len(rows) == 2, "CSV must have one point row"
    assert rows[0] == CSV_COLUMNS, "CSV schema differs"
    assert len(rows[1]) == len(CSV_COLUMNS), "CSV row width differs"
    row = dict(zip(CSV_COLUMNS, rows[1]))
    evidence = point["long_tail"].get("evidence")
    assert evidence and evidence["version"] == 1, "New sweep must retain v1 evidence"
    assert json.dumps(json.loads(row["evidence_json"]), sort_keys=True) == json.dumps(evidence, sort_keys=True), "CSV embedded evidence differs from independent GET"
    _context(json.loads(row["report_context_json"]), run, point)
    group, plan, outcomes = evidence["all"], evidence["plan"], evidence["outcomes"]
    cache, timing, execution = group["cache"], evidence["timing"], evidence["execution"]
    tpot = _metric(group, "tpot", "p50")
    expected = {name: None for name in CSV_COLUMNS[:-2]}
    expected.update({
        "concurrency": point["concurrency"], "request_rate_req_s": None,
        "sessions": plan["planned_sessions"] or None, "requests": group["attempted"],
        "actual_mean_isl": _token(group, "input"), "actual_peak_isl": _token(group, "input", True),
        "actual_mean_osl": _token(group, "output"),
        "interactivity_tok_s_user": 1000 / tpot if tpot and tpot > 0 else None,
        "total_tok_s": float(point["total_tput"]), "output_tok_s": float(point["output_tput"]),
        "ttft_p50_ms": _metric(group, "ttft", "p50"), "ttft_p99_ms": _metric(group, "ttft", "p99"),
        "tpot_p50_ms": tpot, "tpot_p99_ms": _metric(group, "tpot", "p99"),
        "e2el_p50_ms": _metric(group, "last_output_latency", "p50"),
        "e2el_p99_ms": _metric(group, "last_output_latency", "p99"),
        "observed_cache_hit_rate": (_ratio(cache["cached_tokens"], cache["input_tokens"])
                                     if cache["ratio_requests"] > 0 else None),
        "cache_report_coverage": _ratio(cache["reported"], group["attempted"]),
        "success_rate": _ratio(group["success"], group["attempted"]),
        "point_seq": point["seq"], "point_state": point["state"], "point_repeat": 0,
        "target_isl": point["target_isl"], "target_osl": point["target_osl"],
        "successful_input_tokens": point["input_tokens"], "successful_output_tokens": point["output_tokens"],
        "execution_started_at": point.get("started_at"), "execution_ended_at": point.get("ended_at"),
        "evidence_version": 1, "planned_requests": plan["planned_requests"],
        "successful_requests": group["success"], "failed_requests": group["failed"], "canceled_requests": group["canceled"],
        "skipped_requests": outcomes["skipped"], "canceled_before_start": outcomes["canceled_before_start"],
        "completed_sessions": outcomes["completed_sessions"], "failed_sessions": outcomes["failed_sessions"],
        "canceled_sessions": outcomes["canceled_sessions"],
        "session_completion_rate": _ratio(outcomes["completed_sessions"], plan["planned_sessions"]),
        "retry_count": outcomes["retry_count"], "output_budget": plan["output_budget"], "output_policy": plan["output_policy"],
        "actual_peak_osl": _token(group, "output", True), "actual_peak_context": _token(group, "context", True),
        "server_mean_isl": _token(group, "server_input"), "local_mean_isl": _token(group, "local_input"),
        "server_mean_osl": _token(group, "server_output"), "local_mean_osl": _token(group, "local_output"),
        "ttft_samples": group["ttft"]["count"], "tpot_samples": group["tpot"]["count"],
        "last_output_latency_samples": group["last_output_latency"]["count"], "stream_completion_samples": group["completion"]["count"],
        "stream_completion_p50_ms": _metric(group, "completion", "p50"), "stream_completion_p99_ms": _metric(group, "completion", "p99"),
        "cache_reported_requests": cache["reported"], "cache_unknown_requests": cache["unknown"],
        "cache_ratio_requests": cache["ratio_requests"], "cache_input_tokens": cache["input_tokens"], "cached_tokens": cache["cached_tokens"],
        "cache_coverage_scope": "all_attempted_requests", "measurement_start": timing["started_at"], "measurement_end": timing["finished_at"],
        "session_latency_samples": timing["completed_session_latency"]["count"],
        "session_latency_p50_ms": (_metric({"session": timing["completed_session_latency"]}, "session", "p50")),
        "session_latency_p99_ms": (_metric({"session": timing["completed_session_latency"]}, "session", "p99")),
        "effective_seed": execution["seed"], "seed_derivation": execution["seed_derivation"],
        "cache_policy": execution["cache_policy"], "cache_state": execution["cache_state"], "retry_policy": execution["retry_policy"],
        "detail_status": "disabled", "retained_detail_records": 0,
    })
    if group["success"] > 0:
        for key, field in [("input", "successful_input_tokens"), ("output", "successful_output_tokens")]:
            if group["tokens"][key]["count"] == 0:
                expected[field] = None
                expected["total_tok_s"] = expected["output_tok_s"] = None
    if timing["started_at"] and timing["finished_at"]:
        seconds = _seconds(timing["finished_at"]) - _seconds(timing["started_at"])
        if seconds > 0:
            expected["measurement_seconds"] = float(seconds)
            expected["completed_sessions_per_minute"] = float(Decimal(outcomes["completed_sessions"] * 60) / seconds)
    for prefix, key in [("first_turn", "first_turn"), ("followup", "followup_turns")]:
        turn = evidence[key]
        for metric in ["ttft", "tpot"]:
            expected[prefix + "_" + metric + "_samples"] = turn[metric]["count"]
            for percentile in ["p50", "p99"]:
                expected[prefix + "_" + metric + "_" + percentile + "_ms"] = _metric(turn, metric, percentile)
        expected[prefix + "_completion_p50_ms"] = _metric(turn, "completion", "p50")
        expected[prefix + "_cache_reported"] = turn["cache"]["reported"]
        expected[prefix + "_cache_unknown"] = turn["cache"]["unknown"]
        expected[prefix + "_cache_ratio"] = (_ratio(turn["cache"]["cached_tokens"], turn["cache"]["input_tokens"])
                                               if turn["cache"]["ratio_requests"] > 0 else None)
    for name, value in expected.items():
        actual = row[name]
        if value is None:
            assert actual == "", f"CSV {name} must be empty, got {actual!r}"
        elif isinstance(value, (str, int)):
            assert actual == str(value), f"CSV {name} differs"
        else:
            percentage = name in ("success_rate", "session_completion_rate", "observed_cache_hit_rate", "cache_report_coverage", "first_turn_cache_ratio", "followup_cache_ratio")
            if percentage:
                assert actual.endswith("%"), f"CSV {name} lost its percent unit"
                actual = actual[:-1]
            number = float(actual)
            assert math.isfinite(number), f"CSV {name} is not finite"
            tolerance = 0.0051 if percentage else 0.00051
            assert abs(number - value) <= tolerance, f"CSV {name} differs: {number} != {value}"
    return row
