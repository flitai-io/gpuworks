package enginelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fieldLog(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "field", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The captured start whose schedulers all raised while the HTTP server
// stayed up says so in the engine's own words, with the exception as its
// root cause. No other captured start, healthy or starved, and none of the
// repository's other SGLang logs says it.
func TestStartAbortedIsWhatSGLangSaysWhenAChildFailed(t *testing.T) {
	crashed := fieldLog(t, "sglang_ascend_nerdctl_schedulers_crashed.txt")
	aborted := func(dialect, text string) bool {
		_, ok := StartAborted(dialect, text)
		return ok
	}
	cause, ok := StartAborted(DialectSGLang, crashed)
	if !ok || cause != "RuntimeError: Unsupport run graph with different stream." {
		t.Fatalf("the crashed start: aborted=%v cause=%q", ok, cause)
	}
	if aborted(DialectVLLM, crashed) || aborted("", crashed) {
		t.Fatal("another dialect's output is read as SGLang's")
	}
	// Either statement alone is enough: the scheduler's, or the server's.
	for _, marker := range []string{"Scheduler hit an exception", "SIGQUIT received"} {
		var kept []string
		for _, line := range strings.Split(crashed, "\n") {
			if !strings.Contains(line, marker) {
				kept = append(kept, line)
			}
		}
		if !aborted(DialectSGLang, strings.Join(kept, "\n")) {
			t.Fatalf("without the %q lines the crashed start is not recognized", marker)
		}
	}

	logs, err := filepath.Glob(filepath.Join("testdata", "field", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	others, err := filepath.Glob(filepath.Join("testdata", "*sglang*"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	// Captured starts that are aborted starts: each says it in the engine's
	// own words. The rest of the captures, healthy or starved, and the
	// repository's other logs, do not.
	abortedStarts := map[string]bool{
		"sglang_ascend_nerdctl_schedulers_crashed.txt": true,
		"sglang_cuda_docker_capture_assert.txt":        true,
	}
	for _, path := range append(logs, others...) {
		if abortedStarts[filepath.Base(path)] {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // a directory
		}
		checked++
		if aborted(DialectSGLang, string(raw)) {
			t.Errorf("%s is read as an aborted start", path)
		}
	}
	if checked < 5 {
		t.Fatalf("only %d other logs were checked", checked)
	}
	// Prose about the same words is not the engine's statement.
	for _, line := range []string{
		"INFO the scheduler hit an exception last week, see the runbook",
		"echo 'SIGQUIT received' is what one child failed to print",
	} {
		if aborted(DialectSGLang, line) {
			t.Errorf("%q is read as an aborted start", line)
		}
	}
}
