package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// remote is one side of the sync: a repository URL without credentials and
// the account git authenticates as there (see askpass.go).
type remote struct {
	Name  string // "gitlab" or "github": the namespace of its fetched refs
	URL   string
	Creds func(ctx context.Context) (user, password string, err error)
}

// action is what one cycle decided for one ref.
type action struct {
	Ref  string
	Kind string // "push" or "diverged" or "tag-mismatch"
	// For a push: the commit, and the side it goes to.
	SHA  string
	To   string
	Note string
}

// syncer moves fast-forwards between the two sides and refuses everything
// else: it never force-pushes, never moves a tag and never deletes a ref, so
// a mistake on either side can always be undone by a person.
type syncer struct {
	gitlab, github remote
	patterns       []string // ls-remote patterns: refs/heads/master, refs/tags/v*, ...
	cache          string   // a bare repository holding both sides' commits
	dryRun         bool
	logf           func(format string, args ...any)
}

// cycle compares the two sides once and pushes every fast-forward. It returns
// what it did or would do, and the refs that need a person.
func (s *syncer) cycle(ctx context.Context) ([]action, error) {
	left, err := s.lsRemote(ctx, s.gitlab)
	if err != nil {
		return nil, err
	}
	right, err := s.lsRemote(ctx, s.github)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for ref := range left {
		refs[ref] = true
	}
	for ref := range right {
		refs[ref] = true
	}
	names := make([]string, 0, len(refs))
	for ref := range refs {
		names = append(names, ref)
	}
	sort.Strings(names)

	var differ []string
	for _, ref := range names {
		if left[ref] != right[ref] {
			differ = append(differ, ref)
		}
	}
	if len(differ) == 0 {
		return nil, nil
	}
	if err := s.fetch(ctx, s.gitlab, differ, left); err != nil {
		return nil, err
	}
	if err := s.fetch(ctx, s.github, differ, right); err != nil {
		return nil, err
	}

	var actions []action
	for _, ref := range differ {
		a, b := left[ref], right[ref]
		var next action
		switch {
		case b == "":
			next = action{Ref: ref, Kind: "push", SHA: a, To: s.github.Name, Note: "new on " + s.gitlab.Name}
		case a == "":
			next = action{Ref: ref, Kind: "push", SHA: b, To: s.gitlab.Name, Note: "new on " + s.github.Name}
		case strings.HasPrefix(ref, "refs/tags/"):
			next = action{Ref: ref, Kind: "tag-mismatch", Note: fmt.Sprintf("%s has %s, %s has %s", s.gitlab.Name, a, s.github.Name, b)}
		default:
			switch {
			case s.isAncestor(ctx, a, b):
				next = action{Ref: ref, Kind: "push", SHA: b, To: s.gitlab.Name, Note: "fast-forward from " + s.github.Name}
			case s.isAncestor(ctx, b, a):
				next = action{Ref: ref, Kind: "push", SHA: a, To: s.github.Name, Note: "fast-forward from " + s.gitlab.Name}
			default:
				next = action{Ref: ref, Kind: "diverged", Note: fmt.Sprintf("%s has %s, %s has %s", s.gitlab.Name, a, s.github.Name, b)}
			}
		}
		if next.Kind == "push" {
			if err := s.push(ctx, next); err != nil {
				return actions, err
			}
		}
		actions = append(actions, next)
	}
	return actions, nil
}

// lsRemote lists the synced refs of one side, ref -> commit. Annotated tags
// are compared by the tag object, which is what a push transfers.
func (s *syncer) lsRemote(ctx context.Context, r remote) (map[string]string, error) {
	out, err := s.git(ctx, &r, append([]string{"ls-remote", r.URL}, s.patterns...)...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", r.Name, err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok || strings.HasSuffix(ref, "^{}") {
			continue
		}
		refs[ref] = sha
	}
	return refs, nil
}

// fetch brings one side's copy of the refs that differ into the cache, under
// a namespace of its own, so its commits are there to compare and push. Only
// refs the side has are fetched; ls-remote patterns may use wildmatch
// classes that a fetch refspec cannot.
func (s *syncer) fetch(ctx context.Context, r remote, refs []string, has map[string]string) error {
	args := []string{"fetch", "--quiet", "--no-tags", r.URL}
	for _, ref := range refs {
		if has[ref] != "" {
			args = append(args, "+"+ref+":refs/gitsync/"+r.Name+"/"+strings.TrimPrefix(ref, "refs/"))
		}
	}
	if len(args) == 4 {
		return nil
	}
	if _, err := s.git(ctx, &r, args...); err != nil {
		return fmt.Errorf("fetch %s: %w", r.Name, err)
	}
	return nil
}

func (s *syncer) isAncestor(ctx context.Context, ancestor, descendant string) bool {
	_, err := s.git(ctx, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// push sends one commit to one side without force: if that side moved since
// it was listed, the push is refused and the next cycle looks again.
func (s *syncer) push(ctx context.Context, a action) error {
	r := s.gitlab
	if a.To == s.github.Name {
		r = s.github
	}
	if s.dryRun {
		s.logf("dry run: would push %s %s to %s (%s)", a.Ref, a.SHA, r.Name, a.Note)
		return nil
	}
	if _, err := s.git(ctx, &r, "push", "--quiet", r.URL, a.SHA+":"+a.Ref); err != nil {
		return fmt.Errorf("push %s to %s: %w", a.Ref, r.Name, err)
	}
	s.logf("pushed %s %s to %s (%s)", a.Ref, a.SHA, r.Name, a.Note)
	return nil
}

// git runs one git command in the cache. With a remote, the remote's
// credentials reach git through the askpass hook, never through argv or the
// URL, so they show up in neither ps nor an error message.
func (s *syncer) git(ctx context.Context, r *remote, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.cache
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if r != nil && r.Creds != nil {
		user, password, err := r.Creds(ctx)
		if err != nil {
			return "", fmt.Errorf("credentials for %s: %w", r.Name, err)
		}
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+self, askpassEnv+"=1", askpassUserEnv+"="+user, askpassPasswordEnv+"="+password)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}

// openCache creates the bare cache repository once.
func openCache(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command("git", "init", "--quiet", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init %s: %v %s", dir, err, out)
	}
	return nil
}
