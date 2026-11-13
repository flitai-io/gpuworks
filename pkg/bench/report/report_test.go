package report

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestRenderReportFormatsAndRedactsSecrets(t *testing.T) {
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	run := Run{
		ID: 3, Name: `GLM <5.2> / Agentic 80K`, EvaluationURL: "http://gpuworks.example/evaluations/3", State: "done", Mode: "grid", Model: "glm-5.2", CreatedAt: created,
		TokenizerRef:   "hf://org/model",
		Dataset:        []byte(`{"kind":"agentic","mean_input_tokens":79998,"peak_input_tokens":85836,"required_context_tokens":90000,"output_tokens":220,"turns":13,"design_prefix_reuse_rate":0.92,"workload_fingerprint":"agentic-80k-v1-abc"}`),
		Search:         []byte(`{"sessions_per_point":{"1":4,"2":8}}`),
		TargetSnapshot: []byte(`{"served_name":"glm-5.2","engine":"sglang","image":"example/sglang:latest","argv":["python","--model","glm"],"env":{"NORMAL":"visible","API_KEY":"do-not-leak"}}`),
		Warnings:       []string{"BENCHMARK_AGENTIC_LENGTH_DRIFT"},
		Points: []Point{
			{Seq: 1, Concurrency: 1, Success: 52, ActualISL: 90000, ActualOSL: 220, OutputTput: 100, TotalTput: 150, RequestTput: 1.2, P50TTFT: 40, P99TTFT: 80, P50TPOT: 4, P99TPOT: 8, LongTail: []byte(`{"KVHitRate":0.91,"CacheReportCoverage":1,"PeakInputTokens":85800}`)},
			{Seq: 2, Concurrency: 2, Success: 104, ActualISL: 80010, ActualOSL: 220, OutputTput: 190, TotalTput: 260, RequestTput: 2.1, P50TTFT: 50, P99TTFT: 95, P50TPOT: 5, P99TPOT: 9},
		},
	}

	md, err := Render("markdown", run)
	if err != nil {
		t.Fatal(err)
	}
	markdown := string(md.Body)
	for _, want := range []string{"# GLM <5.2> / Agentic 80K", "http://gpuworks.example/evaluations/3", "actual_peak_isl", "best measured point in this run: concurrency", "API_KEY=[REDACTED]", "NORMAL=visible", "BENCHMARK_AGENTIC_LENGTH_DRIFT"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "do-not-leak") || md.Filename != "GLM-5.2-Agentic-80K-agentic-80k-v1-abc.md" {
		t.Fatalf("unsafe markdown document: filename=%q body=%s", md.Filename, markdown)
	}

	csvDoc, err := Render("csv", run)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(csvDoc.Body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || strings.Join(records[0], ",") != strings.Join(append(append([]string{}, columns...), "evidence_json", "report_context_json"), ",") || records[1][column("sessions")] != "4" || records[1][column("requests")] != "52" || records[1][column("observed_cache_hit_rate")] != "91.00%" || records[1][column("interactivity_tok_s_user")] != "250.000" || records[1][column("request_rate_req_s")] != "" {
		t.Fatalf("csv records = %#v", records)
	}

	htmlDoc, err := Render("html", run)
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlDoc.Body)
	for _, want := range []string{"<!doctype html>", "<svg", "C=1", "http://gpuworks.example/evaluations/3", "GLM &lt;5.2&gt; / Agentic 80K", "API_KEY=[REDACTED]", "no external resources or JavaScript"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "do-not-leak") || strings.Contains(html, "<script") {
		t.Fatalf("unsafe standalone html: %s", html)
	}
}

func TestReportFiltersWarningsUsingCurrentAgenticDriftThreshold(t *testing.T) {
	base := Run{ID: 8, Dataset: []byte(`{"kind":"agentic","mean_input_tokens":10000}`), Warnings: []string{"BENCHMARK_AGENTIC_LENGTH_DRIFT"}}
	base.Points = []Point{{ActualISL: 10_526.288}}
	doc, err := Render("markdown", base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc.Body), "BENCHMARK_AGENTIC_LENGTH_DRIFT") {
		t.Fatalf("5.3%% legacy drift warning should be hidden:\n%s", doc.Body)
	}
	base.Points[0].ActualISL = 11_001
	doc, err = Render("markdown", base)
	if err != nil || !strings.Contains(string(doc.Body), "BENCHMARK_AGENTIC_LENGTH_DRIFT") {
		t.Fatalf("drift above 10%% should remain: err=%v body=%s", err, doc.Body)
	}
}

func TestRenderRejectsUnknownFormatAndHandlesMissingCache(t *testing.T) {
	if _, err := Render("pdf", Run{}); err == nil {
		t.Fatal("expected unsupported format error")
	}
	doc, err := Render("csv", Run{ID: 9, Points: []Point{{Concurrency: 1, Success: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := csv.NewReader(strings.NewReader(string(doc.Body))).ReadAll()
	if got := records[1][column("observed_cache_hit_rate")]; got != "" {
		t.Fatalf("missing cache rate should stay empty, got %q", got)
	}
}

func TestRandomReportUsesFixedLengthFieldsInsteadOfEmptyAgenticFields(t *testing.T) {
	run := Run{ID: 7, Name: "Random fixed length", Model: "Qwen3-8B", Dataset: []byte(`{"kind":"random","input_tokens":4096,"output_tokens":1024}`), TargetSnapshot: []byte(`{"target_kind":"external","base_url":"http://example.test:3000","load_origin":"server"}`), Points: []Point{{Concurrency: 4, Success: 20, ActualISL: 4325.5, ActualOSL: 681.65, OutputTput: 120, LongTail: []byte(`{"PeakInputTokens":4372}`)}}}
	doc, err := Render("markdown", run)
	if err != nil {
		t.Fatal(err)
	}
	body := string(doc.Body)
	for _, want := range []string{"| Input length | 4096 tokens |", "| Output limit | 1024 tokens |", "| 4 |  |  | 20 | 4325.500 | 4372.000 |", "| Target type | external |", "| Endpoint | http://example.test:3000 |", "| Load origin | server |"} {
		if !strings.Contains(body, want) {
			t.Fatalf("random report missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"| Mean input |", "| Peak input |", "| Required context |", "| Turns per session |", "| Designed prefix reuse |", "recommended concurrency", "| Deployment | — |", "| GPU count | — |", "### Environment", "designed prefix reuse rate describes"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("random report contains Agentic-only field %q:\n%s", unwanted, body)
		}
	}
}

func TestDatasetSpecificReportPropertiesAndConversationSessions(t *testing.T) {
	tests := []struct {
		name, dataset  string
		want, unwanted []string
	}{
		{
			name: "multiturn", dataset: `{"kind":"multiturn","input_tokens":9999,"output_tokens":128}`,
			want:     []string{"| Corpus | OASST1 multi-turn |", "| Turns per conversation | up to 3 |", "| Output limit | 128 tokens |"},
			unwanted: []string{"| Input length |", "| Mean input |", "| Samples |"},
		},
		{
			name: "imported", dataset: `{"kind":"imported","profile":{"sample_count":12,"max_input_tokens":640,"max_output_tokens":96}}`,
			want:     []string{"| Corpus | Imported JSONL |", "| Samples | 12 |", "| Maximum declared input | 640 tokens |", "| Maximum declared output | 96 tokens |"},
			unwanted: []string{"| Input length |", "| Turns per conversation |"},
		},
		{
			name: "retired", dataset: `{"kind":"agenttrace"}`,
			want:     []string{"| Dataset status | Retired; historical Run only |"},
			unwanted: []string{"| Input length |", "| Mean input |"},
		},
		{
			name: "unknown", dataset: `{"kind":"future-dataset"}`,
			unwanted: []string{"| Input length |", "| Mean input |", "| Corpus |"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := Run{ID: 10, Name: test.name, Model: "model", Dataset: []byte(test.dataset), Search: []byte(`{"requests_per_point_multiplier":4}`), Points: []Point{{Concurrency: 2, Success: 12, OutputTput: 10}}}
			doc, err := Render("markdown", run)
			if err != nil {
				t.Fatal(err)
			}
			body := string(doc.Body)
			for _, want := range test.want {
				if !strings.Contains(body, want) {
					t.Fatalf("report missing %q:\n%s", want, body)
				}
			}
			for _, unwanted := range test.unwanted {
				if strings.Contains(body, unwanted) {
					t.Fatalf("report contains mismatched field %q:\n%s", unwanted, body)
				}
			}
			if test.name == "multiturn" || test.name == "imported" || test.name == "retired" {
				csvDoc, err := Render("csv", run)
				if err != nil {
					t.Fatal(err)
				}
				records, _ := csv.NewReader(strings.NewReader(string(csvDoc.Body))).ReadAll()
				if got := records[1][column("sessions")]; got != "8" {
					t.Fatalf("conversation sessions = %q, want 8", got)
				}
			}
		})
	}
}

func column(name string) int {
	for i, c := range columns {
		if c == name {
			return i
		}
	}
	panic("unknown report column " + name)
}

func TestReportInteractivityAndLatencyUseTPOTAndE2EL(t *testing.T) {
	run := Run{ID: 11, Dataset: []byte(`{"kind":"random","input_tokens":8192,"output_tokens":256}`), Points: []Point{
		// 2 s TTFT and 20 ms TPOT: throughput ÷ concurrency would claim 36
		// tok/s/user, the decode-phase speed is 50.
		{Seq: 1, Concurrency: 8, Success: 80, OutputTput: 288, P50TTFT: 2000, P50TPOT: 20, P99TPOT: 25, P50Latency: 7100, P99Latency: 9000},
		// A point without a measured TPOT has no interactivity.
		{Seq: 2, Concurrency: 16, Success: 160, OutputTput: 300},
	}}
	doc, err := Render("csv", run)
	if err != nil {
		t.Fatal(err)
	}
	records, _ := csv.NewReader(strings.NewReader(string(doc.Body))).ReadAll()
	if got := records[1][column("interactivity_tok_s_user")]; got != "50.000" {
		t.Fatalf("interactivity = %q", got)
	}
	if got := records[2][column("interactivity_tok_s_user")]; got != "" {
		t.Fatalf("interactivity without TPOT = %q", got)
	}
	if records[1][column("e2el_p50_ms")] != "7100.000" || records[1][column("e2el_p99_ms")] != "9000.000" {
		t.Fatalf("e2el = %v", records[1])
	}
	md, _ := Render("markdown", run)
	if !strings.Contains(string(md.Body), "`interactivity_tok_s_user = 1000 / tpot_p50_ms`") || strings.Contains(string(md.Body), "output_tok_s / concurrency") {
		t.Fatalf("metric definitions:\n%s", md.Body)
	}
	html, _ := Render("html", run)
	if !strings.Contains(string(html.Body), "<code>interactivity_tok_s_user = 1000 / tpot_p50_ms</code>") {
		t.Fatalf("html definitions:\n%s", html.Body)
	}
}

func TestReportOpenLoopPointsShowRateAndCap(t *testing.T) {
	run := Run{ID: 12, Dataset: []byte(`{"kind":"random"}`), Points: []Point{
		{Seq: 1, RequestRate: 2.5, Success: 150, OutputTput: 100, P50TPOT: 10},
		{Seq: 2, RequestRate: 5, Concurrency: 32, Success: 300, OutputTput: 180, P50TPOT: 12},
	}}
	doc, err := Render("csv", run)
	if err != nil {
		t.Fatal(err)
	}
	records, _ := csv.NewReader(strings.NewReader(string(doc.Body))).ReadAll()
	if records[1][column("request_rate_req_s")] != "2.500" || records[1][column("concurrency")] != "" {
		t.Fatalf("unlimited open-loop point = %v", records[1])
	}
	if records[2][column("request_rate_req_s")] != "5.000" || records[2][column("concurrency")] != "32" {
		t.Fatalf("capped open-loop point = %v", records[2])
	}
	html, _ := Render("html", run)
	if !strings.Contains(string(html.Body), ">R=2.5<") {
		t.Fatalf("html chart labels:\n%s", html.Body)
	}
}

func TestWarmupFailureWithoutMeasuredPointsHasDiagnosticReport(t *testing.T) {
	run := Run{ID: 98, Name: "warmup failure", State: "failed", Error: "warmup recovery exhausted: engine restarting", Warmup: []loadgen.DetailRecord{{Version: 1, RunID: 98, PointSeq: 0, SessionID: "warmup-1", Attempt: 2, HTTPStatus: 503, ErrorStage: "http", ErrorText: `{"error":{"message":"engine restarting https:\/\/report-user:report-password@host.test\/?access_token=REPORT_SECRET"}}`, Text: "BODY_MUST_NOT_BE_EXPORTED"}}}
	for _, format := range []string{"markdown", "html", "csv"} {
		t.Run(format, func(t *testing.T) {
			document, err := Render(format, run)
			if err != nil {
				t.Fatal(err)
			}
			text := string(document.Body)
			if !strings.Contains(text, "engine restarting") || strings.Contains(text, "REPORT_SECRET") || strings.Contains(text, "report-user") || strings.Contains(text, "report-password") || strings.Contains(text, "BODY_MUST_NOT_BE_EXPORTED") {
				t.Fatalf("missing or unsafe diagnostic: %s", text)
			}
			if format == "csv" {
				rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
				if err != nil || len(rows) != 2 {
					t.Fatalf("rows=%v err=%v", rows, err)
				}
				for _, cell := range rows[1][:len(rows[1])-2] {
					if cell != "" {
						t.Fatalf("warmup invented point measurement: %v", rows[1])
					}
				}
				if !strings.Contains(rows[1][len(rows[1])-1], `"point_seq":0`) {
					t.Fatal("warmup lost separate phase evidence")
				}
			} else if !strings.Contains(text, "Warmup recovery evidence") || !strings.Contains(text, "excluded from measured requests") {
				t.Fatal("warmup not clearly separate from measurements")
			}
		})
	}
}

func TestLegacyWarmupErrorIsDecodedBeforeExportRedaction(t *testing.T) {
	run := Run{ID: 98, Name: "historical warmup failure", State: "failed", Error: `all 1 warmup requests failed: endpoint returned 400: {"error":{"message":"earlier engine failure https:\/\/history-user:history-password@host.test\/?access_token=HISTORY_QUERY_SECRET"}}`}
	for _, format := range []string{"markdown", "html", "csv"} {
		document, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		text := string(document.Body)
		for _, secret := range []string{"history-user", "history-password", "HISTORY_QUERY_SECRET"} {
			if strings.Contains(text, secret) {
				t.Fatalf("historical run.Error leaked through %s", format)
			}
		}
		if !strings.Contains(text, "earlier engine failure") {
			t.Fatalf("legacy diagnostic lost useful cause: %s", text)
		}
	}
}

func TestWarmupReportDistinguishesCanceledAttemptFromFailure(t *testing.T) {
	document, err := Render("markdown", Run{State: "canceled", Warmup: []loadgen.DetailRecord{{SessionID: "warmup-1", Attempt: 1, Canceled: true, ErrorStage: "transport", ErrorText: "context canceled"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(document.Body), "| canceled | unknown | transport |") {
		t.Fatalf("cancellation misreported as failure: %s", document.Body)
	}
}

func TestReportStatesTheConfiguredRequestTimeout(t *testing.T) {
	for _, tc := range []struct{ search, want string }{
		{`{"concurrency":1,"request_timeout_s":1800}`, "| Configured request timeout | 1800 seconds |"},
		// A run stored before the setting existed does not record what bounded
		// its requests, so the report claims no value for it.
		{`{"concurrency":1}`, "| Configured request timeout | not recorded |"},
	} {
		run := Run{ID: 7, Name: "timeout", Model: "m", Dataset: []byte(`{"kind":"random","input_tokens":8,"output_tokens":4}`), Search: []byte(tc.search), Points: []Point{{Concurrency: 1, Success: 1}}}
		doc, err := Render("markdown", run)
		if err != nil {
			t.Fatal(err)
		}
		if body := string(doc.Body); !strings.Contains(body, tc.want) {
			t.Fatalf("report for %s missing %q:\n%s", tc.search, tc.want, body)
		}
	}
}
