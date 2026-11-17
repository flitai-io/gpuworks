package imagepull

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Timing of a pull; variables so tests can shrink them.
var (
	// StallTimeout is how long a download may make no headway before its
	// source is given up on.
	StallTimeout = 3 * time.Minute
	// HeartbeatInterval is how often a running pull reports progress and is
	// checked for a stall.
	HeartbeatInterval = 2 * time.Second
	// StallMinRate (bytes per second) stretches StallTimeout for a runtime
	// that shows a layer's bytes only in coarse steps (nerdctl: 0.1 GiB past
	// 1 GiB): the window grows until one step at this rate fits in it, so a
	// slow download is not taken for a stopped one. See Progress.StallWindow.
	StallMinRate int64 = 100 << 10
	// CandidateTimeout bounds one source's whole pull, unpacking included;
	// a download that stops is caught by the stall watch long before it.
	CandidateTimeout = 60 * time.Minute
	// CrawlWindow and CrawlMinRate give up on a source that does deliver
	// bytes, so never trips the stall watch, but averages under CrawlMinRate
	// bytes per second over a whole CrawlWindow. That is only done while
	// another source is left to try; the last one is let run (CandidateTimeout
	// still bounds it). A runtime that shows a layer's bytes only in coarse
	// steps is not judged this way: its counter cannot tell a crawl from the
	// gap between two steps (see StallMinRate).
	// The floor is the whole pull's rate: 64 KiB/s keeps a node whose link to
	// the source is merely slow from abandoning a source that works.
	CrawlWindow        = 5 * time.Minute
	CrawlMinRate int64 = 64 << 10
)

var (
	// ErrStalled marks a source that stopped downloading.
	ErrStalled = errors.New("no download progress")
	// ErrCrawling marks a source that downloads too slowly to be worth
	// waiting for while another source remains.
	ErrCrawling = errors.New("download too slow")
)

// DiskSpaceError marks a pull failure caused by local storage exhaustion.
// Trying another registry cannot help because every source is unpacked onto
// the same node filesystem.
type DiskSpaceError struct{ Err error }

func (e *DiskSpaceError) Error() string { return "insufficient disk space: " + e.Err.Error() }
func (e *DiskSpaceError) Unwrap() error { return e.Err }

// diskSpaceFailure reports a pull that ran the node out of disk. The runtime
// drivers mark it where the runtime ran (runtime.ErrNoSpace); a write this
// agent made itself fails with ENOSPC.
func diskSpaceFailure(err error) bool {
	return err != nil && (errors.Is(err, runtime.ErrNoSpace) || runtime.DiskFull(err))
}

// Config wires a Puller to the node. Every field is optional.
type Config struct {
	Log *slog.Logger
	// Network reads the node's network config (its proxy).
	Network func() proto.NetworkConfig
	// Auth reads the node's registry logins. They are read at each pull and
	// handed to the runtime for that pull only.
	Auth func() []proto.SourceAuth
	// DaemonProxy reports the proxy the Docker daemon itself pulls through;
	// ok is false when it cannot be observed.
	DaemonProxy func(ctx context.Context) (httpProxy, httpsProxy, noProxy string, ok bool)
	// Prober is shared by every pull on the node, so a registry that just
	// failed is remembered. Nil makes a private one.
	Prober *registryprobe.Prober
	// Now is the clock the stall check reads; nil is the wall clock.
	Now func() time.Time
}

// Puller pulls an image from the first of its sources that works. One Puller
// serves the whole node.
type Puller struct {
	cfg    Config
	prober *registryprobe.Prober
}

// New returns a Puller.
func New(cfg Config) *Puller {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	prober := cfg.Prober
	if prober == nil {
		prober = registryprobe.New()
	}
	return &Puller{cfg: cfg, prober: prober}
}

// ProbesRegistries reports whether the puller ranks sources by probing the
// real registries (registryprobe.Prober.ReachesNetwork).
func (p *Puller) ProbesRegistries() bool { return p.prober.ReachesNetwork() }

// Options says how one walk differs from another.
type Options struct {
	// LaunchIntent: a container is about to be created from the image, so the
	// runtime may prepare its root filesystem during the download.
	LaunchIntent bool
	// NodeProxy: pull through the node's configured proxy where the runtime
	// pulls in a process of its own. Docker's daemon owns its proxy, so it is
	// left alone.
	NodeProxy bool
	// LegacyProxy is the proxy an older server recorded with the instance
	// ("" means direct); nil means the server chose none. It applies to every
	// runtime, as it always did.
	LegacyProxy *string
}

// Snapshot is a pull's progress at one moment.
type Snapshot struct {
	Message  string
	Phase    string
	Done     int64
	Total    int64
	Rate     int64
	Fraction float64
}

// Hooks lets the caller report what a walk is doing. Every hook is optional.
type Hooks struct {
	// Ordered gets a one-line summary of the probe that ordered the sources.
	// It is called at most once per walk, and only if sources were probed.
	Ordered func(summary string)
	// Attempt: a source is about to be pulled.
	Attempt func(ref string)
	// Line: the runtime printed a line. changed is the progress parser's
	// verdict that the line is worth reporting as a state change.
	Line func(ref, line string, s Snapshot, changed bool)
	// Heartbeat: the periodic report of a running pull, quiet runtimes included.
	Heartbeat func(ref string, s Snapshot)
	// Failed: a source did not work (it failed to pull, stalled, or Pulled
	// refused it).
	Failed func(ref string, err error)
	// Pulled finishes a successful pull, for instance by tagging the image
	// with its canonical name. An error counts against the source and the
	// walk goes on to the next one.
	Pulled func(ctx context.Context, ref string) error
	// Recovered runs after a failed pull: a source can fail after delivering
	// what the caller needs, so the caller may say the image is usable.
	Recovered func(ctx context.Context) (ref string, ok bool)
}

// Result is the source a walk ended with.
type Result struct {
	Ref string
	// Recovered: no source succeeded but Hooks.Recovered found the image.
	Recovered bool
}

// AllFailedError is a walk in which no source worked.
type AllFailedError struct {
	// Failures are the "ref: error" of each source tried.
	Failures []string
}

func (e *AllFailedError) Error() string {
	if len(e.Failures) == 0 {
		return "no image source to pull from"
	}
	return "all image sources failed: " + strings.Join(e.Failures, " | ")
}

// Walk pulls from cands in order — after ranking the runs the server marked
// rankable — until one works. A runtime that cannot run at all aborts the walk
// with its error (no source could help). Otherwise the error is an
// *AllFailedError.
func (p *Puller) Walk(ctx context.Context, driver runtime.Driver, cands []proto.ImagePullCandidate, opts Options, h Hooks) (Result, error) {
	cands = cleaned(cands)
	if len(cands) == 0 {
		return Result{}, &AllFailedError{}
	}
	if hasRankRun(cands) {
		var report registryprobe.Report
		cands, report = p.prober.Rank(ctx, cands, p.routeFor(ctx, driver))
		if len(report.Hosts) > 0 && h.Ordered != nil {
			h.Ordered(report.String())
		}
	}

	var failures []string
	fail := func(ref string, err error) {
		failures = append(failures, ref+": "+err.Error())
		if h.Failed != nil {
			h.Failed(ref, err)
		}
	}
	for i, c := range cands {
		if ctx.Err() != nil {
			break
		}
		err := p.pullOne(ctx, driver, c.Ref, opts, h, i < len(cands)-1)
		if err != nil {
			if diskSpaceFailure(err) {
				if h.Recovered != nil {
					if ref, ok := h.Recovered(ctx); ok {
						return Result{Ref: ref, Recovered: true}, nil
					}
				}
				return Result{}, &DiskSpaceError{Err: err}
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				// The other sources fail the same way, and blaming this
				// registry would send the reader to the wrong place.
				if h.Failed != nil {
					h.Failed(c.Ref, err)
				}
				return Result{}, err
			}
			if ctx.Err() == nil && !registryAnswered(err) {
				p.prober.Cool(registryprobe.HostOf(c.Ref))
			}
			fail(c.Ref, err)
			if h.Recovered != nil {
				if ref, ok := h.Recovered(ctx); ok {
					return Result{Ref: ref, Recovered: true}, nil
				}
			}
			continue
		}
		if h.Pulled != nil {
			if err := h.Pulled(ctx, c.Ref); err != nil {
				fail(c.Ref, err)
				continue
			}
		}
		return Result{Ref: c.Ref}, nil
	}
	return Result{}, &AllFailedError{Failures: failures}
}

// cleaned drops empty and repeated references, keeping order.
func cleaned(cands []proto.ImagePullCandidate) []proto.ImagePullCandidate {
	out := make([]proto.ImagePullCandidate, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		c.Ref = strings.TrimSpace(c.Ref)
		if c.Ref == "" || seen[c.Ref] {
			continue
		}
		seen[c.Ref] = true
		out = append(out, c)
	}
	return out
}

func hasRankRun(cands []proto.ImagePullCandidate) bool {
	for i := 1; i < len(cands); i++ {
		if cands[i].Rank && cands[i-1].Rank {
			return true
		}
	}
	return false
}

// pullOne pulls one source, reporting through h while it runs and giving up on
// it if the download stops making headway, or, when another source remains
// (hasNext), crawls.
func (p *Puller) pullOne(ctx context.Context, driver runtime.Driver, ref string, opts Options, h Hooks, hasNext bool) error {
	if h.Attempt != nil {
		h.Attempt(ref)
	}
	progress := NewProgress()
	tctx, cancelTimeout := context.WithTimeout(ctx, CandidateTimeout)
	defer cancelTimeout()
	cctx, cancel := context.WithCancelCause(tctx)
	defer cancel(nil)

	done := make(chan struct{})
	watcherStopped := make(chan struct{})
	// stalledAfter is the window the watch gave up after; read only once
	// watcherStopped is closed.
	var stalledAfter time.Duration
	// crawlRate is the average rate the watch gave up on a crawl at.
	var crawlRate int64
	go func() {
		defer close(watcherStopped)
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		last, _ := progress.Fingerprint()
		lastChange := p.cfg.Now()
		crawl := crawlWatch{window: CrawlWindow, minRate: CrawlMinRate}
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			now := p.cfg.Now()
			if h.Heartbeat != nil {
				h.Heartbeat(ref, snapshotOf(progress, now))
			}
			fp, downloading := progress.Fingerprint()
			if hasNext {
				if rate, crawling := crawl.observe(now, fp.Done, downloading && !progress.Coarse()); crawling {
					crawlRate = rate
					cancel(ErrCrawling)
					return
				}
			}
			if !downloading || fp != last {
				last, lastChange = fp, now
				continue
			}
			if window := progress.StallWindow(StallTimeout, StallMinRate); now.Sub(lastChange) >= window {
				stalledAfter = window
				cancel(ErrStalled)
				return
			}
		}
	}()

	pullCtx := p.pullContext(cctx, driver, opts)
	err := driver.PullImage(pullCtx, ref, func(line string) {
		p.cfg.Log.Debug("pull", "ref", ref, "line", line)
		phase, doneBytes, total, rate, fraction, changed := progress.Observe(line, p.cfg.Now())
		if h.Line != nil {
			h.Line(ref, line, Snapshot{Message: strings.TrimSpace(line), Phase: phase, Done: doneBytes, Total: total, Rate: rate, Fraction: fraction}, changed)
		}
	})
	close(done)
	<-watcherStopped
	if err != nil && ctx.Err() == nil {
		switch cause := context.Cause(cctx); {
		case errors.Is(cause, ErrCrawling):
			return fmt.Errorf("%w: averaged %d KiB/s over %s, below %d KiB/s", ErrCrawling, crawlRate>>10, CrawlWindow, CrawlMinRate>>10)
		case errors.Is(cause, ErrStalled):
			return fmt.Errorf("%w for %s", ErrStalled, stalledAfter)
		}
	}
	return err
}

// snapshotOf is the progress a quiet runtime still owes the reader.
func snapshotOf(p *Progress, now time.Time) Snapshot {
	message, phase, done, total, rate, fraction := p.Snapshot(now)
	return Snapshot{Message: message, Phase: phase, Done: done, Total: total, Rate: rate, Fraction: fraction}
}

// pullContext carries to the runtime everything a pull needs beyond the
// reference: the registry logins and the proxy choice.
func (p *Puller) pullContext(ctx context.Context, driver runtime.Driver, opts Options) context.Context {
	// The instance is created from this image right after the pull, so
	// runtimes may prepare its rootfs while the image downloads.
	if opts.LaunchIntent {
		ctx = runtime.WithLaunchIntent(ctx)
	}
	if p.cfg.Auth != nil {
		ctx = runtime.WithRegistryAuth(ctx, p.cfg.Auth())
	}
	switch {
	case opts.LegacyProxy != nil:
		ctx = runtime.WithPullProxy(ctx, *opts.LegacyProxy)
	case opts.NodeProxy && !isDocker(driver):
		net := p.network()
		if proxy := nodeProxy(net); proxy != "" {
			ctx = runtime.WithPullNoProxy(runtime.WithPullProxy(ctx, proxy), net.NoProxy)
		}
	}
	return ctx
}

func (p *Puller) network() proto.NetworkConfig {
	if p.cfg.Network == nil {
		return proto.NetworkConfig{}
	}
	return p.cfg.Network()
}

func isDocker(driver runtime.Driver) bool { return driver != nil && driver.Name() == "docker" }

// nodeProxy is the single proxy a CLI pull is pointed at.
func nodeProxy(cfg proto.NetworkConfig) string {
	if proxy := strings.TrimSpace(cfg.HTTPSProxy); proxy != "" {
		return proxy
	}
	return strings.TrimSpace(cfg.HTTPProxy)
}

// registryAnswered reports a pull failure in which the registry answered: the
// tag or repository is not there, or the login is refused. That says nothing
// about how well the node reaches the registry, so its host is not cooled -
// cooling it would push a working registry behind slower ones for every
// other image.
func registryAnswered(err error) bool {
	return errors.Is(err, runtime.ErrRegistryRefused)
}
