package download

import (
	"net/http"
	"testing"
	"time"
)

func TestStatusErrorCarriesTheWaitOfAThrottledAnswerOnly(t *testing.T) {
	header := http.Header{"Retry-After": {"9"}}
	for code, throttled := range map[int]bool{429: true, 503: true, 500: false, 403: false, 404: false} {
		e := statusError(&http.Response{StatusCode: code, Header: header}, "x")
		if e.Throttled() != throttled {
			t.Errorf("%d: Throttled() = %v", code, e.Throttled())
		}
		wait, ok := IsThrottled(e)
		want := time.Duration(0)
		if throttled {
			want = 9 * time.Second
		}
		if ok != throttled || wait != want || e.RetryAfter != want {
			t.Errorf("%d: IsThrottled = %s, %v; RetryAfter %s; want %s", code, wait, ok, e.RetryAfter, want)
		}
	}
	if _, ok := IsThrottled(nil); ok {
		t.Fatal("nil is not throttled")
	}
}
