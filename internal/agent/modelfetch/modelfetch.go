// Package modelfetch fetches one model repository from the addresses the
// server planned for it (proto.ModelFileTask.Candidates): it lists the files
// and downloads each one at the first address that serves it, moving on to
// the next address when one fails or stalls twice, and remembers the
// addresses that failed so the next download tries them last.
//
// It knows repositories and addresses, not tasks: where the files go, disk
// admission and what is reported stay with the caller (agent/app).
package modelfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// StallsBeforeSwitch is how many consecutive stalls (download.ErrStalled)
// one address may cost a file before the fetch moves to the next address.
const StallsBeforeSwitch = 2

// ListingRetry is the file name Config.OnRetry is given for a retry of the
// repository listing rather than of a file.
const ListingRetry = "(file list)"

// MaxThrottleWait bounds the wait one throttled answer (429/503) can ask
// for: a Hugging Face rate-limit window is five minutes, so a reset time
// beyond that is not one to trust. The extra second is the one
// modelrepo.ThrottleWait adds to a reset: a quota exhausted at the start of
// a window (t=300) must still be waited out past its reset.
const MaxThrottleWait = 5*time.Minute + time.Second

// Cooler remembers addresses that just failed; a cooled host's candidates are
// tried after every other one. The node's registry prober
// (registryprobe.Prober) is the one memory image pulls and model downloads
// share.
type Cooler interface {
	Cool(host string)
	Cooled(host string) bool
}

// Switch is the fetch moving from one address to the next.
type Switch struct {
	From, To string // hosts
	Reason   string
}

// Config is one repository fetch.
type Config struct {
	Source, Repo string
	Candidates   []proto.ModelFetchCandidate
	// Client is the HTTP client requests to base go through (the node's
	// proxy; for an HTTP directory, one confined to its host).
	Client func(base string) (*http.Client, error)
	// Cooler is optional; without one nothing is remembered.
	Cooler Cooler
	// Retry is the same-address retry of a transient transport failure;
	// zero is DefaultRetry.
	Retry RetryPolicy
	// OnSwitch is called whenever the fetch moves to another address.
	OnSwitch func(Switch)
	// OnRetry is called before a same-address retry.
	OnRetry func(file string, attempt int, delay time.Duration, err error)
	// StallTimeout replaces the downloader's stall limit; zero keeps it.
	StallTimeout time.Duration
	// Logins are the node's logins; a request carries one only as
	// modelrepo.AuthorizationFor allows (its own host, https).
	Logins []modelrepo.Login
}

// Fetcher walks one repository's candidates. It is not safe for concurrent
// use; files are fetched one after another.
type Fetcher struct {
	cfg      Config
	order    []proto.ModelFetchCandidate
	at       int
	failures []string
}

// New orders the candidates: every cooled address after every other one,
// otherwise in the planned order.
func New(cfg Config) (*Fetcher, error) {
	if len(cfg.Candidates) == 0 {
		return nil, errors.New("no address to fetch the model from")
	}
	if cfg.Client == nil {
		cfg.Client = func(string) (*http.Client, error) { return nil, nil }
	}
	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry = DefaultRetry
	}
	var fresh, cooled []proto.ModelFetchCandidate
	for _, candidate := range cfg.Candidates {
		if cfg.Cooler != nil && cfg.Cooler.Cooled(Host(candidate.Base)) {
			cooled = append(cooled, candidate)
			continue
		}
		fresh = append(fresh, candidate)
	}
	return &Fetcher{cfg: cfg, order: append(fresh, cooled...)}, nil
}

// Host is the host (and port) of an address, as cooling and messages name it.
func Host(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Host)
}

// Host is the address the fetch is at now.
func (f *Fetcher) Host() string {
	if f.at >= len(f.order) {
		return ""
	}
	return Host(f.order[f.at].Base)
}

// List lists the repository's files at revision, from the first address that
// answers. An address that cannot is cooled and the next one asked; one that
// asks for time (429/503) is waited for and asked again, as a file is.
func (f *Fetcher) List(ctx context.Context, revision string) ([]modelrepo.File, error) {
	for f.at < len(f.order) {
		candidate := f.order[f.at]
		maxAttempts := f.attempts()
		delay := f.cfg.Retry.InitialBackoff
		for attempt := 1; ; attempt++ {
			client, err := f.cfg.Client(candidate.Base)
			if err == nil {
				var files []modelrepo.File
				headers := modelrepo.AuthorizationHeaders(candidate.Base, f.cfg.Logins)
				files, err = modelrepo.ListFiles(ctx, f.cfg.Source, f.cfg.Repo, revision, candidate.Base, headers, modelrepo.RequestOptions{Client: client})
				if err == nil {
					return files, nil
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			retryAfter, throttled := ThrottleWait(err)
			if !throttled || attempt >= maxAttempts {
				// Whatever else this address answered - a missing repository,
				// an error status, a portal page that does not parse, no
				// answer at all - another address may still list it.
				f.next("failed: " + err.Error())
				break
			}
			pause := min(max(delay, retryAfter), MaxThrottleWait)
			if f.cfg.OnRetry != nil {
				f.cfg.OnRetry(ListingRetry, attempt, pause, err)
			}
			if err := sleep(ctx, pause); err != nil {
				return nil, err
			}
			delay = nextBackoff(delay, f.cfg.Retry.MaxBackoff)
		}
	}
	return nil, f.exhausted("list files")
}

// attempts bounds the attempts at the address the fetch is at: the last
// address, with nothing to move to, gets the patience a single address
// always had.
func (f *Fetcher) attempts() int {
	if f.at == len(f.order)-1 && f.cfg.Retry.LastAttempts > f.cfg.Retry.MaxAttempts {
		return f.cfg.Retry.LastAttempts
	}
	return f.cfg.Retry.MaxAttempts
}

// ThrottleWait reports whether err is a source asking the fetch to wait (a
// 429 or 503 on a listing or a file), and how long it asked for (zero when
// it did not say).
func ThrottleWait(err error) (time.Duration, bool) {
	if wait, ok := download.IsThrottled(err); ok {
		return wait, true
	}
	var status *modelrepo.StatusError
	if errors.As(err, &status) && status.Throttled() {
		return status.RetryAfter, true
	}
	return 0, false
}

// File is one repository file to fetch.
type File struct {
	Path string
	// Revision is the revision the file is resolved at; a commit pins it.
	Revision     string
	Dest         string
	Concurrency  int
	RateLimitKBs int
}

// Fetch downloads one file, starting at the address the fetch is at. A file
// pinned to a commit continues its partial bytes at the next address; one
// that is not starts over there, since another address may serve other
// bytes under the same branch name.
func (f *Fetcher) Fetch(ctx context.Context, file File, onProgress func(download.Progress)) error {
	pinned := isCommit(file.Revision)
	for f.at < len(f.order) {
		candidate := f.order[f.at]
		reason, err := f.fetchFrom(ctx, candidate, file, pinned, onProgress)
		if err == nil {
			return nil
		}
		if reason == "" {
			return err // not the address's doing: retrying elsewhere cannot help
		}
		if !pinned {
			_ = os.Remove(file.Dest + ".part")
			_ = os.Remove(file.Dest + ".part.state")
		}
		f.next(reason)
	}
	return f.exhausted("download " + file.Path)
}

// fetchFrom downloads file from one address, retrying a transient failure
// there. A non-empty reason says the address failed and the next one should
// be tried; an empty one with an error is a failure no address can fix.
func (f *Fetcher) fetchFrom(ctx context.Context, candidate proto.ModelFetchCandidate, file File, pinned bool, onProgress func(download.Progress)) (string, error) {
	client, err := f.cfg.Client(candidate.Base)
	if err != nil {
		return err.Error(), err
	}
	address := modelrepo.ResolveURL(f.cfg.Source, f.cfg.Repo, file.Revision, file.Path, candidate.Base)
	options := download.Options{
		URL: address, Dest: file.Dest, Concurrency: file.Concurrency,
		RateLimitKBs: file.RateLimitKBs, Client: client, StallTimeout: f.cfg.StallTimeout,
		Headers: modelrepo.AuthorizationHeaders(address, f.cfg.Logins),
	}
	if pinned {
		options.ResumeKey = f.cfg.Source + "/" + f.cfg.Repo + "@" + file.Revision + "/" + file.Path
	}
	last := f.at == len(f.order)-1
	maxAttempts := f.attempts()
	delay := f.cfg.Retry.InitialBackoff
	stalls := 0
	for attempt := 1; ; attempt++ {
		err := download.Download(ctx, options, onProgress)
		if err == nil {
			return "", nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if isLocal(err) {
			return "", fmt.Errorf("download %s: %w", file.Path, err)
		}
		// A throttled answer is the address asking for time, not saying it
		// cannot serve the file: wait what it asked (at least the backoff)
		// and ask the same address again.
		pause := delay
		if retryAfter, throttled := ThrottleWait(err); throttled {
			stalls = 0
			pause = min(max(delay, retryAfter), MaxThrottleWait)
		} else if errors.Is(err, download.ErrStalled) {
			stalls++
			if stalls >= StallsBeforeSwitch && !last {
				return fmt.Sprintf("stalled %d times in a row: %s: %v", stalls, address, err), err
			}
		} else {
			stalls = 0
			if !IsTransient(err) {
				return fmt.Sprintf("failed: %s: %v", address, err), err
			}
		}
		if attempt >= maxAttempts {
			return fmt.Sprintf("failed %d times: %s: %v", attempt, address, err), err
		}
		if f.cfg.OnRetry != nil {
			f.cfg.OnRetry(file.Path, attempt, pause, err)
		}
		if err := sleep(ctx, pause); err != nil {
			return "", err
		}
		delay = nextBackoff(delay, f.cfg.Retry.MaxBackoff)
	}
}

// next cools the current address, records why it failed and moves on.
func (f *Fetcher) next(reason string) {
	from := f.order[f.at]
	if f.cfg.Cooler != nil {
		f.cfg.Cooler.Cool(Host(from.Base))
	}
	failure := Host(from.Base) + " " + reason
	f.failures = append(f.failures, failure)
	f.at++
	if f.at < len(f.order) && f.cfg.OnSwitch != nil {
		f.cfg.OnSwitch(Switch{From: Host(from.Base), To: Host(f.order[f.at].Base), Reason: failure})
	}
}

// exhausted is the failure once no address is left: every address's reason,
// in the order they were tried.
func (f *Fetcher) exhausted(what string) error {
	if len(f.failures) == 1 {
		return fmt.Errorf("%s: %s", what, f.failures[0])
	}
	return fmt.Errorf("%s: every source failed: %s", what, strings.Join(f.failures, "; "))
}

// isCommit reports whether revision names a commit rather than a branch or
// tag: what the hubs pin a registration to (Hugging Face's 40 hex digits,
// ModelScope's full or short commit id).
func isCommit(revision string) bool {
	revision = strings.TrimSpace(revision)
	if len(revision) < 7 {
		return false
	}
	for _, r := range revision {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
