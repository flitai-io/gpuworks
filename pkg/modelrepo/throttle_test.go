package modelrepo

import (
	"net/http"
	"testing"
	"time"
)

func TestThrottleWaitReadsBothHeaderForms(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		header http.Header
		want   time.Duration
	}{
		"seconds":      {http.Header{"Retry-After": {"12"}}, 12 * time.Second},
		"date":         {http.Header{"Retry-After": {now.Add(45 * time.Second).Format(http.TimeFormat)}}, 45 * time.Second},
		"past date":    {http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, 0},
		"far date":     {http.Header{"Retry-After": {now.AddDate(10, 0, 0).Format(http.TimeFormat)}}, 24 * time.Hour},
		"negative":     {http.Header{"Retry-After": {"-3"}}, 0},
		"garbage":      {http.Header{"Retry-After": {"soon"}}, 0},
		"huge seconds": {http.Header{"Retry-After": {"99999999999999999"}}, 24 * time.Hour},
		// The reset is whole seconds: a second more keeps the retry from
		// arriving just before it.
		"hugging face reset": {http.Header{"Ratelimit": {`"resolvers";r=0;t=110`}}, 111 * time.Second},
		// The exhausted quota is what the answer is about, even when another
		// resets later.
		"exhausted policy wins": {http.Header{"Ratelimit": {`"api";r=500;t=280, "resolvers";r=0;t=10`}}, 11 * time.Second},
		"none exhausted":        {http.Header{"Ratelimit": {`"api";r=5;t=20, "resolvers";r=3;t=90`}}, 91 * time.Second},
		"zero reset":            {http.Header{"Ratelimit": {`"resolvers";r=0;t=0`}}, 0},
		"retry-after wins":      {http.Header{"Retry-After": {"2"}, "Ratelimit": {`"resolvers";r=0;t=110`}}, 2 * time.Second},
		"ratelimit without t":   {http.Header{"Ratelimit": {`"resolvers";r=0`}}, 0},
		"huge reset":            {http.Header{"Ratelimit": {`"resolvers";r=0;t=9223372036854775807`}}, 24 * time.Hour},
		"no header":             {http.Header{}, 0},
	} {
		if got := ThrottleWait(tc.header, now); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestThrottles(t *testing.T) {
	for code, want := range map[int]bool{429: true, 503: true, 500: false, 502: false, 403: false, 404: false, 200: false} {
		if Throttles(code) != want || (&StatusError{Code: code}).Throttled() != want {
			t.Errorf("%d: want %v", code, want)
		}
	}
}

// Only a throttled answer carries a wait: a Retry-After on a 500 or a 404
// is not a reason to wait and ask the same address again.
func TestOnlyAThrottledListingAnswerCarriesItsWait(t *testing.T) {
	header := http.Header{"Retry-After": {"9"}}
	for code, want := range map[int]time.Duration{429: 9 * time.Second, 503: 9 * time.Second, 500: 0, 404: 0} {
		if got := throttleError(&http.Response{StatusCode: code, Header: header}, "u", "d").RetryAfter; got != want {
			t.Errorf("%d: RetryAfter %s, want %s", code, got, want)
		}
	}
}
