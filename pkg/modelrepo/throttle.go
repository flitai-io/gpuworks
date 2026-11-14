package modelrepo

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Throttled reports whether the hub answered "slow down" rather than that it
// cannot serve the request: 429, or 503. The answer is about the moment, not
// the address.
func (e *StatusError) Throttled() bool { return Throttles(e.Code) }

// Throttles reports whether an HTTP status asks the client to slow down.
func Throttles(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// ThrottleWait is how long a throttled answer asks the client to wait: its
// Retry-After (seconds or an HTTP date), or else the reset time of an IETF
// RateLimit header, which Hugging Face sends instead, as in
//
//	RateLimit: "resolvers";r=0;t=110
//
// where r is what is left of the quota and t the whole seconds until it
// resets; a second is added, as huggingface_hub does, so the retry does not
// arrive just before the reset. Of several quotas, the ones exhausted (r=0)
// are what the answer is about; only when none is does the longest reset
// count. Zero when nothing says.
func ThrottleWait(h http.Header, now time.Time) time.Duration {
	if value := strings.TrimSpace(h.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return throttleSeconds(seconds)
		}
		if when, err := http.ParseTime(value); err == nil {
			return min(max(0, when.Sub(now)), maxThrottleSeconds*time.Second)
		}
	}
	var exhausted, longest time.Duration
	for _, item := range strings.Split(h.Get("RateLimit"), ",") {
		remaining, reset := int64(-1), int64(0)
		for _, param := range strings.Split(item, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				continue
			}
			switch strings.TrimSpace(key) {
			case "r":
				remaining = n
			case "t":
				reset = n
			}
		}
		if reset <= 0 {
			continue
		}
		wait := throttleSeconds(min(reset, maxThrottleSeconds) + 1)
		longest = max(longest, wait)
		if remaining == 0 {
			exhausted = max(exhausted, wait)
		}
	}
	if exhausted > 0 {
		return exhausted
	}
	return longest
}

// throttleError is the StatusError for a response that cannot be what was
// asked for; a throttled one carries the wait it names.
func throttleError(resp *http.Response, url, detail string) *StatusError {
	e := &StatusError{URL: url, Code: resp.StatusCode, Detail: detail}
	if e.Throttled() {
		e.RetryAfter = ThrottleWait(resp.Header, time.Now())
	}
	return e
}

// maxThrottleSeconds keeps a hostile or broken header from overflowing a
// Duration; callers bound the wait far below it anyway.
const maxThrottleSeconds = 24 * 60 * 60

func throttleSeconds(n int64) time.Duration {
	return time.Duration(min(max(n, 0), maxThrottleSeconds)) * time.Second
}
