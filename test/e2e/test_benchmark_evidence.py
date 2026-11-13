"""Regression evidence for corrupted benchmark metrics and exported reports."""
import copy
import csv
import io
import json
import unittest
from benchmark_evidence import CSV_COLUMNS, validate_csv, validate_usage


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.point, self.run, self.values = versioned_fixture()
        self.usage = dict(prompt_tokens=3, completion_tokens=2, total_tokens=5)
        self.csv = export_text(self.values)

    def test_known_usage_and_all_csv_columns_match(self):
        validate_usage(self.point, self.usage)
        self.assertEqual(validate_csv(self.csv, self.point)["requests"], "1")

    def test_wrong_usage_and_approximation_are_rejected(self):
        for patch in ({"input_tokens": 999}, {"output_tokens": 999},
                      {"actual_isl": 999}, {"long_tail": {"approximate_tokens": True}}):
            with self.subTest(patch=patch), self.assertRaises(AssertionError):
                validate_usage({**self.point, **patch}, self.usage)

    def test_explicit_zero_cache_coverage_is_distinct_from_unknown(self):
        validate_csv(self.csv, self.point)
        for value in ("", "99.00%"):
            with self.assertRaises(AssertionError):
                validate_csv(export_text({**self.values, "cache_report_coverage": value}), self.point)

    def test_truncated_malformed_and_wrong_metric_reports_are_rejected(self):
        for text in ("concurrency,success_rate\n1,100garbage\n",
                     self.csv.replace(",5.000,2.000,", ",999,2.000,"),
                     self.csv.replace("100.00%", "100garbage"),
                     self.csv.replace(",5.000,2.000,", ",NaN,2.000,")):
            with self.subTest(text=text), self.assertRaises((AssertionError, ValueError)):
                validate_csv(text, self.point)


# Literal public export contract, independent of the production report writer.
V1_COLUMNS = """
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


def export_text(values, columns=V1_COLUMNS):
    output = io.StringIO()
    csv.writer(output).writerows([columns, [values.get(key, "") for key in columns]])
    return output.getvalue()


def versioned_fixture():
    metric = lambda n, value: dict(count=n, mean_ms=value, p50_ms=value, p99_ms=value)
    token = lambda n, value: dict(count=n, total=value, min=value, max=value, p50=value, p99=value)
    tokens = {key: token(1, value) for key, value in dict(input=3, output=2, context=5,
              server_input=3, server_output=2, local_input=4, local_output=6).items()}
    tokens.update(reasoning=token(0, 0), failed_output=token(0, 0),
                  input_sources={"server_usage": 1}, output_sources={"server_usage": 1})
    group = dict(attempted=1, success=1, failed=0, canceled=0, ttft=metric(1, 1),
                 tpot=metric(1, 10), last_output_latency=metric(1, 11), completion=metric(1, 12),
                 tokens=tokens, cache=dict(reported=0, unknown=1, ratio_requests=0,
                 successful_reported=0, input_tokens=0, cached_tokens=0), finish_reasons={}, error_stages={})
    empty = dict(attempted=0, success=0, failed=0, canceled=0,
                 **{key: metric(0, 0) for key in ["ttft", "tpot", "last_output_latency", "completion"]},
                 tokens={key: ({} if key.endswith("sources") else token(0, 0)) for key in tokens},
                 cache={key: 0 for key in group["cache"]}, finish_reasons={}, error_stages={})
    start, end = "2026-10-20T08:22:49.123456789Z", "2026-10-20T08:22:50.123456789Z"
    evidence = dict(version=1, plan=dict(planned_sessions=1, planned_requests=1, turns_per_session=1,
                    output_budget=8, output_policy="ignore_eos_requested"),
                    outcomes=dict(skipped=0, canceled_before_start=0, completed_sessions=1,
                    failed_sessions=0, canceled_sessions=0, retry_count=0), all=group,
                    first_turn=copy.deepcopy(group), followup_turns=empty, by_turn={"1": copy.deepcopy(group)},
                    timing=dict(started_at=start, finished_at=end, completed_session_latency=metric(1, 1000)),
                    execution=dict(seed="-8320581280513421302", seed_derivation="unchanged", cache_policy="unknown",
                    cache_state="unknown", retry_policy="none", tokenizer_source="", local_token_count_method="character_estimate",
                    detail_policy="origin_configured", warmup_requests=0, warmup_succeeded=0),
                    sessions=[dict(session_id="session-000001", planned_turns=1, attempted=1, success=1, failed=0,
                    canceled=0, skipped=0, state="completed", started_at=start, finished_at=end)], sessions_truncated=False)
    point = dict(seq=1, state="done", concurrency=1, target_isl=16, target_osl=8, request_rate=0,
                 success=1, errors=0, input_tokens=3, output_tokens=2, actual_isl=3, actual_osl=2,
                 total_tput=5, output_tput=2, p50_ttft=1, p99_ttft=1, p50_tpot=10, p99_tpot=10,
                 p50_latency=11, p99_latency=11, started_at=start, ended_at=end,
                 long_tail=dict(approximate_tokens=False, peak_input_tokens=3, evidence=evidence))
    run = dict(id=7, name="e2e-export", mode="grid", model="fixture-model", state="done", created_at=start, started_at=start,
               ended_at=end, keep_detail=False, detail_cap_mb=20, dataset=dict(kind="random", input_tokens=16,
               output_tokens=8, ignore_eos=True), search=dict(concurrency=[1], isl=[16], osl=[8],
               warmup_requests=0, requests_per_point_multiplier=1), target_snapshot={"model": "fixture-model"},
               verdict={"kind": "grid_done", "best_point_seq": 1})
    context = {"run_id": 7, "run_name": "e2e-export", "model": "fixture-model", "mode": "grid",
               "state": "done", "failure_diagnostic": "", "warmup_attempts": None,
               "created_at": start, "started_at": start, "ended_at": end, "detail_configured": False,
               "detail_cap_mb": 20, "dataset": run["dataset"], "search": run["search"], "tokenizer": "",
               "target_snapshot": run["target_snapshot"], "warnings": [], "persisted_verdict": run["verdict"],
               "point_eligibility": [["Point 1 (C=1)", "eligible under declared constraints"]],
               "recommendation": "best measured point in this run: concurrency 1; objective=output_tps; output throughput 2.000 tok/s; interactivity 100.000 tok/s/user (decode only); comparison warnings and declared constraints apply; no capacity upper bound",
               "recommendation_scope": "best measured point in this run; no capacity upper bound",
               "repeats": "see point repeat; no uncertainty estimate",
               "state": "done", "failure_diagnostic": None, "warmup_attempts": []}
    # All scalar wire values are literal; no production formatter/oracle supplies expectations.
    values = dict(zip(V1_COLUMNS, [
        "1", "", "1", "1", "3.000", "3", "2.000", "100.000", "5.000", "2.000", "1.000", "1.000", "10.000", "10.000", "11.000", "11.000", "", "", "0.00%", "100.00%",
        "1", "done", "0", "16", "8", "3", "2", start, end, "1", "1", "1", "0", "0", "0", "0", "1", "0", "0", "100.00%", "0", "8", "ignore_eos_requested", "2", "5", "3.000", "4.000", "2.000", "6.000", "1", "1", "1", "1", "12.000", "12.000", "0", "1", "0", "0", "0", "all_attempted_requests", start, end, "1.000", "60.000", "1", "1000.000", "1000.000",
        "1", "1.000", "1.000", "1", "10.000", "10.000", "12.000", "0", "1", "", "0", "", "", "0", "", "", "", "0", "0", "", "-8320581280513421302", "unchanged", "unknown", "unknown", "none", "disabled", "0", json.dumps(evidence), json.dumps(context)]))
    assert len(values) == 97
    return point, run, values


class VersionedCSVTests(unittest.TestCase):
    def setUp(self):
        self.point, self.run, self.values = versioned_fixture()

    def validate(self, values=None, columns=V1_COLUMNS):
        return validate_csv(export_text(values or self.values, columns), self.point, self.run)

    def test_complete_literal_schema_and_versioned_values_match(self):
        self.assertEqual(CSV_COLUMNS, V1_COLUMNS)
        self.assertEqual(self.validate()["sessions"], "1")

    def test_added_columns_cannot_be_missing_renamed_reordered_or_duplicated(self):
        for columns in [V1_COLUMNS[:-1], V1_COLUMNS[:20], V1_COLUMNS + ["extra"],
                        V1_COLUMNS[:20] + ["wrong"] + V1_COLUMNS[21:],
                        V1_COLUMNS[:20] + V1_COLUMNS[21:23] + [V1_COLUMNS[20]] + V1_COLUMNS[23:],
                        V1_COLUMNS[:-1] + ["evidence_json"]]:
            with self.subTest(columns=columns), self.assertRaises(AssertionError):
                self.validate(columns=columns)

    def test_every_added_scalar_value_is_asserted(self):
        for key in V1_COLUMNS[20:-2]:
            with self.subTest(column=key), self.assertRaises((AssertionError, ValueError)):
                self.validate({**self.values, key: "999"})

    def test_unknown_values_cannot_be_exported_as_false_zero(self):
        for key in ["observed_cache_hit_rate", "followup_ttft_p50_ms", "followup_tpot_p99_ms", "followup_completion_p50_ms", "followup_cache_ratio"]:
            with self.subTest(column=key), self.assertRaises(AssertionError):
                self.validate({**self.values, key: "0.00%" if "ratio" in key or "rate" in key else "0.000"})
        self.point["long_tail"]["evidence"]["all"]["tokens"]["input"]["count"] = 0
        with self.assertRaises(AssertionError):
            self.validate()

    def test_embedded_evidence_and_context_are_independently_verified(self):
        for key, body in [("evidence_json", {}), ("report_context_json", {}),
                          ("evidence_json", {**self.point["long_tail"]["evidence"], "version": 2}),
                          ("report_context_json", {**json.loads(self.values["report_context_json"]), "run_id": 999})]:
            with self.subTest(column=key), self.assertRaises(AssertionError):
                self.validate({**self.values, key: json.dumps(body)})

    def test_embedded_json_cannot_replace_integer_or_boolean_types(self):
        evidence = copy.deepcopy(self.point["long_tail"]["evidence"])
        evidence["outcomes"]["skipped"] = False
        context = json.loads(self.values["report_context_json"])
        context["detail_configured"] = 0
        for key, body in [("evidence_json", evidence), ("report_context_json", context)]:
            with self.subTest(column=key), self.assertRaises(AssertionError):
                self.validate({**self.values, key: json.dumps(body)})

    def test_full_header_with_wrong_row_width_is_rejected(self):
        rows = list(csv.reader(io.StringIO(export_text(self.values))))
        for row in (rows[1][:-1], rows[1] + ["extra"]):
            output = io.StringIO()
            csv.writer(output).writerows([rows[0], row])
            with self.assertRaises(AssertionError):
                validate_csv(output.getvalue(), self.point, self.run)

    def test_attempted_and_planned_denominators_are_distinct(self):
        e = self.point["long_tail"]["evidence"]
        e["plan"].update(planned_sessions=2, planned_requests=3)
        e["outcomes"].update(failed_sessions=1, skipped=1)
        e["all"].update(attempted=2, failed=1)
        e["all"]["cache"].update(reported=1, unknown=1, ratio_requests=1, input_tokens=3, cached_tokens=0)
        self.point["errors"] = 1
        values = {**self.values, "sessions": "2", "requests": "2", "planned_requests": "3", "failed_requests": "1",
                  "skipped_requests": "1", "failed_sessions": "1", "success_rate": "50.00%", "session_completion_rate": "50.00%",
                  "cache_reported_requests": "1", "cache_ratio_requests": "1", "cache_input_tokens": "3",
                  "observed_cache_hit_rate": "0.00%", "cache_report_coverage": "50.00%", "evidence_json": json.dumps(e)}
        self.validate(values)
        for key in ["sessions", "requests", "planned_requests", "success_rate", "session_completion_rate", "cache_report_coverage"]:
            with self.subTest(column=key), self.assertRaises(AssertionError):
                self.validate({**values, key: "100.00%" if key.endswith("rate") or key.endswith("coverage") else "99"})
