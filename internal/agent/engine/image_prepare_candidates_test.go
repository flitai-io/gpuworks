package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// candidateDriver is the imageToleranceDriver with a scripted pull, so a test
// can stall one source or inspect the context a pull got.
type candidateDriver struct {
	imageToleranceDriver
	mu    sync.Mutex
	steps map[string]func(ctx context.Context, onLine func(string)) error
	auth  map[string]proto.SourceAuth
}

func (d *candidateDriver) PullImage(ctx context.Context, ref string, onLine func(string)) error {
	d.mu.Lock()
	if auth, ok := runtime.RegistryAuthFor(ctx, ref); ok {
		if d.auth == nil {
			d.auth = map[string]proto.SourceAuth{}
		}
		d.auth[ref] = auth
	}
	step := d.steps[ref]
	d.mu.Unlock()
	if step != nil {
		if err := step(ctx, onLine); err != nil {
			d.mu.Lock()
			if d.pullErrs == nil {
				d.pullErrs = map[string]error{}
			}
			d.pullErrs[ref] = err
			d.mu.Unlock()
		}
	}
	return d.imageToleranceDriver.PullImage(ctx, ref, onLine)
}

// startWith is startImageSpec with engine options, returning the statuses.
func startWith(t *testing.T, driver runtime.Driver, spec proto.InstanceDesiredState, options ...EngineOption) []proto.InstanceStatus {
	t.Helper()
	h := newSleepHTTPServer(t)
	reported := make(chan proto.InstanceStatus, 256)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{}, options...)
	spec.UUID = "image-candidates"
	spec.APIPort, spec.Ports, spec.StartTimeout = h.port(t), []int{h.port(t)}, 1
	e.startOrAdopt(t.Context(), spec)
	var all []proto.InstanceStatus
	for {
		select {
		case st := <-reported:
			all = append(all, st)
		default:
			return all
		}
	}
}

func messagesOf(statuses []proto.InstanceStatus) string {
	var all []string
	for _, st := range statuses {
		all = append(all, st.Message)
	}
	return strings.Join(all, "\n")
}

type fixedTransport map[string]int // host -> status; 0 = unreachable

func (f fixedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := f[req.URL.Host]
	if status == 0 {
		return nil, errors.New("connection refused")
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

func pullerWith(cfg imagepull.Config, hosts fixedTransport) *imagepull.Puller {
	cfg.Prober = registryprobe.New(registryprobe.WithTimeout(time.Second), registryprobe.WithTransport(func(registryprobe.Route) http.RoundTripper { return hosts }))
	return imagepull.New(cfg)
}

func TestServerPlannedCandidatesAreUsedExactlyAsSent(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{"endpoint.example/org/repo:v1": errTest("endpoint down")}}
	setNetwork, networkOption := networkConfigHolder()
	setNetwork(proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: "ghcr.io", To: "ghcr.mirror.example"}, {From: "endpoint.example", To: "rewritten.example"}}})
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image: "ghcr.io/org/repo:v1",
		// A legacy flat list and legacy flags that the candidates supersede.
		ImagePullRefs: []string{"legacy.example/org/repo:v1"}, ImagePullExplicit: true, ImagePullProxy: "http://legacy:3128",
		ImagePullCandidates: []proto.ImagePullCandidate{
			{Ref: "endpoint.example/org/repo:v1"},
			{Ref: "ghcr.io/org/repo:v1", Rank: true},
		},
	}, networkOption)
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if want := []string{"endpoint.example/org/repo:v1", "ghcr.io/org/repo:v1"}; !reflect.DeepEqual(driver.pullRefs, want) {
		t.Fatalf("pulled %v, want %v (no node rewrite, no legacy list)", driver.pullRefs, want)
	}
	for _, p := range driver.pullProxy {
		if p != "" {
			t.Fatalf("a legacy per-deployment proxy reached a server-planned pull: %v", driver.pullProxy)
		}
	}
	if len(driver.tags) != 0 || len(driver.runImages) != 1 || driver.runImages[0] != "ghcr.io/org/repo:v1" {
		t.Fatalf("tags=%v launched=%v", driver.tags, driver.runImages)
	}
}

func TestServerPlannedCandidateIsTaggedBackToTheCanonicalRef(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image:               "ghcr.io/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "swr.example.com/org/repo:v1"}},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(driver.tags) != 1 || driver.tags[0] != [2]string{"swr.example.com/org/repo:v1", "ghcr.io/org/repo:v1"} || driver.runImages[0] != "ghcr.io/org/repo:v1" {
		t.Fatalf("tags=%v launched=%v", driver.tags, driver.runImages)
	}
}

func TestServerPlannedTagFailureFallsThroughToTheNextCandidate(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	// The first candidate pulls but cannot be tagged; the second can.
	tagFailures := &tagFailingDriver{imageToleranceDriver: driver, failSource: "a.example/org/repo:v1"}
	statuses := startWith(t, tagFailures, proto.InstanceDesiredState{
		Image:               "ghcr.io/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "a.example/org/repo:v1"}, {Ref: "b.example/org/repo:v1"}},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if want := []string{"a.example/org/repo:v1", "b.example/org/repo:v1"}; !reflect.DeepEqual(driver.pullRefs, want) {
		t.Fatalf("pulled %v", driver.pullRefs)
	}
	if !strings.Contains(messagesOf(statuses), "canonical image tag failed") {
		t.Fatalf("the refused source is not visible:\n%s", messagesOf(statuses))
	}
}

type tagFailingDriver struct {
	*imageToleranceDriver
	failSource string
}

func (d *tagFailingDriver) TagImage(ctx context.Context, source, target string) error {
	if source == d.failSource {
		return errors.New("tag denied")
	}
	return d.imageToleranceDriver.TagImage(ctx, source, target)
}

func TestServerPlannedAllSourcesFailedNamesEach(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{"a.example/r:1": errTest("denied"), "b.example/r:1": errTest("timeout")}}
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image:               "a.example/r:1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "a.example/r:1"}, {Ref: "b.example/r:1"}},
	})
	if got, want := failedMessage(statuses), "all image sources failed: a.example/r:1: denied | b.example/r:1: timeout"; got != want {
		t.Fatalf("failure = %q, want %q", got, want)
	}
	if driver.runCalls != 0 {
		t.Fatal("run called after every source failed")
	}
}

func TestRankedCandidatesAreOrderedByProbeAndTheOrderIsReportedOnce(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	puller := pullerWith(imagepull.Config{}, fixedTransport{"up.example": 200})
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image: "down.example/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{
			{Ref: "down.example/org/repo:v1", Rank: true},
			{Ref: "up.example/org/repo:v1", Rank: true},
		},
	}, WithImagePuller(puller))
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if want := []string{"up.example/org/repo:v1"}; !reflect.DeepEqual(driver.pullRefs, want) {
		t.Fatalf("pulled %v, want the reachable copy first (and only)", driver.pullRefs)
	}
	if n := strings.Count(messagesOf(statuses), "image sources by probe:"); n != 1 {
		t.Fatalf("probe order reported %d times:\n%s", n, messagesOf(statuses))
	}
	if !strings.Contains(messagesOf(statuses), "image sources by probe: up.example (") || !strings.Contains(messagesOf(statuses), "down.example (unreachable)") {
		t.Fatalf("summary:\n%s", messagesOf(statuses))
	}
	for _, want := range []string{"from=node", "GET /v2/", "HTTP 200", "observed_at=", "image authorization, tag, manifest and layers not checked"} {
		if !strings.Contains(messagesOf(statuses), want) {
			t.Errorf("deployment preparation lost registry probe scope %q:\n%s", want, messagesOf(statuses))
		}
	}
}

func TestUnrankedCandidatesAreNeverProbed(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	puller := pullerWith(imagepull.Config{}, fixedTransport{}) // everything unreachable
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image:               "a.example/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "a.example/org/repo:v1"}, {Ref: "b.example/org/repo:v1"}},
	}, WithImagePuller(puller))
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if strings.Contains(messagesOf(statuses), "by probe") || driver.pullRefs[0] != "a.example/org/repo:v1" {
		t.Fatalf("unranked candidates were reordered or probed: %v\n%s", driver.pullRefs, messagesOf(statuses))
	}
}

func TestStalledSourceIsAbandonedForTheNext(t *testing.T) {
	oldStall, oldBeat := imagepull.StallTimeout, imagepull.HeartbeatInterval
	imagepull.StallTimeout, imagepull.HeartbeatInterval = 80*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { imagepull.StallTimeout, imagepull.HeartbeatInterval = oldStall, oldBeat })

	driver := &candidateDriver{
		imageToleranceDriver: imageToleranceDriver{present: map[string]bool{}},
		steps: map[string]func(context.Context, func(string)) error{
			"slow.example/org/repo:v1": func(ctx context.Context, onLine func(string)) error {
				onLine("abcdef123456: Downloading 1MB/500MB")
				<-ctx.Done()
				return ctx.Err()
			},
		},
	}
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image: "ghcr.io/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{
			{Ref: "slow.example/org/repo:v1"},
			{Ref: "fast.example/org/repo:v1"},
		},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if want := []string{"slow.example/org/repo:v1", "fast.example/org/repo:v1"}; !reflect.DeepEqual(driver.pullRefs, want) {
		t.Fatalf("pulled %v", driver.pullRefs)
	}
	if !strings.Contains(messagesOf(statuses), "image source failed: slow.example/org/repo:v1: no download progress for 80ms") {
		t.Fatalf("the stalled source is not reported:\n%s", messagesOf(statuses))
	}
}

func TestRegistryLoginsReachTheRuntimePull(t *testing.T) {
	driver := &candidateDriver{imageToleranceDriver: imageToleranceDriver{present: map[string]bool{}}}
	puller := imagepull.New(imagepull.Config{Auth: func() []proto.SourceAuth {
		return []proto.SourceAuth{{Host: "swr.example.com", Username: "robot", Secret: "s3cret"}}
	}})
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image:               "swr.example.com/org/repo:v1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "swr.example.com/org/repo:v1"}},
	}, WithImagePuller(puller))
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if got := driver.auth["swr.example.com/org/repo:v1"]; got.Username != "robot" || got.Secret != "s3cret" {
		t.Fatalf("pull got login %+v", got)
	}
	if strings.Contains(messagesOf(statuses), "s3cret") {
		t.Fatal("the secret leaked into the progress stream")
	}
}

func TestNodeProxyIsAttachedToServerPlannedPullsOfNonDockerRuntimes(t *testing.T) {
	node := proto.NetworkConfig{HTTPSProxy: "http://node.proxy:3128"}
	for _, tc := range []struct {
		name       string
		driverName string
		spec       proto.InstanceDesiredState
		want       string
	}{
		{"nerdctl follows the node proxy", "nerdctl", proto.InstanceDesiredState{Image: "r.example/x:1", ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "r.example/x:1"}}}, "http://node.proxy:3128"},
		{"docker leaves its daemon's proxy alone", "docker", proto.InstanceDesiredState{Image: "r.example/x:1", ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "r.example/x:1"}}}, ""},
		{"the legacy path never took the node proxy", "nerdctl", proto.InstanceDesiredState{Image: "r.example/x:1", ImagePullRefs: []string{"r.example/x:1"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &imageToleranceDriver{present: map[string]bool{}}
			driver.name = tc.driverName
			setNetwork, networkOption := networkConfigHolder()
			setNetwork(node)
			statuses := startWith(t, driver, tc.spec, networkOption)
			if msg := failedMessage(statuses); msg != "" {
				t.Fatalf("start failed: %s", msg)
			}
			if len(driver.pullProxy) != 1 || driver.pullProxy[0] != tc.want {
				t.Fatalf("pull proxy = %v, want %q", driver.pullProxy, tc.want)
			}
		})
	}
}

func TestLegacyDesiredStateIsPulledAsListed(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}}
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image: "ghcr.io/org/repo:v1", ImagePullRefs: []string{"ghcr.io/org/repo:v1"},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if want := []string{"ghcr.io/org/repo:v1"}; !reflect.DeepEqual(driver.pullRefs, want) {
		t.Fatalf("legacy list pulled as %v, want %v", driver.pullRefs, want)
	}
	if strings.Contains(messagesOf(statuses), "by probe") {
		t.Fatal("a legacy list must not be probed")
	}
}

func TestLocalDiskExhaustionDoesNotFallBackOrBlameImageSources(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{
		// As the docker driver returns it: the CLI's output, marked where the CLI ran.
		"a.example/r:1": runtime.MarkPullFailure(errTest("write /var/lib/docker/tmp/blob: no space left on device"), runtime.ErrNoSpace),
	}}
	statuses := startWith(t, driver, proto.InstanceDesiredState{
		Image:               "a.example/r:1",
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "a.example/r:1"}, {Ref: "b.example/r:1"}},
	})
	msg := failedMessage(statuses)
	if !strings.Contains(msg, "insufficient disk space") || !strings.Contains(msg, "no space left on device") {
		t.Fatalf("failure = %q, want actionable disk space error", msg)
	}
	if strings.Contains(msg, "image source failed") || strings.Contains(msg, "all image sources failed") {
		t.Fatalf("failure misdiagnoses node disk exhaustion as source failure: %q", msg)
	}
	if got := driver.pullRefs; !reflect.DeepEqual(got, []string{"a.example/r:1"}) {
		t.Fatalf("pulled %v, want no fallback source", got)
	}
	if driver.runCalls != 0 {
		t.Fatalf("run called %d times after image pull exhausted disk", driver.runCalls)
	}
}
