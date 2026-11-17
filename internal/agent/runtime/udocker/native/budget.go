package native

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
)

const (
	// DefaultMaxConnections is the most registry connections a pull may open
	// to one registry host (or one proxy) when nothing is configured. The old
	// fixed split could open Workers x RangeParts = 24; registries with a
	// per-address connection or request limit (Harbor, SWR, ACR, a corporate
	// proxy) answer that with 429s or resets, and a link that is slow as a
	// whole gains nothing from it. The budget starts lower and climbs only
	// while more connections raise the pull's rate, so this is a ceiling, not
	// a target.
	DefaultMaxConnections = 16
	// MaxConnectionsLimit is the most a configuration may ask for: the HTTP
	// client keeps this many idle connections per host, so a larger ceiling
	// would open connections it then cannot keep for reuse.
	MaxConnectionsLimit = 32
	// startConnections is where a pull's climb begins: what one large blob
	// used before the budget, and a handful of small layers' worth.
	startConnections = 4

	// MaxConnectionsEnv overrides the ceiling per registry host: a positive
	// number is the ceiling (1 = a single connection), 0 or unset is automatic.
	MaxConnectionsEnv = "GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS"
)

// MaxConnectionsFromEnv is the ceiling MaxConnectionsEnv asks for (at most
// MaxConnectionsLimit), or DefaultMaxConnections.
func MaxConnectionsFromEnv() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxConnectionsEnv))); err == nil && n > 0 {
		return min(n, MaxConnectionsLimit)
	}
	return DefaultMaxConnections
}

// openSession opens the budget session of one FetchAll: its connections are
// those of the registry host (or the proxy in front of it) it pulls from.
func (d *Downloader) openSession(ctx context.Context, ref Ref) *connbudget.Session {
	cfg := connbudget.SessionConfig{
		Name:       ref.Registry,
		Start:      startConnections,
		SlowWindow: slowWindow,
		SlowFloor:  slowMinRate,
		Log:        func(msg string, kv ...any) { slog.Info(msg, kv...) },
	}
	var pool *connbudget.Pool
	if d.Budget != nil {
		pool = d.Budget.For(d.Client.BudgetKey(ctx, ref))
	} else {
		workers := d.Workers
		if workers <= 0 {
			workers = 4
		}
		fixed := workers * max(1, d.RangeParts)
		pool = connbudget.NewPool(fixed)
		cfg.Fixed, cfg.Cap = fixed, fixed
	}
	if d.Tune != nil {
		d.Tune(&cfg)
	}
	return pool.Open(cfg)
}

// BudgetKey names the upstream whose limits a pull from ref runs into: the
// registry host, or the proxy the blob requests go through (a proxy's limits
// belong to the proxy, whatever it relays).
func (c *Client) BudgetKey(ctx context.Context, ref Ref) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.knownBaseURL(ref.Registry)+"/v2/", nil)
	if err == nil {
		if proxy, perr := RequestProxy(req); perr == nil && proxy != nil {
			return "proxy:" + proxy.Host
		}
	}
	return ref.Registry
}

// statusError is a registry answer that is not a blob; retryable() and
// isThrottled() read its status, never its text.
type statusError struct {
	msg        string
	status     int
	retryAfter time.Duration
}

func (e *statusError) Error() string { return e.msg }

// throttles reports whether an HTTP status asks the client to slow down.
func throttles(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// isThrottled reports whether err is the registry (or a proxy) pushing back: a
// 429/503 asking the client to slow down, or a connection refused, which is
// what a per-address connection limit looks like from the client. Both are
// waited out with the budget's pause, not retried at once and not failed.
func isThrottled(err error) bool {
	var status *statusError
	return (errors.As(err, &status) && throttles(status.status)) || errors.Is(err, syscall.ECONNREFUSED)
}

// parseRetryAfter reads a Retry-After header: delay seconds or an HTTP date.
// Zero means the header said nothing usable.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(0, time.Duration(seconds)*time.Second)
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(0, when.Sub(now))
	}
	return 0
}

// reportFailure tells the budget how a connection failed, when the way says
// something about the registry rather than about one connection: refused
// connections mean a limit, repeated resets a gentler one. A recycled or
// idle-cut connection, or a cancelled pull, says nothing.
func reportFailure(sess *connbudget.Session, ctx context.Context, err error) {
	switch {
	case ctx.Err() != nil, errors.Is(err, errSlow), errors.Is(err, errIdle), errors.Is(err, context.Canceled):
	case errors.Is(err, syscall.ECONNREFUSED):
		sess.Throttled(0)
	case isReset(err):
		sess.Reset()
	}
}

// isReset reports whether err is a connection that was torn down under the
// client (reset, closed early, handshake dropped) as opposed to a timeout.
func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
