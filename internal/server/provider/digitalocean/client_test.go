package digitalocean

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

var badCreds = provider.Credentials{APIKey: "dop_v1_wrong"}

func TestValidateCredentials(t *testing.T) {
	d, f := newFake(t)
	if err := d.ValidateCredentials(t.Context(), testCreds); err != nil {
		t.Fatal(err)
	}
	if got := f.calls(http.MethodGet, "/v2/account"); len(got) != 1 {
		t.Errorf("GET /v2/account sent %d times", len(got))
	}
}

func TestUnauthorizedIsReportedWithTheAPIsWords(t *testing.T) {
	d, f := newFake(t)
	err := d.ValidateCredentials(t.Context(), badCreds)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.ID != "Unauthorized" || apiErr.Message != "Unable to authenticate you" {
		t.Fatalf("error = %#v", err)
	}
	for _, want := range []string{"401", "Unauthorized", "Unable to authenticate you", "GET /v2/account"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
	if n := len(f.requests); n != 1 {
		t.Errorf("a 401 was sent %d times, want 1 (no repeat)", n)
	}
	// Every call fails the same way, not just the validation.
	if _, err := d.ListInstances(t.Context(), badCreds); err == nil {
		t.Error("ListInstances with a bad token succeeded")
	}
	if _, err := d.ListClusters(t.Context(), badCreds); err == nil {
		t.Error("ListClusters with a bad token succeeded")
	}
}

func TestMissingTokenIsRefusedBeforeAnyRequest(t *testing.T) {
	d, f := newFake(t)
	for _, key := range []string{"", "  "} {
		if err := d.ValidateCredentials(t.Context(), provider.Credentials{APIKey: key}); err == nil {
			t.Errorf("token %q accepted", key)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("requests sent: %v", f.requests)
	}
}

func TestEndpointOverride(t *testing.T) {
	_, f := newFake(t)
	d := New() // the real base URL, replaced through the credentials
	creds := provider.Credentials{APIKey: testToken, Endpoint: f.srv.URL + "/"}
	if err := d.ValidateCredentials(t.Context(), creds); err != nil {
		t.Fatal(err)
	}
	if got := New().base(provider.Credentials{}); got != "https://api.digitalocean.com" {
		t.Errorf("default base = %q", got)
	}
}

func TestRateLimitIsRepeatedThenSucceeds(t *testing.T) {
	d, f := newFake(t)
	var waits []time.Duration
	d.sleep = func(_ context.Context, wait time.Duration) error { waits = append(waits, wait); return nil }
	f.forced = []forcedResponse{
		{status: 429, header: map[string]string{"Retry-After": "5"}, body: `{"id":"too_many_requests","message":"API Rate limit exceeded."}`},
		{status: 429, body: `{"id":"too_many_requests","message":"API Rate limit exceeded."}`},
	}
	if err := d.ValidateCredentials(t.Context(), testCreds); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(http.MethodGet, "/v2/account")); n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
	// The first wait is what Retry-After asked for, the second a backoff.
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 2*time.Second {
		t.Errorf("waits = %v", waits)
	}
}

func TestServerErrorsAreRepeatedForReadsAndGiveUp(t *testing.T) {
	d, f := newFake(t)
	for range requestAttempts {
		f.forced = append(f.forced, forcedResponse{status: 503, body: `{"id":"service_unavailable","message":"Service Unavailable"}`})
	}
	_, err := d.ListInstances(t.Context(), testCreds)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 || apiErr.ID != "service_unavailable" {
		t.Fatalf("error = %v", err)
	}
	if n := len(f.requests); n != requestAttempts {
		t.Errorf("requests = %d, want %d", n, requestAttempts)
	}
}

func TestLongRetryAfterIsNotWaitedFor(t *testing.T) {
	d, f := newFake(t)
	f.forced = []forcedResponse{{status: 429, header: map[string]string{"Retry-After": "3600"}, body: `{"id":"too_many_requests","message":"API Rate limit exceeded."}`}}
	if err := d.ValidateCredentials(t.Context(), testCreds); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v", err)
	}
	if len(f.requests) != 1 {
		t.Errorf("requests = %d", len(f.requests))
	}
}

func TestErrorFromAProxyIsKeptShort(t *testing.T) {
	d, f := newFake(t)
	f.forced = []forcedResponse{{status: 403, body: "<html>" + strings.Repeat("blocked ", 500) + "</html>"}}
	err := d.ValidateCredentials(t.Context(), testCreds)
	if err == nil || !strings.Contains(err.Error(), "403") || len(err.Error()) > 400 {
		t.Fatalf("error (%d bytes) = %v", len(err.Error()), err)
	}
}

func TestRepeatingStopsWhenTheContextEnds(t *testing.T) {
	d, f := newFake(t)
	for range requestAttempts {
		f.forced = append(f.forced, forcedResponse{status: 429, header: map[string]string{"Retry-After": "1"}, body: `{"id":"too_many_requests","message":"API Rate limit exceeded."}`})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.sleep = func(c context.Context, _ time.Duration) error { cancel(); return c.Err() }
	if err := d.ValidateCredentials(ctx, testCreds); err == nil {
		t.Fatal("succeeded")
	}
	if len(f.requests) != 1 {
		t.Errorf("requests = %d, want 1", len(f.requests))
	}
}
