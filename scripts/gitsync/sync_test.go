package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests sync two real bare repositories with the real git CLI: a fake
// would only show that the syncer calls git, not that the pushes it makes
// are the ones a remote accepts.

type fixture struct {
	t              *testing.T
	gitlab, github string // bare repositories
	work           string // a clone that commits and pushes to either
	s              *syncer
	logs           []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	f := &fixture{t: t, gitlab: filepath.Join(root, "gitlab.git"), github: filepath.Join(root, "github.git"), work: filepath.Join(root, "work")}
	for _, bare := range []string{f.gitlab, f.github} {
		f.run(root, "init", "--quiet", "--bare", "--initial-branch=master", bare)
	}
	f.run(root, "init", "--quiet", "--initial-branch=master", f.work)
	f.run(f.work, "config", "user.email", "test@example.com")
	f.run(f.work, "config", "user.name", "test")
	f.commit("base")
	f.run(f.work, "push", "--quiet", f.gitlab, "master")
	f.run(f.work, "push", "--quiet", f.github, "master")
	cache := filepath.Join(root, "cache.git")
	if err := openCache(cache); err != nil {
		t.Fatal(err)
	}
	f.s = &syncer{
		gitlab:   remote{Name: "gitlab", URL: f.gitlab},
		github:   remote{Name: "github", URL: f.github},
		patterns: []string{"refs/heads/master", "refs/heads/release/[0-9]*", "refs/tags/v[0-9]*"},
		cache:    cache,
		logf:     func(format string, args ...any) { f.logs = append(f.logs, format) },
	}
	return f
}

func (f *fixture) run(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) commit(message string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, message+".txt"), []byte(message), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.run(f.work, "add", ".")
	f.run(f.work, "commit", "--quiet", "-m", message)
	return f.run(f.work, "rev-parse", "HEAD")
}

func (f *fixture) ref(bare, ref string) string {
	f.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = bare
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (f *fixture) cycle() []action {
	f.t.Helper()
	actions, err := f.s.cycle(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return actions
}

func TestEqualSidesDoNothing(t *testing.T) {
	f := newFixture(t)
	if actions := f.cycle(); len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
}

func TestFastForwardGoesToTheSideBehind(t *testing.T) {
	for _, tc := range []struct{ ahead, behind, to string }{{"gitlab", "github", "github"}, {"github", "gitlab", "gitlab"}} {
		t.Run(tc.ahead+" ahead", func(t *testing.T) {
			f := newFixture(t)
			sides := map[string]string{"gitlab": f.gitlab, "github": f.github}
			f.commit("one")
			head := f.commit("two")
			f.run(f.work, "push", "--quiet", sides[tc.ahead], "master")

			actions := f.cycle()
			if len(actions) != 1 || actions[0].Kind != "push" || actions[0].To != tc.to || actions[0].SHA != head {
				t.Fatalf("actions = %+v, want one push of %s to %s", actions, head, tc.to)
			}
			if got := f.ref(sides[tc.behind], "refs/heads/master"); got != head {
				t.Fatalf("%s master = %s, want %s", tc.behind, got, head)
			}
			if again := f.cycle(); len(again) != 0 {
				t.Fatalf("second cycle = %+v, want nothing left to do", again)
			}
		})
	}
}

func TestDivergedBranchIsLeftAloneAndReported(t *testing.T) {
	f := newFixture(t)
	base := f.ref(f.gitlab, "refs/heads/master")
	left := f.commit("left")
	f.run(f.work, "push", "--quiet", f.gitlab, "master")
	f.run(f.work, "reset", "--quiet", "--hard", base)
	right := f.commit("right")
	f.run(f.work, "push", "--quiet", f.github, "master")

	actions := f.cycle()
	if len(actions) != 1 || actions[0].Kind != "diverged" || !strings.Contains(actions[0].Note, left) || !strings.Contains(actions[0].Note, right) {
		t.Fatalf("actions = %+v, want master reported diverged with both commits", actions)
	}
	if f.ref(f.gitlab, "refs/heads/master") != left || f.ref(f.github, "refs/heads/master") != right {
		t.Fatal("a diverged branch was changed")
	}
}

func TestNewRefsAreCopiedAndTagsNeverMove(t *testing.T) {
	f := newFixture(t)
	f.commit("release")
	f.run(f.work, "tag", "-a", "-m", "v26.11.3", "v26.11.3")
	f.run(f.work, "push", "--quiet", f.gitlab, "HEAD:refs/heads/release/26.11", "v26.11.3")
	// Branches the patterns do not name stay where they are, including one
	// under release/ that is not a release line.
	f.run(f.work, "push", "--quiet", f.github, "HEAD:refs/heads/feature", "HEAD:refs/heads/release/notes-guard")

	f.cycle()
	for _, ref := range []string{"refs/heads/release/26.11", "refs/tags/v26.11.3"} {
		if got, want := f.ref(f.github, ref), f.ref(f.gitlab, ref); got == "" || got != want {
			t.Fatalf("github %s = %q, want %q", ref, got, want)
		}
	}
	for _, ref := range []string{"refs/heads/feature", "refs/heads/release/notes-guard"} {
		if f.ref(f.gitlab, ref) != "" {
			t.Fatalf("unsynced branch %s was copied", ref)
		}
	}

	// The same tag name on another commit is reported, not moved.
	f.commit("later")
	f.run(f.work, "tag", "-f", "-a", "-m", "moved", "v26.11.3")
	f.run(f.work, "push", "--quiet", "--force", f.github, "v26.11.3")
	gitlabTag := f.ref(f.gitlab, "refs/tags/v26.11.3")
	actions := f.cycle()
	if len(actions) != 1 || actions[0].Kind != "tag-mismatch" {
		t.Fatalf("actions = %+v, want one tag-mismatch", actions)
	}
	if f.ref(f.gitlab, "refs/tags/v26.11.3") != gitlabTag {
		t.Fatal("a tag was moved")
	}
}

func TestDryRunPushesNothing(t *testing.T) {
	f := newFixture(t)
	f.s.dryRun = true
	before := f.ref(f.github, "refs/heads/master")
	f.commit("ahead")
	f.run(f.work, "push", "--quiet", f.gitlab, "master")
	if actions := f.cycle(); len(actions) != 1 || actions[0].Kind != "push" {
		t.Fatalf("actions = %+v, want the push it would make", actions)
	}
	if f.ref(f.github, "refs/heads/master") != before {
		t.Fatal("a dry run pushed")
	}
}

// A side that moved between the listing and the push refuses the push (no
// force), and the cycle reports the error instead of overwriting it.
func TestPushNeverForces(t *testing.T) {
	f := newFixture(t)
	base := f.ref(f.gitlab, "refs/heads/master")
	f.commit("ahead")
	f.run(f.work, "push", "--quiet", f.gitlab, "master")
	// Make github's master move to a sibling right before the push.
	f.run(f.work, "reset", "--quiet", "--hard", base)
	sibling := f.commit("sibling")
	// The credentials hook runs before each git command on github: the first
	// is the listing, which still sees the old master; before the next one,
	// someone merges the sibling there.
	listed := false
	f.s.github.Creds = func(context.Context) (string, string, error) {
		if listed {
			f.run(f.work, "push", "--quiet", f.github, "master")
		}
		listed = true
		return "", "", nil
	}
	if _, err := f.s.cycle(context.Background()); err == nil {
		t.Fatal("a push onto a moved branch succeeded")
	}
	if got := f.ref(f.github, "refs/heads/master"); got != sibling {
		t.Fatalf("github master = %s, want the sibling %s left in place", got, sibling)
	}
}

func TestUnreachableSideFailsTheCycleAndIsReportedOnlyWhenItLasts(t *testing.T) {
	f := newFixture(t)
	f.s.github.URL = filepath.Join(t.TempDir(), "missing.git")
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	var sent []string
	alerts := &alerter{repeat: 30 * time.Minute, now: func() time.Time { return now }, logf: func(format string, args ...any) {
		if strings.HasPrefix(format, "alert:") {
			sent = append(sent, args[0].(string))
		}
	}}
	r := &runner{syncer: f.s, alerts: alerts, failingAfter: 10 * time.Minute, now: func() time.Time { return now }, logf: func(string, ...any) {}}
	if err := r.step(context.Background()); err == nil {
		t.Fatal("a cycle against a missing side succeeded")
	}
	if len(sent) != 0 {
		t.Fatalf("alerted after one failure: %v", sent)
	}
	now = now.Add(11 * time.Minute)
	_ = r.step(context.Background())
	if len(sent) != 1 || !strings.Contains(sent[0], "syncing has failed since") {
		t.Fatalf("alerts = %v, want one failing alert", sent)
	}
	now = now.Add(time.Minute)
	_ = r.step(context.Background())
	if len(sent) != 1 {
		t.Fatalf("alerts = %v, want no repeat within the repeat interval", sent)
	}
}
