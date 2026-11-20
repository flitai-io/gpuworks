package seed

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// perfTradeoffBudget lists the built-in specs whose guarded performance costs
// carry no stated reason yet. It only shrinks: see the file's header.
const perfTradeoffBudget = "testdata/perf_tradeoff_budget.txt"

// perfTradeoffBudgetLines is how many gaps the budget may list. The test
// fails when the file lists more, so a change that adds a gap must also raise
// this number, where review sees it; and when it lists fewer, so the number
// follows every gap closed and cannot keep room for a new one.
const perfTradeoffBudgetLines = 72

// Every guarded performance cost a built-in spec sets (the engine registry,
// engines.ScanPerformanceCosts) must say why in the spec's
// performance_tradeoffs, be exempt as a pooling task, or be one of the gaps
// the budget file still lists. A budget line whose gap is gone fails too, and
// the file's line count is pinned (perfTradeoffBudgetLines), so the budget
// cannot keep room for a new unexplained setting nor grow unseen.
//
//	go test ./internal/server/inferencecatalog/seed/ -run TestBuiltinPerformanceCostsAreExplained
func TestBuiltinPerformanceCostsAreExplained(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	budget := readPerfTradeoffBudget(t)
	switch {
	case len(budget) > perfTradeoffBudgetLines:
		t.Errorf("%s lists %d gaps, more than perfTradeoffBudgetLines (%d): explain the new setting in the spec's performance_tradeoffs instead", perfTradeoffBudget, len(budget), perfTradeoffBudgetLines)
	case len(budget) < perfTradeoffBudgetLines:
		t.Errorf("%s lists %d gaps: lower perfTradeoffBudgetLines from %d to %d", perfTradeoffBudget, len(budget), perfTradeoffBudgetLines, len(budget))
	}
	used := make(map[string]bool, len(budget))
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, finding := range inferencecatalog.UnexplainedPerformanceFindings(spec) {
				line := model.ModelID + "\t" + spec.Name + "\t" + finding.Param
				if _, listed := budget[line]; listed {
					used[line] = true
					continue
				}
				t.Errorf("%s spec %s sets %s %s (%s, in %s) without a reason; add to the spec:\n"+
					"  performance_tradeoffs:\n  - param: %s\n    reason: <%s>\n    detail_en: ...\n    detail_zh: ...",
					model.ModelID, spec.Name, finding.Param, finding.Value, finding.Cost, finding.Where,
					finding.Param, reasonList())
			}
		}
	}
	for line, number := range budget {
		if !used[line] {
			t.Errorf("%s:%d %q is explained or gone; delete the line", perfTradeoffBudget, number, line)
		}
	}
}

func reasonList() string {
	reasons := inferencecatalog.PerformanceTradeoffReasons()
	names := make([]string, len(reasons))
	for i, reason := range reasons {
		names[i] = string(reason)
	}
	return strings.Join(names, "|")
}

// readPerfTradeoffBudget returns each budget line (model, spec and param,
// tab-separated) with its line number. Blank lines and # comments are skipped.
func readPerfTradeoffBudget(t *testing.T) map[string]int {
	t.Helper()
	file, err := os.Open(perfTradeoffBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	budget := map[string]int{}
	scanner := bufio.NewScanner(file)
	for number := 1; scanner.Scan(); number++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Split(line, "\t"); len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			t.Fatalf("%s:%d must be model<TAB>spec<TAB>param: %q", perfTradeoffBudget, number, line)
		}
		if previous, duplicate := budget[line]; duplicate {
			t.Fatalf("%s:%d repeats line %d", perfTradeoffBudget, number, previous)
		}
		budget[line] = number
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return budget
}
