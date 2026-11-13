// Command gitsync keeps the GitLab and GitHub copies of the repository in
// step while merge requests land on both: it lists the synced refs on each
// side, and when one side is a fast-forward of the other it pushes the newer
// commit to the older side. A branch that diverged (both sides merged before
// a sync) or a tag that differs is never resolved by force: the syncer
// leaves both sides alone and alerts a person until they agree again. New
// refs are copied; nothing is ever deleted.
//
// It polls with `git ls-remote`, which costs no GitHub API quota, and runs
// as a long-lived service (scripts/gitsync/gitsync.service). Configuration
// comes from the environment:
//
//	GITSYNC_GITLAB_URL           https clone URL, no credentials in it
//	GITSYNC_GITLAB_USER          default oauth2 (a project or personal access token)
//	GITSYNC_GITLAB_TOKEN
//	GITSYNC_GITHUB_URL           https://github.com/<owner>/<repo>.git
//	GITSYNC_GITHUB_APP_ID        with GITSYNC_GITHUB_APP_KEY_FILE: push as a GitHub App
//	GITSYNC_GITHUB_APP_KEY_FILE  the App's PEM private key
//	GITSYNC_GITHUB_TOKEN         instead of an App: a token git uses as x-access-token
//	GITSYNC_DINGTALK_WEBHOOK     optional: where alerts go besides the log
//
//	go run ./scripts/gitsync -once -dry-run   # one comparison, push nothing
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if runAskpass(os.Args) {
		return
	}
	cache := flag.String("cache", "/var/lib/gitsync/repo.git", "bare repository holding both sides' commits")
	interval := flag.Duration("interval", 30*time.Second, "time between comparisons")
	refs := flag.String("refs", "refs/heads/master,refs/heads/release/[0-9]*,refs/tags/v[0-9]*", "comma-separated git ls-remote patterns of the refs to sync: the default branch, release lines (release/26.11) and version tags")
	dryRun := flag.Bool("dry-run", false, "decide and log, push nothing")
	once := flag.Bool("once", false, "compare once and exit")
	alertRepeat := flag.Duration("alert-repeat", 30*time.Minute, "how often an unresolved problem is reported again")
	failingAfter := flag.Duration("failing-after", 10*time.Minute, "how long cycles may fail before that is reported")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	s, err := newSyncer(*cache, strings.Split(*refs, ","), *dryRun, logger.Printf)
	if err != nil {
		logger.Fatal(err)
	}
	alerts := &alerter{webhook: os.Getenv("GITSYNC_DINGTALK_WEBHOOK"), repeat: *alertRepeat, client: &http.Client{Timeout: 20 * time.Second}, now: time.Now, logf: logger.Printf}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run := &runner{syncer: s, alerts: alerts, failingAfter: *failingAfter, now: time.Now, logf: logger.Printf}
	for {
		if err := run.step(ctx); err != nil && *once {
			logger.Fatal(err)
		}
		if *once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

func newSyncer(cache string, patterns []string, dryRun bool, logf func(string, ...any)) (*syncer, error) {
	gitlabURL, githubURL := os.Getenv("GITSYNC_GITLAB_URL"), os.Getenv("GITSYNC_GITHUB_URL")
	if gitlabURL == "" || githubURL == "" {
		return nil, errors.New("set GITSYNC_GITLAB_URL and GITSYNC_GITHUB_URL")
	}
	for _, raw := range []string{gitlabURL, githubURL} {
		if u, err := url.Parse(raw); err != nil || u.User != nil {
			return nil, fmt.Errorf("%q: give the URL without credentials; tokens come from the environment", redact(raw))
		}
	}
	gitlab := remote{Name: "gitlab", URL: gitlabURL}
	if token := os.Getenv("GITSYNC_GITLAB_TOKEN"); token != "" {
		user := os.Getenv("GITSYNC_GITLAB_USER")
		if user == "" {
			user = "oauth2"
		}
		gitlab.Creds = staticCreds(user, token)
	}
	github := remote{Name: "github", URL: githubURL}
	switch {
	case os.Getenv("GITSYNC_GITHUB_APP_ID") != "":
		pemBytes, err := os.ReadFile(os.Getenv("GITSYNC_GITHUB_APP_KEY_FILE"))
		if err != nil {
			return nil, fmt.Errorf("github app key: %w", err)
		}
		key, err := parseAppKey(pemBytes)
		if err != nil {
			return nil, err
		}
		repo, err := githubRepo(githubURL)
		if err != nil {
			return nil, err
		}
		app := &githubApp{api: "https://api.github.com", appID: os.Getenv("GITSYNC_GITHUB_APP_ID"), key: key, repo: repo, client: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
		github.Creds = app.Creds
	case os.Getenv("GITSYNC_GITHUB_TOKEN") != "":
		github.Creds = staticCreds("x-access-token", os.Getenv("GITSYNC_GITHUB_TOKEN"))
	}
	for i := range patterns {
		patterns[i] = strings.TrimSpace(patterns[i])
		if !strings.HasPrefix(patterns[i], "refs/heads/") && !strings.HasPrefix(patterns[i], "refs/tags/") {
			return nil, fmt.Errorf("ref pattern %q: only refs/heads/ and refs/tags/ are synced", patterns[i])
		}
	}
	if err := openCache(cache); err != nil {
		return nil, err
	}
	return &syncer{gitlab: gitlab, github: github, patterns: patterns, cache: cache, dryRun: dryRun, logf: logf}, nil
}

func staticCreds(user, token string) func(context.Context) (string, string, error) {
	return func(context.Context) (string, string, error) { return user, token, nil }
}

// githubRepo is owner/name from https://github.com/owner/name(.git).
func githubRepo(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("%q is not a https://github.com/<owner>/<repo> URL", raw)
	}
	return parts[0] + "/" + parts[1], nil
}

func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	return raw
}

// runner is one service step: a cycle, then the alerts its outcome calls for.
type runner struct {
	syncer       *syncer
	alerts       *alerter
	failingAfter time.Duration
	now          func() time.Time
	logf         func(format string, args ...any)
	failingSince time.Time
}

func (r *runner) step(ctx context.Context) error {
	cycleCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	actions, err := r.syncer.cycle(cycleCtx)
	problems := map[string]string{}
	for _, a := range actions {
		switch a.Kind {
		case "diverged":
			problems[a.Ref+" "+a.Note] = fmt.Sprintf("%s diverged (%s): merge one side into the other by hand; nothing is synced for it until they agree", a.Ref, a.Note)
		case "tag-mismatch":
			problems[a.Ref+" "+a.Note] = fmt.Sprintf("tag %s differs (%s): tags are never moved; fix one side by hand", a.Ref, a.Note)
		}
	}
	if err != nil {
		r.logf("cycle failed: %v", err)
		if r.failingSince.IsZero() {
			r.failingSince = r.now()
		}
		if r.now().Sub(r.failingSince) >= r.failingAfter {
			problems["failing"] = fmt.Sprintf("syncing has failed since %s: %v", r.failingSince.Format(time.RFC3339), err)
		}
	} else {
		r.failingSince = time.Time{}
	}
	// A failed cycle saw only part of the refs: report it only once it has
	// failed for long enough, rather than forgetting what the refs it did not
	// reach were known to need.
	if ctx.Err() == nil && (err == nil || problems["failing"] != "") {
		r.alerts.report(ctx, problems)
	}
	return err
}
