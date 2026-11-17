package imagepull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// clock is a settable time source shared by the puller and the test.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Unix(5_000_000, 0).UnixNano())
	return c
}
func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// pullStep is what the fake runtime does for one reference.
type pullStep func(ctx context.Context, onLine func(string)) error

type fakeDriver struct {
	runtime.Driver // unused methods panic: a walk must only pull
	name           string

	mu     sync.Mutex
	steps  map[string]pullStep
	pulled []string
	ctxs   map[string]context.Context
}

func newDriver(name string, steps map[string]pullStep) *fakeDriver {
	return &fakeDriver{name: name, steps: steps, ctxs: map[string]context.Context{}}
}

func (d *fakeDriver) Name() string { return d.name }

func (d *fakeDriver) PullImage(ctx context.Context, ref string, onLine func(string)) error {
	d.mu.Lock()
	d.pulled = append(d.pulled, ref)
	d.ctxs[ref] = ctx
	step := d.steps[ref]
	d.mu.Unlock()
	if step == nil {
		return failCLI("Error response from daemon: manifest unknown: "+ref)(ctx, onLine)
	}
	return step(ctx, onLine)
}

func (d *fakeDriver) order() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.pulled...)
}

func ok(_ context.Context, onLine func(string)) error {
	onLine("done")
	return nil
}

func fail(msg string) pullStep {
	return func(context.Context, func(string)) error { return errors.New(msg) }
}

// failCLI fails the way the docker and nerdctl drivers do: the CLI output in
// the error text, marked with what runtime.PullOutputFailure reads from it.
func failCLI(output string) pullStep {
	return func(context.Context, func(string)) error {
		err := fmt.Errorf("docker pull: exit status 1: %s", output)
		return runtime.MarkPullFailure(err, runtime.PullOutputFailure(output))
	}
}

// shortTimings makes the stall watch tick every few milliseconds of real time;
// what counts as 3 minutes is the fake clock's business.
func shortTimings(t *testing.T) {
	t.Helper()
	oldStall, oldBeat := StallTimeout, HeartbeatInterval
	HeartbeatInterval = 2 * time.Millisecond
	t.Cleanup(func() { StallTimeout, HeartbeatInterval = oldStall, oldBeat })
}

func cands(refs ...string) []proto.ImagePullCandidate {
	out := make([]proto.ImagePullCandidate, len(refs))
	for i, r := range refs {
		out[i] = proto.ImagePullCandidate{Ref: r}
	}
	return out
}

func ranked(refs ...string) []proto.ImagePullCandidate {
	out := cands(refs...)
	for i := range out {
		out[i].Rank = true
	}
	return out
}

// probeNet answers /v2/ probes: hosts in down refuse, the rest answer after
// their delay on the fake clock, one probe at a time.
type probeNet struct {
	mu    sync.Mutex
	down  map[string]bool
	seen  []registryprobe.Route
	calls map[string]int
}

func newPuller(t *testing.T, c *clock, down map[string]bool, cfg Config) (*Puller, *probeNet) {
	t.Helper()
	pn := &probeNet{down: down, calls: map[string]int{}}
	cfg.Prober = registryprobe.New(registryprobe.WithClock(c.Now), registryprobe.WithTimeout(time.Second), registryprobe.WithTransport(func(r registryprobe.Route) http.RoundTripper {
		pn.mu.Lock()
		pn.seen = append(pn.seen, r)
		pn.mu.Unlock()
		return pn
	}))
	cfg.Now = c.Now
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg), pn
}

func (pn *probeNet) RoundTrip(req *http.Request) (*http.Response, error) {
	pn.mu.Lock()
	pn.calls[req.URL.Host]++
	down := pn.down[req.URL.Host]
	pn.mu.Unlock()
	if down {
		return nil, errors.New("connection refused")
	}
	return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

func TestWalkFallsBackAndCoolsTheFailedHost(t *testing.T) {
	c := newClock()
	p, _ := newPuller(t, c, nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": fail("dial tcp 10.0.0.1:443: i/o timeout"), "b.example/x:1": ok, "c.example/x:1": ok})
	var attempts, failed []string
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1", "c.example/x:1"), Options{}, Hooks{
		Attempt: func(ref string) { attempts = append(attempts, ref) },
		Failed:  func(ref string, err error) { failed = append(failed, ref+"|"+err.Error()) },
	})
	if err != nil || res.Ref != "b.example/x:1" || res.Recovered {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := strings.Join(d.order(), ","); got != "a.example/x:1,b.example/x:1" {
		t.Fatalf("pulled %s", got)
	}
	if got := strings.Join(attempts, ","); got != "a.example/x:1,b.example/x:1" {
		t.Fatalf("attempt hook saw %s", got)
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "i/o timeout") {
		t.Fatalf("failed hook saw %v", failed)
	}
	if !p.prober.Cooled("a.example") || p.prober.Cooled("b.example") {
		t.Fatal("exactly the failed registry must be cooling down")
	}
}

// A registry that answers - the tag is not there, the login is refused - is
// reachable: moving on is right, cooling its host is not, or one missing tag
// would push a working registry behind slower ones for every other image.
func TestWalkDoesNotCoolARegistryThatAnswered(t *testing.T) {
	for _, answer := range []string{
		`failed to resolve reference "quay.io/x:probe": quay.io/x:probe: not found`,
		"unknown: Tag probe was deleted or has expired",
		"manifest unknown",
		"unauthorized: authentication required",
		"denied: requested access to the resource is denied",
	} {
		c := newClock()
		p, _ := newPuller(t, c, nil, Config{})
		d := newDriver("udocker", map[string]pullStep{"a.example/x:1": failCLI(answer), "b.example/x:1": ok})
		res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
		if err != nil || res.Ref != "b.example/x:1" {
			t.Fatalf("%q: res=%+v err=%v", answer, res, err)
		}
		if p.prober.Cooled("a.example") {
			t.Fatalf("%q cooled a registry that answered", answer)
		}
	}
}

// The walk reads the driver's mark, never the error text: an unmarked
// failure that merely says "denied" is a transport failure as far as the
// walk knows, and a marked one still classifies after wrapping.
func TestWalkReadsTheRegistryRefusalMarkNotTheText(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": fail("denied"), "b.example/x:1": ok})
	if _, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if !p.prober.Cooled("a.example") {
		t.Fatal("an unmarked failure must cool the registry like any transport failure")
	}
	wrapped := fmt.Errorf("pull a.example/x:1: %w", runtime.MarkPullFailure(errors.New("401 Unauthorized"), runtime.ErrRegistryRefused))
	if !registryAnswered(wrapped) {
		t.Fatal("a wrapped refusal must still classify")
	}
}

func TestWalkAllSourcesFailing(t *testing.T) {
	c := newClock()
	p, _ := newPuller(t, c, nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": fail("timeout"), "b.example/x:1": fail("denied")})
	_, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
	var all *AllFailedError
	if !errors.As(err, &all) || len(all.Failures) != 2 {
		t.Fatalf("err = %v", err)
	}
	if got := err.Error(); got != "all image sources failed: a.example/x:1: timeout | b.example/x:1: denied" {
		t.Fatalf("message = %q", got)
	}
	// Empty and repeated references are not attempts.
	_, err = p.Walk(context.Background(), d, cands("", "  "), Options{}, Hooks{})
	if !errors.As(err, &all) || len(d.order()) != 2 {
		t.Fatalf("empty list: err=%v pulled=%v", err, d.order())
	}
}

func TestWalkDeduplicatesReferences(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": fail("nope")})
	_, _ = p.Walk(context.Background(), d, cands("a.example/x:1", " a.example/x:1 "), Options{}, Hooks{})
	if len(d.order()) != 1 {
		t.Fatalf("a repeated source was pulled twice: %v", d.order())
	}
}

func TestWalkRanksRankableRunsAndReportsOnce(t *testing.T) {
	c := newClock()
	p, pn := newPuller(t, c, map[string]bool{"down.example": true}, Config{})
	list := []proto.ImagePullCandidate{
		{Ref: "fixed.example/x:1"},
		{Ref: "down.example/x:1", Rank: true},
		{Ref: "up.example/x:1", Rank: true},
	}
	d := newDriver("udocker", map[string]pullStep{"fixed.example/x:1": fail("rule endpoint down"), "up.example/x:1": ok, "down.example/x:1": ok})
	var summaries []string
	res, err := p.Walk(context.Background(), d, list, Options{}, Hooks{Ordered: func(s string) { summaries = append(summaries, s) }})
	if err != nil || res.Ref != "up.example/x:1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := strings.Join(d.order(), ","); got != "fixed.example/x:1,up.example/x:1" {
		t.Fatalf("pull order = %s (the operator's rule endpoint stays first, the reachable copy goes before the unreachable one)", got)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0], "up.example (") || !strings.Contains(summaries[0], "down.example (unreachable)") {
		t.Fatalf("summaries = %v", summaries)
	}
	if strings.Index(summaries[0], "up.example") > strings.Index(summaries[0], "down.example") {
		t.Fatalf("summary does not list the chosen order: %q", summaries[0])
	}
	if pn.calls["fixed.example"] != 0 {
		t.Fatal("an unranked candidate was probed")
	}
}

func TestWalkRunOfOneIsNotProbedAndSilent(t *testing.T) {
	p, pn := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": ok})
	called := false
	_, err := p.Walk(context.Background(), d, []proto.ImagePullCandidate{{Ref: "a.example/x:1", Rank: true}}, Options{}, Hooks{Ordered: func(string) { called = true }})
	if err != nil || called || len(pn.calls) != 0 {
		t.Fatalf("err=%v ordered=%v probes=%v", err, called, pn.calls)
	}
}

func TestCooldownReordersTheNextWalk(t *testing.T) {
	c := newClock()
	p, _ := newPuller(t, c, nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": fail("EOF"), "b.example/x:1": ok})
	if _, err := p.Walk(context.Background(), d, ranked("a.example/x:1", "b.example/x:1"), Options{}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.order(), ","); got != "a.example/x:1,b.example/x:1" {
		t.Fatalf("first walk: %s", got)
	}
	// Both registries answer the probe, but a.example just failed: b goes first.
	d2 := newDriver("udocker", map[string]pullStep{"a.example/x:1": ok, "b.example/x:1": ok})
	if _, err := p.Walk(context.Background(), d2, ranked("a.example/x:1", "b.example/x:1"), Options{}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d2.order(), ","); got != "b.example/x:1" {
		t.Fatalf("second walk pulled %s, want the healthy registry first", got)
	}
	// After the cooldown a.example is eligible again, in its original place.
	c.Advance(registryprobe.CooldownTTL + time.Minute)
	d3 := newDriver("udocker", map[string]pullStep{"a.example/x:1": ok, "b.example/x:1": ok})
	if _, err := p.Walk(context.Background(), d3, ranked("a.example/x:1", "b.example/x:1"), Options{}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d3.order(), ","); got != "a.example/x:1" {
		t.Fatalf("third walk pulled %s", got)
	}
}

// stallingPull announces a download, then hangs until its context ends; the
// fake clock is moved by the test while it hangs.
func stallingPull(started chan<- struct{}) pullStep {
	return func(ctx context.Context, onLine func(string)) error {
		onLine("abcdef123456: Downloading 1MB/100MB")
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestWalkGivesUpOnADownloadThatStopsAndMovesOn(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	started := make(chan struct{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": stallingPull(started), "b.example/x:1": ok})
	type outcome struct {
		res Result
		err error
	}
	got := make(chan outcome, 1)
	var failedMsg atomic.Value
	// A heartbeat that already shows the downloaded bytes is the watch's own
	// tick after the line: the stall clock has started by then.
	var beats atomic.Int32
	go func() {
		res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
			Failed: func(ref string, err error) { failedMsg.Store(ref + ": " + err.Error()) },
			Heartbeat: func(ref string, s Snapshot) {
				if ref == "a.example/x:1" && s.Done > 0 {
					beats.Add(1)
				}
			},
		})
		got <- outcome{res, err}
	}()
	<-started
	for begin := time.Now(); beats.Load() < 2; time.Sleep(time.Millisecond) {
		if time.Since(begin) > 5*time.Second {
			t.Fatal("the stall watch never ticked")
		}
	}
	c.Advance(2*time.Minute + 59*time.Second)
	select {
	case o := <-got:
		t.Fatalf("gave up before 3 minutes without progress: %+v", o)
	case <-time.After(60 * time.Millisecond):
	}
	c.Advance(2 * time.Second)
	select {
	case o := <-got:
		if o.err != nil || o.res.Ref != "b.example/x:1" {
			t.Fatalf("outcome = %+v", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled download was never cancelled")
	}
	if msg, _ := failedMsg.Load().(string); msg != "a.example/x:1: no download progress for 3m0s" {
		t.Fatalf("failure = %q", msg)
	}
	if !p.prober.Cooled("a.example") {
		t.Fatal("a stalled host must cool down")
	}
}

func TestStallWatchIsResetByHeadway(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	// Five virtual minutes of steady download, each line two minutes apart on
	// a three-minute watch: bytes keep changing, so no stall.
	steady := func(ctx context.Context, onLine func(string)) error {
		for mb := 1; mb <= 10; mb++ {
			onLine(fmt.Sprintf("abcdef123456: Downloading %dMB/100MB", mb))
			time.Sleep(15 * time.Millisecond) // several watch ticks pass
			c.Advance(2 * time.Minute)
			time.Sleep(15 * time.Millisecond)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	}
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": steady})
	if _, err := p.Walk(context.Background(), d, cands("a.example/x:1"), Options{}, Hooks{}); err != nil {
		t.Fatalf("a download that keeps moving was cancelled: %v", err)
	}
	if p.prober.Cooled("a.example") {
		t.Fatal("a healthy host was cooled")
	}
}

// quietPull prints lines, lets the fake clock run an hour with the pull silent
// for the whole of it, then succeeds unless it was cancelled.
func quietPull(c *clock, lines ...string) pullStep {
	return func(ctx context.Context, onLine func(string)) error {
		for _, l := range lines {
			onLine(l)
		}
		time.Sleep(20 * time.Millisecond)
		c.Advance(time.Hour)
		time.Sleep(40 * time.Millisecond)
		return ctx.Err()
	}
}

func TestStallWatchNeverFiresWhereItCannotTell(t *testing.T) {
	for name, lines := range map[string][]string{
		"extracting is never timed out":                     {"abcdef123456: Downloading 100MB/100MB", "abcdef123456: Extracting 10MB/100MB"},
		"a runtime that prints no byte counters":            {"abcdef123456: Pulling fs layer", "abcdef123456: Waiting"},
		"every announced byte has arrived":                  {"abcdef123456: Downloading 100MB/100MB"},
		"the first progress line has not been observed yet": nil,
	} {
		t.Run(name, func(t *testing.T) {
			shortTimings(t)
			c := newClock()
			StallTimeout = 3 * time.Minute
			p, _ := newPuller(t, c, nil, Config{})
			d := newDriver("udocker", map[string]pullStep{"a.example/x:1": quietPull(c, lines...)})
			if _, err := p.Walk(context.Background(), d, cands("a.example/x:1"), Options{}, Hooks{}); err != nil {
				t.Fatalf("pull was cancelled: %v", err)
			}
		})
	}
}

func TestWalkAbortsWhenTheRuntimeCannotRun(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	unavailable := fmt.Errorf("docker: %w", runtime.ErrRuntimeUnavailable)
	d := newDriver("docker", map[string]pullStep{"a.example/x:1": func(context.Context, func(string)) error { return unavailable }, "b.example/x:1": ok})
	var failed int
	_, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{Failed: func(string, error) { failed++ }})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(d.order()) != 1 || failed != 1 {
		t.Fatalf("pulled=%v failed hook=%d", d.order(), failed)
	}
	if p.prober.Cooled("a.example") {
		t.Fatal("an unusable runtime must not blame the registry")
	}
}

func TestWalkPulledRefusalMovesOnWithoutCooling(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": ok, "b.example/x:1": ok})
	var finished []string
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Pulled: func(_ context.Context, ref string) error {
			finished = append(finished, ref)
			if ref == "a.example/x:1" {
				return errors.New("canonical image tag failed: denied")
			}
			return nil
		},
	})
	if err != nil || res.Ref != "b.example/x:1" || len(finished) != 2 {
		t.Fatalf("res=%+v err=%v finished=%v", res, err, finished)
	}
	if p.prober.Cooled("a.example") {
		t.Fatal("a tagging failure is not the registry's fault")
	}
	_, err = p.Walk(context.Background(), d, cands("a.example/x:1"), Options{}, Hooks{Pulled: func(context.Context, string) error { return errors.New("no tagger") }})
	var all *AllFailedError
	if !errors.As(err, &all) || all.Failures[0] != "a.example/x:1: no tagger" {
		t.Fatalf("err = %v", err)
	}
}

func TestWalkRecoveredImageEndsTheWalk(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{"b.example/x:1": ok})
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Recovered: func(context.Context) (string, bool) { return "a.example/x:1", true },
	})
	if err != nil || res.Ref != "a.example/x:1" || !res.Recovered || len(d.order()) != 1 {
		t.Fatalf("res=%+v err=%v pulled=%v", res, err, d.order())
	}
}

func TestWalkStopsOnCancellationWithoutCoolingRegistries(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	d := newDriver("udocker", map[string]pullStep{
		"a.example/x:1": func(ctx context.Context, _ func(string)) error { cancel(); <-ctx.Done(); return ctx.Err() },
		"b.example/x:1": ok,
	})
	_, err := p.Walk(ctx, d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
	var all *AllFailedError
	if !errors.As(err, &all) || len(d.order()) != 1 {
		t.Fatalf("err=%v pulled=%v", err, d.order())
	}
	if p.prober.Cooled("a.example") {
		t.Fatal("a cancelled pull must not cool the registry")
	}
}

func TestWalkHooksSeeProgressAndHeartbeats(t *testing.T) {
	shortTimings(t)
	p, _ := newPuller(t, newClock(), nil, Config{})
	slow := func(ctx context.Context, onLine func(string)) error {
		onLine("abcdef123456: Downloading 5MB/10MB")
		time.Sleep(30 * time.Millisecond)
		return nil
	}
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": slow})
	var mu sync.Mutex
	var lines []Snapshot
	var beats int
	_, err := p.Walk(context.Background(), d, cands("a.example/x:1"), Options{}, Hooks{
		Line: func(ref, line string, s Snapshot, changed bool) {
			mu.Lock()
			defer mu.Unlock()
			if ref != "a.example/x:1" || !changed {
				t.Errorf("ref=%q changed=%v", ref, changed)
			}
			lines = append(lines, s)
		},
		Heartbeat: func(ref string, s Snapshot) { mu.Lock(); beats++; mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0].Done != 5<<20 || lines[0].Total != 10<<20 || lines[0].Fraction != 0.5 || lines[0].Phase != proto.PhaseImagePull {
		t.Fatalf("line snapshots = %+v", lines)
	}
	if beats == 0 {
		t.Fatal("no heartbeat while the pull ran")
	}
}

func TestPullContextCarriesLoginsLaunchIntentAndProxyChoice(t *testing.T) {
	auth := []proto.SourceAuth{{Host: "a.example", Username: "u", Secret: "s"}}
	nodeNet := proto.NetworkConfig{HTTPProxy: "http://plain.proxy:3128", HTTPSProxy: "http://tls.proxy:3128", NoProxy: "x"}
	direct := ""
	legacy := "http://legacy.proxy:1"
	cases := []struct {
		name      string
		driver    string
		opts      Options
		net       proto.NetworkConfig
		wantProxy string
		explicit  bool // a pull proxy choice is attached
		intent    bool
	}{
		{"cli runtime, node proxy", "nerdctl", Options{NodeProxy: true, LaunchIntent: true}, nodeNet, "http://tls.proxy:3128", true, true},
		{"native runtime, http proxy only", "udocker", Options{NodeProxy: true}, proto.NetworkConfig{HTTPProxy: "http://plain.proxy:3128"}, "http://plain.proxy:3128", true, false},
		{"no node proxy attaches nothing", "udocker", Options{NodeProxy: true}, proto.NetworkConfig{}, "", false, false},
		{"docker never gets a pull proxy", "docker", Options{NodeProxy: true}, nodeNet, "", false, false},
		{"legacy proxy applies to docker too", "docker", Options{LegacyProxy: &legacy}, nodeNet, legacy, true, false},
		{"legacy direct choice", "udocker", Options{LegacyProxy: &direct}, nodeNet, "", true, false},
		{"legacy path ignores the node proxy", "udocker", Options{}, nodeNet, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newPuller(t, newClock(), nil, Config{Network: func() proto.NetworkConfig { return tc.net }, Auth: func() []proto.SourceAuth { return auth }})
			d := newDriver(tc.driver, map[string]pullStep{"a.example/x:1": ok})
			if _, err := p.Walk(context.Background(), d, cands("a.example/x:1"), tc.opts, Hooks{}); err != nil {
				t.Fatal(err)
			}
			ctx := d.ctxs["a.example/x:1"]
			if got, found := runtime.RegistryAuthFor(ctx, "a.example/x:1"); !found || got.Secret != "s" {
				t.Fatalf("login not handed to the runtime: %+v", got)
			}
			if _, found := runtime.RegistryAuthFor(ctx, "other.example/x:1"); found {
				t.Fatal("login matched another registry")
			}
			if runtime.PullProxy(ctx) != tc.wantProxy || (runtime.PullProxy(ctx) != "" || runtime.PullDirect(ctx)) != tc.explicit {
				t.Fatalf("proxy = %q direct=%v, want %q (attached=%v)", runtime.PullProxy(ctx), runtime.PullDirect(ctx), tc.wantProxy, tc.explicit)
			}
			if runtime.LaunchIntent(ctx) != tc.intent {
				t.Fatalf("launch intent = %v", runtime.LaunchIntent(ctx))
			}
		})
	}
}

// proxyOf answers which proxy a probe request for host would take.
func proxyOf(t *testing.T, r registryprobe.Route, host string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/v2/", nil)
	u, err := r.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func TestProbeRouteFollowsThePullPath(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env.proxy:9")
	t.Setenv("https_proxy", "http://env.proxy:9")
	nodeNet := proto.NetworkConfig{HTTPSProxy: "http://user:pw@node.proxy:3128", NoProxy: "internal.example"}
	daemon := func(h, s, n string, ok bool) func(context.Context) (string, string, string, bool) {
		return func(context.Context) (string, string, string, bool) { return h, s, n, ok }
	}
	cases := []struct {
		name   string
		driver string
		net    proto.NetworkConfig
		daemon func(context.Context) (string, string, string, bool)
		host   string
		want   string
	}{
		{"nerdctl uses the node proxy", "nerdctl", nodeNet, nil, "quay.io", "http://user:pw@node.proxy:3128"},
		{"nerdctl honours the node's no_proxy", "nerdctl", nodeNet, nil, "internal.example", ""},
		{"no node proxy falls back to the agent environment", "udocker", proto.NetworkConfig{}, nil, "quay.io", "http://env.proxy:9"},
		{"docker uses the daemon's proxy, not the agent environment", "docker", proto.NetworkConfig{}, daemon("http://d.proxy:1", "http://d.proxy:1", "", true), "quay.io", "http://d.proxy:1"},
		{"docker daemon masked credentials come from the node config", "docker", proto.NetworkConfig{HTTPSProxy: "http://user:pw@d.proxy:1"}, daemon("http://xxxxx:xxxxx@d.proxy:1", "http://xxxxx:xxxxx@d.proxy:1", "", true), "quay.io", "http://user:pw@d.proxy:1"},
		{"docker daemon masked credentials with nothing to restore send none", "docker", proto.NetworkConfig{}, daemon("http://xxxxx:xxxxx@d.proxy:1", "http://xxxxx:xxxxx@d.proxy:1", "", true), "quay.io", "http://d.proxy:1"},
		{"docker daemon with no proxy goes direct despite the environment", "docker", nodeNet, daemon("", "", "", true), "quay.io", ""},
		{"docker daemon honours its own no_proxy", "docker", proto.NetworkConfig{}, daemon("http://d.proxy:1", "http://d.proxy:1", "quay.io", true), "quay.io", ""},
		{"docker daemon unobservable falls back to the node config", "docker", nodeNet, daemon("", "", "", false), "quay.io", "http://user:pw@node.proxy:3128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			p, pn := newPuller(t, c, nil, Config{Network: func() proto.NetworkConfig { return tc.net }, DaemonProxy: tc.daemon})
			d := newDriver(tc.driver, map[string]pullStep{"quay.io/x:1": ok, "internal.example/x:1": ok})
			if _, err := p.Walk(context.Background(), d, ranked("quay.io/x:1", "internal.example/x:1"), Options{}, Hooks{}); err != nil {
				t.Fatal(err)
			}
			if len(pn.seen) != 1 {
				t.Fatalf("probe routes = %d", len(pn.seen))
			}
			if got := proxyOf(t, pn.seen[0], tc.host); got != tc.want {
				t.Fatalf("probe to %s goes through %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestUnmaskedLeavesPlainValuesAlone(t *testing.T) {
	net := proto.NetworkConfig{HTTPSProxy: "http://user:pw@d.proxy:1"}
	for in, want := range map[string]string{
		"":                            "",
		"d.proxy:1":                   "d.proxy:1",
		"http://real:creds@d.proxy:1": "http://real:creds@d.proxy:1",
		"http://xxxxx:xxxxx@other:1":  "http://other:1",
	} {
		if got := unmasked(in, net); got != want {
			t.Errorf("unmasked(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWalkStopsWhenPullExhaustsLocalDisk(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("docker", map[string]pullStep{
		"a.example/x:1": failCLI("write /var/lib/docker/tmp/blob: no space left on device"),
		"b.example/x:1": ok,
	})
	failedCalls := 0
	_, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Failed: func(string, error) { failedCalls++ },
	})
	var diskErr *DiskSpaceError
	if !errors.As(err, &diskErr) {
		t.Fatalf("error = %v, want DiskSpaceError", err)
	}
	if !strings.Contains(err.Error(), "insufficient disk space") || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("error should explain local storage exhaustion: %v", err)
	}
	if got := d.order(); len(got) != 1 || got[0] != "a.example/x:1" {
		t.Fatalf("pull attempts = %v, want only the first source", got)
	}
	if failedCalls != 0 {
		t.Fatalf("source failure hook called %d times for a node storage failure", failedCalls)
	}
}

// A native puller's write fails with the operating system's ENOSPC; the walk
// knows it by type. Text that only says so is not a disk failure.
func TestDiskSpaceFailureIsKnownByTypeNotText(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	enospc := &os.PathError{Op: "write", Path: "/var/lib/gpuworks/udocker/layers/sha256:1", Err: syscall.ENOSPC}
	d := newDriver("udocker", map[string]pullStep{"a.example/x:1": func(context.Context, func(string)) error {
		return fmt.Errorf("pull a.example/x:1: %w", enospc)
	}})
	_, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
	var diskErr *DiskSpaceError
	if !errors.As(err, &diskErr) || len(d.order()) != 1 {
		t.Fatalf("error=%v pulls=%v, want disk error and no fallback", err, d.order())
	}
	d = newDriver("docker", map[string]pullStep{"a.example/x:1": fail("write failed: INSUFFICIENT DISK SPACE"), "b.example/x:1": ok})
	if _, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{}); err != nil || len(d.order()) != 2 {
		t.Fatalf("error=%v pulls=%v, want unmarked text treated as a source failure", err, d.order())
	}
}

func TestWalkAcceptsAnImageRecoveredAfterDiskSpaceFailure(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("docker", map[string]pullStep{"a.example/x:1": failCLI("write failed: no space left on device")})
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Recovered: func(context.Context) (string, bool) { return "a.example/x:1", true },
	})
	if err != nil || !res.Recovered || res.Ref != "a.example/x:1" {
		t.Fatalf("result=%+v error=%v, want the already-available image", res, err)
	}
	if got := d.order(); len(got) != 1 {
		t.Fatalf("pull attempts = %v, want no fallback after recovery", got)
	}
}
