package main

import (
	"os"
	"testing"
)

func TestClassifyBusFixDoesNotTrigger(t *testing.T) {
	rules, err := parseRules(mustRules(t))
	if err != nil {
		t.Fatal(err)
	}
	contract, decision := classify(rules, []string{
		"internal/server/bus/bus.go",
		"internal/server/bus/bus_test.go",
	})
	if len(contract) != 0 || len(decision) != 0 {
		t.Fatalf("bus fix triggered contract=%v decision=%v", contract, decision)
	}
}

func TestContractRequiresDocs(t *testing.T) {
	rules, err := parseRules(mustRules(t))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"internal/server/api/errors.go", "internal/server/api/errors_test.go"}
	contract, _ := classify(rules, files)
	if len(contract) != 1 || contract[0] != "internal/server/api/errors.go" {
		t.Fatalf("contract = %v", contract)
	}
	if contractSatisfied(files) {
		t.Fatal("test file must not satisfy the docs requirement")
	}
	if !contractSatisfied(append(files, "docs/reference/api.md")) {
		t.Fatal("docs/ change should satisfy")
	}
	if contractSatisfied([]string{"docs-en/reference/api.md"}) {
		t.Fatal("translation alone must not satisfy")
	}
}

func TestDecisionRecord(t *testing.T) {
	if decisionRecorded([]string{"---\ntitle: x\n---\n"}) {
		t.Fatal("missing fields")
	}
	body := "---\ndecision: keep the random port\nrejected: a fixed port range\n---\n\nwhy\n"
	if !decisionRecorded([]string{body}) {
		t.Fatal("both fields present")
	}
}

func TestRealRulesLeaveBusAlone(t *testing.T) {
	raw, err := os.ReadFile("../change-record-rules.txt")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := parseRules(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	contract, decision := classify(rules, []string{
		"internal/server/bus/bus.go",
		"internal/server/bus/bus_test.go",
	})
	if len(contract) != 0 || len(decision) != 0 {
		t.Fatalf("bus fix triggered contract=%v decision=%v", contract, decision)
	}
}

func mustRules(t *testing.T) string {
	t.Helper()
	return `# comment
contract	internal/server/api/errors.go
decision	internal/agent/modelsync/**
`
}
