package registryprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// behavior is how a fake registry host answers a /v2/ request.
type behavior struct {
	status int           // HTTP status; 0 with err set means a transport failure
	delay  time.Duration // virtual latency, advanced on the fake clock
	block  bool          // never answer: wait for the request's context
	err    error
}

type fakeNet struct {
	clock *fakeClock
	mu    sync.Mutex
	hosts map[string]behavior
	calls map[string]int
	urls  []string
	route []Route
}

func newFakeNet(hosts map[string]behavior) (*fakeNet, *Prober) {
	n := &fakeNet{clock: &fakeClock{t: time.Unix(1_000_000, 0)}, hosts: hosts, calls: map[string]int{}}
	p := New(WithClock(n.clock.Now), WithTimeout(50*time.Millisecond), WithTransport(func(r Route) http.RoundTripper {
		n.mu.Lock()
		n.route = append(n.route, r)
		n.mu.Unlock()
		return n
	}))
	return n, p
}

func (n *fakeNet) RoundTrip(req *http.Request) (*http.Response, error) {
	n.mu.Lock()
	b, ok := n.hosts[req.URL.Host]
	n.calls[req.URL.Host]++
	n.urls = append(n.urls, req.URL.String())
	n.mu.Unlock()
	if !ok {
		return nil, errors.New("no such host")
	}
	if b.block {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	n.clock.Advance(b.delay)
	if b.err != nil {
		return nil, b.err
	}
	return &http.Response{StatusCode: b.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

func (n *fakeNet) callCount(host string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[host]
}

// warm probes hosts one after another, so each latency is its own delay.
func warm(p *Prober, route Route, hosts ...string) {
	for _, h := range hosts {
		p.Probe(context.Background(), []string{h}, route)
	}
}

func ranked(refs ...string) []proto.ImagePullCandidate {
	out := make([]proto.ImagePullCandidate, len(refs))
	for i, r := range refs {
		out[i] = proto.ImagePullCandidate{Ref: r, Rank: true}
	}
	return out
}

func refsOf(cands []proto.ImagePullCandidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Ref
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"ubuntu:22.04":                      "docker.io",
		"library/ubuntu":                    "docker.io",
		"gpuworks/runner:tag":               "docker.io",
		"docker.io/gpuworks/runner:tag":     "docker.io",
		"index.docker.io/x/y":               "docker.io",
		"registry-1.docker.io/x/y@sha256:a": "docker.io",
		"Quay.IO/zstackai/x:1":              "quay.io",
		"localhost/x":                       "localhost",
		"localhost:5000/x":                  "localhost:5000",
		"10.0.0.1:5000/a/b":                 "10.0.0.1:5000",
	}
	for ref, want := range cases {
		if got := HostOf(ref); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestRankOrdersByReachabilityThenLatency(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{
		"slow.example":         {status: 401, delay: 210 * time.Millisecond},
		"fast.example":         {status: 200, delay: 35 * time.Millisecond},
		"gone.example":         {err: errors.New("dial tcp: i/o timeout")},
		"registry-1.docker.io": {status: 404, delay: 90 * time.Millisecond},
		"never.example":        {block: true},
	})
	// Latency is measured against one shared virtual clock, so the hosts are
	// probed one at a time here and Rank then reads the cached answers.
	warm(p, Route{Key: "k"}, "gone.example", "slow.example", "docker.io", "fast.example", "never.example")
	in := ranked("gone.example/a:1", "slow.example/a:1", "docker.io/a:1", "fast.example/a:1", "never.example/a:1")
	out, report := p.Rank(context.Background(), in, Route{Key: "k"})
	want := []string{"fast.example/a:1", "docker.io/a:1", "slow.example/a:1", "gone.example/a:1", "never.example/a:1"}
	if !equal(refsOf(out), want) {
		t.Fatalf("order = %v, want %v", refsOf(out), want)
	}
	if got := refsOf(in); got[0] != "gone.example/a:1" {
		t.Fatalf("input slice was reordered: %v", got)
	}
	// Any HTTP answer, including 401 and 404, counts as reachable; Docker Hub
	// is probed on its API host.
	if n.callCount("registry-1.docker.io") != 1 || n.callCount("docker.io") != 0 {
		t.Fatalf("docker hub probe went to %v", n.calls)
	}
	for _, u := range n.urls {
		if !strings.HasPrefix(u, "https://") || !strings.HasSuffix(u, "/v2/") {
			t.Fatalf("probe url %q is not GET https://<host>/v2/", u)
		}
	}
	summary := report.String()
	for _, frag := range []string{"fast.example (35 ms)", "docker.io (90 ms)", "slow.example (210 ms)", "gone.example (unreachable)", "never.example (unreachable)"} {
		if !strings.Contains(summary, frag) {
			t.Errorf("report %q lacks %q", summary, frag)
		}
	}
}

func TestRankKeepsUnrankedPositionsAndSortsRunsSeparately(t *testing.T) {
	_, p := newFakeNet(map[string]behavior{
		"a.example": {status: 200, delay: 300 * time.Millisecond},
		"b.example": {status: 200, delay: 10 * time.Millisecond},
		"c.example": {status: 200, delay: 200 * time.Millisecond},
		"d.example": {status: 200, delay: 5 * time.Millisecond},
		"e.example": {status: 200, delay: 1 * time.Millisecond},
	})
	warm(p, Route{}, "a.example", "b.example", "c.example", "d.example", "e.example")
	in := []proto.ImagePullCandidate{
		{Ref: "a.example/x:1", Rank: true},
		{Ref: "b.example/x:1", Rank: true},
		{Ref: "pinned.example/x:1"},
		{Ref: "c.example/x:1", Rank: true},
		{Ref: "d.example/x:1", Rank: true},
		{Ref: "e.example/x:1", Rank: false},
	}
	out, _ := p.Rank(context.Background(), in, Route{})
	want := []string{"b.example/x:1", "a.example/x:1", "pinned.example/x:1", "d.example/x:1", "c.example/x:1", "e.example/x:1"}
	if !equal(refsOf(out), want) {
		t.Fatalf("order = %v, want %v", refsOf(out), want)
	}
	for i := range out {
		if out[i].Rank != in[map[string]int{"a.example/x:1": 0, "b.example/x:1": 1, "pinned.example/x:1": 2, "c.example/x:1": 3, "d.example/x:1": 4, "e.example/x:1": 5}[out[i].Ref]].Rank {
			t.Fatalf("Rank flag detached from %s", out[i].Ref)
		}
	}
}

func TestRankSingleCandidateRunIsNotProbed(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{"a.example": {status: 200}, "b.example": {status: 200}})
	in := []proto.ImagePullCandidate{
		{Ref: "a.example/x:1", Rank: true},
		{Ref: "fixed.example/x:1"},
		{Ref: "b.example/x:1", Rank: true},
	}
	out, report := p.Rank(context.Background(), in, Route{})
	if !equal(refsOf(out), refsOf(in)) {
		t.Fatalf("order changed: %v", refsOf(out))
	}
	if len(n.calls) != 0 || len(report.Hosts) != 0 || report.String() != "" {
		t.Fatalf("a run of one was probed: calls=%v report=%q", n.calls, report.String())
	}
}

func TestProbeTimeoutDoesNotWaitForAHungHost(t *testing.T) {
	_, p := newFakeNet(map[string]behavior{"hung.example": {block: true}, "ok.example": {status: 200, delay: time.Millisecond}})
	start := time.Now()
	out, _ := p.Rank(context.Background(), ranked("hung.example/x:1", "ok.example/x:1"), Route{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v despite the 50ms timeout", elapsed)
	}
	if out[0].Ref != "ok.example/x:1" {
		t.Fatalf("hung host was not ranked last: %v", refsOf(out))
	}
}

func TestProbeIsParallel(t *testing.T) {
	// Four hosts that each hang for the whole timeout finish in about one
	// timeout, not four.
	hosts := map[string]behavior{}
	var refs []string
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		hosts[h+".example"] = behavior{block: true}
		refs = append(refs, h+".example/x:1")
	}
	_, p := newFakeNet(hosts)
	start := time.Now()
	p.Rank(context.Background(), ranked(refs...), Route{})
	if elapsed := time.Since(start); elapsed >= 4*50*time.Millisecond {
		t.Fatalf("probes ran serially: %v", elapsed)
	}
}

func TestProbeCachesResultsPerHostForThirtyMinutes(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{"a.example": {status: 200, delay: 10 * time.Millisecond}, "b.example": {status: 200, delay: 20 * time.Millisecond}})
	cands := ranked("a.example/x:1", "b.example/x:1")
	p.Rank(context.Background(), cands, Route{Key: "r"})
	p.Rank(context.Background(), cands, Route{Key: "r"})
	if n.callCount("a.example") != 1 || n.callCount("b.example") != 1 {
		t.Fatalf("second ranking re-probed cached hosts: %v", n.calls)
	}
	// A different host is probed on its own; the cached ones are not.
	n.hosts["c.example"] = behavior{status: 200}
	p.Rank(context.Background(), ranked("a.example/x:1", "c.example/x:1"), Route{Key: "r"})
	if n.callCount("a.example") != 1 || n.callCount("c.example") != 1 {
		t.Fatalf("only the new host should be probed: %v", n.calls)
	}
	// A different route is a different measurement.
	p.Rank(context.Background(), cands, Route{Key: "other"})
	if n.callCount("a.example") != 2 {
		t.Fatalf("a changed route must not reuse the old result: %v", n.calls)
	}
	n.clock.Advance(29 * time.Minute)
	p.Rank(context.Background(), cands, Route{Key: "r"})
	if n.callCount("b.example") != 2 { // b was re-probed once by the "other" route
		t.Fatalf("result expired early: %v", n.calls)
	}
	n.clock.Advance(2 * time.Minute)
	before := n.callCount("b.example")
	p.Rank(context.Background(), cands, Route{Key: "r"})
	if n.callCount("b.example") != before+1 {
		t.Fatalf("result older than 30m was reused: %v", n.calls)
	}
}

func TestCancelledProbeIsNotCached(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{"a.example": {block: true}, "b.example": {block: true}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Rank(ctx, ranked("a.example/x:1", "b.example/x:1"), Route{})
	n.hosts["a.example"] = behavior{status: 200}
	n.hosts["b.example"] = behavior{status: 200}
	_, report := p.Rank(context.Background(), ranked("a.example/x:1", "b.example/x:1"), Route{})
	for _, h := range report.Hosts {
		if !h.Reachable {
			t.Fatalf("a cancelled probe poisoned the cache: %+v", report.Hosts)
		}
	}
}

func TestCooledHostsSortLastButAreKept(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{
		"fast.example": {status: 200, delay: 1 * time.Millisecond},
		"mid.example":  {status: 200, delay: 50 * time.Millisecond},
		"down.example": {err: errors.New("down")},
	})
	cands := ranked("fast.example/x:1", "mid.example/x:1", "down.example/x:1")
	p.Cool("fast.example")
	if !p.Cooled("FAST.example") {
		t.Fatal("cooldown must be case-insensitive")
	}
	out, report := p.Rank(context.Background(), cands, Route{})
	if want := []string{"mid.example/x:1", "down.example/x:1", "fast.example/x:1"}; !equal(refsOf(out), want) {
		t.Fatalf("order = %v, want %v", refsOf(out), want)
	}
	if !strings.Contains(report.String(), "fast.example (cooling down)") {
		t.Fatalf("report does not mention the cooldown: %q", report.String())
	}
	n.clock.Advance(CooldownTTL - time.Second)
	if !p.Cooled("fast.example") {
		t.Fatal("cooldown ended early")
	}
	n.clock.Advance(2 * time.Second)
	if p.Cooled("fast.example") {
		t.Fatal("cooldown outlived 30 minutes")
	}
	// Results expired alongside the cooldown. Sample virtual latencies one
	// host at a time: concurrent probes advance the same fake clock and can
	// otherwise charge one host for another host's simulated delay.
	warm(p, Route{}, "fast.example", "mid.example", "down.example")
	out, _ = p.Rank(context.Background(), cands, Route{})
	if out[0].Ref != "fast.example/x:1" {
		t.Fatalf("host did not recover after cooldown: %v", refsOf(out))
	}
}

func TestCooldownSharedAcrossDockerHubSpellings(t *testing.T) {
	_, p := newFakeNet(nil)
	p.Cool("registry-1.docker.io")
	if !p.Cooled("docker.io") || !p.Cooled("index.docker.io") {
		t.Fatal("Docker Hub spellings must share one cooldown")
	}
}

func TestPlainHTTPRegistryAnswersCountAsReachable(t *testing.T) {
	_, p := newFakeNet(map[string]behavior{
		// What crypto/tls returns when the server answers the handshake with
		// "HTTP/1.1 400 ..."; net/http.Client turns it into ErrSchemeMismatch.
		"plain.example": {err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake", RecordHeader: [5]byte{'H', 'T', 'T', 'P', '/'}}},
		"down.example":  {err: errors.New("refused")},
	})
	out, _ := p.Rank(context.Background(), ranked("down.example/x:1", "plain.example/x:1"), Route{})
	if out[0].Ref != "plain.example/x:1" {
		t.Fatalf("plain-HTTP registry treated as down: %v", refsOf(out))
	}
}

func TestRouteForHonoursNoProxyAndFallsBackToEnvironment(t *testing.T) {
	req := func(raw string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, raw, nil)
		return r
	}
	r := RouteFor("", "http://proxy.corp:3128", "registry.internal,.mirror.corp,10.0.0.0/8,other.example:8443")
	for raw, wantProxy := range map[string]bool{
		"https://quay.io/v2/":           true,
		"https://registry.internal/v2/": false,
		"https://a.mirror.corp/v2/":     false,
		// A leading dot names subdomains only, as Go reads NO_PROXY.
		"https://mirror.corp/v2/":        true,
		"https://10.4.5.6/v2/":           false,
		"https://11.4.5.6/v2/":           true,
		"https://other.example:8443/v2/": false,
		"https://other.example:443/v2/":  true,
		"https://localhost/v2/":          false,
		"http://127.0.0.1:5000/v2/":      false,
	} {
		got, err := r.Proxy(req(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if (got != nil) != wantProxy {
			t.Errorf("%s: proxy=%v, want proxied=%v", raw, got, wantProxy)
		}
		if got != nil && got.Host != "proxy.corp:3128" {
			t.Errorf("%s: wrong proxy %v", raw, got)
		}
	}
	if RouteFor("", "", "x").Key != "env" {
		t.Fatal("no configured proxy must fall back to the environment route")
	}
	t.Setenv("HTTPS_PROXY", "http://env-proxy:1")
	t.Setenv("https_proxy", "http://env-proxy:1")
	got, _ := RouteFor("", "", "").Proxy(req("https://quay.io/v2/"))
	if got == nil || got.Host != "env-proxy:1" {
		t.Fatalf("environment proxy not used: %v", got)
	}
	if _, err := RouteFor("", "ftp://bad", "").Proxy(req("https://quay.io/v2/")); err == nil {
		t.Fatal("an unsupported proxy scheme must be an error, not a silent direct connection")
	}
}

func TestDefaultTransportUsesRoute(t *testing.T) {
	called := false
	rt := defaultTransport(Route{Proxy: func(*http.Request) (*url.URL, error) { called = true; return nil, nil }})
	tr, ok := rt.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("transport has no proxy func: %T", rt)
	}
	r, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	_, _ = tr.Proxy(r)
	if !called {
		t.Fatal("route proxy func not wired into transport")
	}
}
