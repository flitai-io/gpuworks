package selfinstall

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The seams in Deps exist so install and upgrade are hermetic in tests; every
// one of them must still be wired in production, or a run would dereference a
// nil function far from the cause.
func TestNewDepsWiresEverySeam(t *testing.T) {
	deps := NewDeps(io.Discard)
	seams := map[string]bool{
		"Run":            deps.Run == nil,
		"Client":         deps.Client == nil,
		"Now":            deps.Now == nil,
		"Sleep":          deps.Sleep == nil,
		"StartDetached":  deps.StartDetached == nil,
		"Euid":           deps.Euid == nil,
		"LookupUser":     deps.LookupUser == nil,
		"LookupAccount":  deps.LookupAccount == nil,
		"HasBinary":      deps.HasBinary == nil,
		"SystemdRunning": deps.SystemdRunning == nil,
		"FreeBytes":      deps.FreeBytes == nil,
		"Health":         deps.Health == nil,
	}
	for name, missing := range seams {
		if missing {
			t.Errorf("Deps.%s is not wired", name)
		}
	}
	if deps.HealthTimeout <= 0 || deps.StopTimeout <= 0 {
		t.Fatalf("timeouts = %s, %s", deps.HealthTimeout, deps.StopTimeout)
	}
	if deps.Euid() != os.Geteuid() {
		t.Fatal("the effective uid is not the process's own")
	}
	if !deps.LookupUser(currentUserName()) {
		t.Fatalf("the invoking user %q was not found", currentUserName())
	}
	if free, err := deps.FreeBytes("."); err != nil || free == 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
}

func TestNewIOUsesTheProcessStreams(t *testing.T) {
	io := NewIO()
	if io.In != os.Stdin || io.Out != os.Stdout || io.Err != os.Stderr {
		t.Fatalf("io = %+v", io)
	}
	io.Say("a message")
	io.Warn("a warning")
}

// `upgrade --quiet` must still report failures: silencing the progress
// narration is the point, hiding the reason an upgrade rolled back is not.
func TestQuietIOKeepsWarningsAndDropsTheNarration(t *testing.T) {
	var out, errOut strings.Builder
	o := &IO{In: strings.NewReader(""), Out: &out, Err: &errOut, Quiet: true}
	o.Say("downloading %s", "gpuworks-v1.2.3-linux-amd64.tar.gz")
	o.Warn("restoring %s", "/opt/gpuworks/server/backups/v1.2.3-20260910T090000Z.tar.gz")
	if out.String() != "" {
		t.Fatalf("quiet output = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "restoring") {
		t.Fatalf("the warning was swallowed: %q", errOut.String())
	}
}

// A prompt must never be attempted when nothing can answer it: an unattended
// run would hang instead of failing where the operator can see it.
func TestPromptsRefuseAStdinThatCannotAnswer(t *testing.T) {
	o := &IO{In: strings.NewReader("yes\n"), Out: io.Discard, Err: io.Discard}
	if o.Terminal() {
		t.Fatal("a pipe was taken for a terminal")
	}
	if _, err := o.Prompt("continue? "); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("Prompt err = %v", err)
	}
	if _, err := o.PromptPassword("password: "); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("PromptPassword err = %v", err)
	}
}
