package modelfetch

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// throttledHub answers chosen requests of a hub with 429, the way
// huggingface.co answers a client over its quota: on the API (the tree
// listing) and on /resolve/ URLs, which every probe and range request goes
// through before the redirect; the CDN behind them is not rate limited
// (https://huggingface.co/docs/hub/rate-limits). Its headers are the ones
// huggingface.co sent on 2026-11-13:
//
//	RateLimit: "resolvers";r=2999;t=110
//	RateLimit-Policy: "fixed window";"resolvers";q=3000;w=300
type throttledHub struct {
	*hub
	mu sync.Mutex
	// listings, resolves and ranges are the 429s still to give: on the tree
	// listing, on any resolve request, and on resolve requests for a range
	// other than the size probe.
	listings, resolves, ranges int
	// header is what a 429 carries besides the policy.
	header http.Header
	served int // 429s given
}

func newThrottledHub(t *testing.T, files map[string][]byte) *throttledHub {
	t.Helper()
	h := &throttledHub{hub: newHub(t, files), header: http.Header{}}
	h.Config.Handler = http.HandlerFunc(h.serve)
	return h
}

func (h *throttledHub) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("RateLimit-Policy", `"fixed window";"resolvers";q=3000;w=300`)
	h.mu.Lock()
	throttle := false
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/models/") && h.listings > 0:
		h.listings--
		throttle = true
	case strings.Contains(r.URL.Path, "/resolve/") && h.resolves > 0:
		h.resolves--
		throttle = true
	case strings.Contains(r.URL.Path, "/resolve/") && r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-0" && h.ranges > 0:
		h.ranges--
		throttle = true
	}
	if throttle {
		h.served++
	}
	h.mu.Unlock()
	if !throttle {
		w.Header().Set("RateLimit", `"resolvers";r=2999;t=110`)
		h.hub.serve(w, r)
		return
	}
	for key, values := range h.header {
		w.Header()[key] = values
	}
	http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
}

func (h *throttledHub) given() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.served
}

// sleeps records the waits a fetch asks for instead of spending them.
func sleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	old := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = old })
	return &waits
}

// The only address there is asks for 110 seconds: the download waits that
// long and asks the same address again, instead of ending.
func TestARateLimitedResolveIsWaitedOutAtTheSameAddress(t *testing.T) {
	waits := sleeps(t)
	files := modelFiles()
	h := newThrottledHub(t, files)
	h.resolves = 1
	h.header.Set("RateLimit", `"resolvers";r=0;t=110`)
	r, dir := &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, &cooler{}, r, h.URL), commit, dir); err != nil {
		t.Fatalf("a single 429 ended the download: %v", err)
	}
	assertFiles(t, dir, files)
	if h.given() != 1 {
		t.Fatalf("the hub gave %d 429s, want the one under test", h.given())
	}
	if len(*waits) != 1 || (*waits)[0] != 111*time.Second {
		t.Fatalf("waits = %v, want the 110 s the RateLimit header named and a second more", *waits)
	}
	if len(r.switches) != 0 {
		t.Fatalf("switches = %+v; there is nowhere to switch to", r.switches)
	}
}

// The wait is the one the answer names, never shorter than the backoff and
// never longer than MaxThrottleWait.
func TestAThrottledWaitIsTheNamedOneWithinBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		header  http.Header
		backoff time.Duration
		want    time.Duration
	}{
		"retry-after seconds":        {header: http.Header{"Retry-After": {"7"}}, want: 7 * time.Second},
		"ratelimit reset":            {header: http.Header{"Ratelimit": {`"resolvers";r=0;t=40`}}, want: 41 * time.Second},
		"retry-after over ratelimit": {header: http.Header{"Retry-After": {"3"}, "Ratelimit": {`"resolvers";r=0;t=40`}}, want: 3 * time.Second},
		"no hint keeps the backoff":  {header: http.Header{}, backoff: 2 * time.Second, want: 2 * time.Second},
		"a hint below the backoff":   {header: http.Header{"Retry-After": {"1"}}, backoff: 2 * time.Second, want: 2 * time.Second},
		"an hour is bounded":         {header: http.Header{"Retry-After": {"3600"}}, want: MaxThrottleWait},
		// A quota exhausted at the start of a five-minute window is waited
		// out past its reset, not cut a second short of it.
		"a whole window": {header: http.Header{"Ratelimit": {`"resolvers";r=0;t=300`}}, want: 301 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			waits := sleeps(t)
			files := modelFiles()
			h := newThrottledHub(t, files)
			h.resolves, h.header = 1, tc.header
			f := newFetcher(t, &cooler{}, &run{}, h.URL)
			f.cfg.Retry.InitialBackoff = tc.backoff
			if err := fetchAll(t, f, commit, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if len(*waits) != 1 || (*waits)[0] != tc.want {
				t.Fatalf("waits = %v, want [%s]", *waits, tc.want)
			}
		})
	}
}

// A Retry-After given as a date is the time until then.
func TestARetryAfterDateIsTheTimeUntilThen(t *testing.T) {
	waits := sleeps(t)
	files := modelFiles()
	h := newThrottledHub(t, files)
	h.resolves = 1
	h.header.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if err := fetchAll(t, newFetcher(t, &cooler{}, &run{}, h.URL), commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] < 25*time.Second || (*waits)[0] > 30*time.Second {
		t.Fatalf("waits = %v, want about 30 s", *waits)
	}
}

// A 429 on one range of a ranged transfer, after the size probe succeeded,
// is waited out like one on the probe.
func TestAThrottledRangeDoesNotEndTheTransfer(t *testing.T) {
	waits := sleeps(t)
	files := modelFiles()
	h := newThrottledHub(t, files)
	h.ranges = 1
	h.header.Set("Retry-After", "5")
	f := newFetcher(t, &cooler{}, &run{}, h.URL)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dest := filepath.Join(dir, "model.safetensors")
	if err := f.Fetch(ctx, File{Path: "model.safetensors", Revision: commit, Dest: dest, Concurrency: 4}, nil); err != nil {
		t.Fatalf("a 429 on one range ended the download: %v", err)
	}
	assertFiles(t, dir, map[string][]byte{"model.safetensors": files["model.safetensors"]})
	if h.given() != 1 {
		t.Fatalf("the hub gave %d 429s, want one on a range", h.given())
	}
	if len(*waits) != 1 || (*waits)[0] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s]", *waits)
	}
}

// Throttling that does not end still ends the download, at the attempts the
// address has, and the failure names it.
func TestEndlessThrottlingEndsAtTheAttemptBudget(t *testing.T) {
	waits := sleeps(t)
	h := newThrottledHub(t, modelFiles())
	h.resolves = 1 << 20
	h.header.Set("Retry-After", "1")
	err := fetchAll(t, newFetcher(t, &cooler{}, &run{}, h.URL), commit, t.TempDir())
	if err == nil {
		t.Fatal("a source that never stops throttling was waited for without end")
	}
	for _, want := range []string{"failed 3 times", "status 429"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if len(*waits) != 2 {
		t.Fatalf("waits = %v, want one before each of the two retries", *waits)
	}
}

// A mirror that asks for time is asked again rather than abandoned for the
// next address at once.
func TestAThrottledMirrorIsAskedAgainBeforeSwitching(t *testing.T) {
	sleeps(t)
	files := modelFiles()
	mirror, upstream := newThrottledHub(t, files), newHub(t, files)
	mirror.resolves = 1
	mirror.header.Set("Retry-After", "2")
	r, dir := &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, &cooler{}, r, mirror.URL, upstream.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if len(r.switches) != 0 {
		t.Fatalf("switches = %+v: a throttled mirror was abandoned", r.switches)
	}
	if got := upstream.startsOf("model.safetensors"); len(got) != 0 {
		t.Fatalf("the upstream served %v; the mirror should have", got)
	}
}

// The listing asks for time too: it is waited for at the same address.
func TestARateLimitedListingIsWaitedOutAtTheSameAddress(t *testing.T) {
	waits := sleeps(t)
	files := modelFiles()
	h := newThrottledHub(t, files)
	h.listings = 1
	h.header.Set("RateLimit", `"api";r=0;t=40`)
	r, dir := &run{}, t.TempDir()
	f := newFetcher(t, &cooler{}, r, h.URL)
	var retried []string
	f.cfg.OnRetry = func(file string, _ int, _ time.Duration, _ error) { retried = append(retried, file) }
	if err := fetchAll(t, f, commit, dir); err != nil {
		t.Fatalf("a throttled listing ended the download: %v", err)
	}
	assertFiles(t, dir, files)
	if len(*waits) != 1 || (*waits)[0] != 41*time.Second || len(r.switches) != 0 {
		t.Fatalf("waits = %v, switches = %+v", *waits, r.switches)
	}
	if len(retried) != 1 || retried[0] != ListingRetry {
		t.Fatalf("retries were reported for %q, want the listing", retried)
	}
}

// A listing that is throttled without end still moves to the next address,
// after the attempts an address has.
func TestAnEndlesslyThrottledListingMovesOn(t *testing.T) {
	waits := sleeps(t)
	files := modelFiles()
	mirror, upstream := newThrottledHub(t, files), newHub(t, files)
	mirror.listings = 1 << 20
	r, dir := &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, &cooler{}, r, mirror.URL, upstream.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if len(r.switches) != 1 || !strings.Contains(r.switches[0].Reason, "429") {
		t.Fatalf("switches = %+v", r.switches)
	}
	if len(*waits) != 2 {
		t.Fatalf("waits = %v, want one before each of the mirror's two retries", *waits)
	}
}
