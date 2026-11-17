package aihelper

import (
	"fmt"
	"strings"
	"testing"
)

func TestSeedLogsKeepsNewestWithinBudget(t *testing.T) {
	var lines []LogLine
	for i := 0; i < 100; i++ {
		lines = append(lines, LogLine{TimeMS: int64(i), Line: fmt.Sprintf("%03d %s", i, strings.Repeat("x", 196))})
	}
	kept, dropped := SeedLogs(lines)
	if dropped == 0 || len(kept)+dropped != 100 {
		t.Fatalf("kept %d dropped %d", len(kept), dropped)
	}
	if kept[len(kept)-1].TimeMS != 99 || kept[0].TimeMS != int64(dropped) {
		t.Fatalf("kept the wrong end: first %d last %d", kept[0].TimeMS, kept[len(kept)-1].TimeMS)
	}
	total := 0
	for _, l := range kept {
		total += len(l.Line)
	}
	if total > maxSeedLogBytes {
		t.Fatalf("kept %d bytes, budget %d", total, maxSeedLogBytes)
	}
	// One absurdly long line is clipped, not allowed to evict the rest.
	kept, dropped = SeedLogs([]LogLine{{Line: "a"}, {Line: strings.Repeat("z", 100000)}})
	if dropped != 0 || len(kept[1].Line) > maxLineChars+len("…") {
		t.Fatalf("kept %+v dropped %d", len(kept), dropped)
	}
}

func TestErrorPromptCarriesManifestAndQuestion(t *testing.T) {
	c := ErrorContext{
		InstanceUUID: "i1", State: "failed", Message: "engine exited", DeploymentID: 3, DeploymentName: "qwen",
		Command: []string{"vllm", "serve", "/m"}, TailLines: 60, ErrorLines: 40,
		Logs:     []LogLine{{TimeMS: 1, Level: "error", Line: "CUDA out of memory"}},
		Events:   []EventLine{{Kind: "state", Code: "failed"}},
		Findings: Match([]string{"CUDA out of memory"}),
	}
	p := c.Prompt()
	for _, want := range []string{"Diagnose why", "deployment: qwen (deployment_id 3)", "launch command: vllm serve /m", "## Manifest", "newest 60 lines and up to 40 error-level", "GET /api/v1/logs/search (q, level, from/to, instances)", "GET /api/v1/instances/{uuid}/events", "search_metrics", "[CUDA_OOM]"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	c.Question = "why only replica 2?"
	c.Logs = nil
	p = c.Prompt()
	if !strings.HasPrefix(p, "Question: why only replica 2?") || !strings.Contains(p, "(no log lines recorded)") {
		t.Fatalf("prompt = %s", p)
	}
}

func TestModelPromptSaysPlainlyWhenTheFleetHasNoAccelerators(t *testing.T) {
	p := ModelContext{ModelID: 7, Meta: map[string]any{}, Fit: Fit{ModelID: 7}}.Prompt()
	if !strings.Contains(p, "No online node reports an accelerator") || strings.Contains(p, `"rows"`) {
		t.Fatalf("prompt = %s", p)
	}
}

func TestModelPromptCarriesFitAndManifest(t *testing.T) {
	p := ModelContext{ModelID: 7, Meta: map[string]any{"name": "Qwen3-32B"}, Fit: Fit{ModelID: 7, Rows: []FitRow{{Name: "H100", Fits: true, TP: 1}}}}.Prompt()
	for _, want := range []string{"model_id 7", `"name":"Qwen3-32B"`, `"tp":1`, "## Manifest", "multi-node"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestConversationKeepsTheNewestTurnsOfTheThread(t *testing.T) {
	var history []Message
	for i := 0; i < 14; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, Message{Role: role, Content: fmt.Sprintf("turn %d", i)})
	}
	history = append(history, Message{Role: "tool", Content: "never forwarded"}, Message{Role: "user", Content: "   "}, Message{Role: "assistant", Content: strings.Repeat("x", 9000)})
	msgs := Conversation("sys", "seed", history, " next? ")
	if msgs[0].Content != "sys" || msgs[1].Content != "seed" || msgs[len(msgs)-1].Content != "next?" {
		t.Fatalf("frame = %+v", msgs)
	}
	thread := msgs[2 : len(msgs)-1]
	for _, m := range thread {
		if m.Role == "tool" || strings.TrimSpace(m.Content) == "" {
			t.Fatalf("forwarded %+v", m)
		}
	}
	if thread[0].Content != "turn 7" || len(thread[len(thread)-1].Content) > maxHistoryChars+len("…") {
		t.Fatalf("thread = %d turns starting %q", len(thread), thread[0].Content)
	}
	if got := Conversation("sys", "seed", nil, ""); len(got) != 2 {
		t.Fatalf("no history, no question = %+v", got)
	}
}
