package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// caseTB stands in for one case: its own name, outcome and cleanups, so the
// failure log can be driven through a failing and a passing case.
type caseTB struct {
	*testing.T
	name     string
	failed   bool
	cleanups []func()
}

func (c *caseTB) Name() string        { return c.name }
func (c *caseTB) Failed() bool        { return c.failed }
func (c *caseTB) Cleanup(f func())    { c.cleanups = append(c.cleanups, f) }
func (c *caseTB) Logf(string, ...any) {}
func (c *caseTB) finish(failed bool) {
	c.failed = failed
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

func TestAFailedCaseKeepsItsServerAndAgentLogsAndAPassingOneLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	failing := &caseTB{T: t, name: "TestDockerExit/rebuild after stop"}
	failureLogger(failing, dir).Info("server: deployment created", "id", 7)
	failureLogger(failing, dir).Debug("agent: docker run -d", "container", "c1")
	passing := &caseTB{T: t, name: "TestDockerSmoke"}
	failureLogger(passing, dir).Info("server: all good")

	passing.finish(false)
	failing.finish(true)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "TestDockerExit_rebuild_after_stop.log" {
		t.Fatalf("kept %v, want only the failing case's log", entries)
	}
	body, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	server := strings.Index(string(body), "server: deployment created")
	agent := strings.Index(string(body), "agent: docker run -d")
	if server < 0 || agent < server {
		t.Fatalf("log should hold the server line then the debug agent line in one file:\n%s", body)
	}
}

func TestAFailureLogKeepsOnlyTheEndOfALongCase(t *testing.T) {
	buf := &tailBuffer{limit: 10}
	for _, chunk := range []string{"0123456", "789abc", "def"} {
		if _, err := buf.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "case.log")
	if err := buf.writeFile(path); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	want := "... 6 earlier bytes dropped, last 10 kept ...\n6789abcdef"
	if string(body) != want {
		t.Fatalf("got %q, want %q", body, want)
	}
}
