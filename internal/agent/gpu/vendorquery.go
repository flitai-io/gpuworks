package gpu

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Read-only vendor CLI queries (inventory, metrics, topology, versions) run
// in the background, apart from the caller that asked: a caller waits at most
// vendorCLITimeout, while the query itself may run to vendorQueryLimit. On a
// host where the driver answers slowly (every client queues on the driver's
// lock, as when a card fails to initialize) a query that outlives its caller
// is not killed and wasted: it finishes, and the next call is served its
// result. Commands that change driver state, and answers a caller must have
// fresh, go through RunVendorCLI instead.
var (
	// vendorQueryLimit is how long one background query may run before it
	// is killed.
	vendorQueryLimit = 180 * time.Second
	// vendorQueryMaxAge is how old an answer may be and still be served.
	// Past it, the CLI counts as unresponsive and the probe fails.
	vendorQueryMaxAge = 10 * time.Minute
)

// vendorQuery is one distinct query (binary and arguments).
type vendorQuery struct {
	running chan struct{} // closed when the current run ends; nil while idle

	out string    // the last completed run
	err error     // ...its error
	at  time.Time // ...when it completed

	okOut string    // the last successful run
	okAt  time.Time // ...when it completed
}

// vendorBinary is what the queries of one CLI share.
type vendorBinary struct {
	// slot lets its queries run one at a time, so a slow driver never has
	// more than one of our processes queued on it.
	slot         chan struct{}
	runningSince time.Time     // when the query holding the slot started; zero while idle
	lastTook     time.Duration // how long its last finished query ran
}

var vendorQueries = struct {
	sync.Mutex
	byKey    map[string]*vendorQuery
	binaries map[string]*vendorBinary
}{byKey: map[string]*vendorQuery{}, binaries: map[string]*vendorBinary{}}

// VendorCLISlow reports whether a CLI answers slower than callers wait: its
// last query ran past vendorCLITimeout, or the one running now has. Callers
// then skip optional queries (each one queues behind the others) and are
// served earlier answers without waiting.
func VendorCLISlow(name string) bool {
	vendorQueries.Lock()
	defer vendorQueries.Unlock()
	return vendorCLISlowLocked(name)
}

func vendorCLISlowLocked(name string) bool {
	b := vendorQueries.binaries[binaryKey(name)]
	if b == nil {
		return false
	}
	return b.lastTook > vendorCLITimeout || (!b.runningSince.IsZero() && time.Since(b.runningSince) > vendorCLITimeout)
}

// QueryVendorCLI runs a read-only query of a vendor CLI and returns its
// combined output and when that output was produced. Concurrent callers of
// the same query share one process. A caller that cannot wait for the
// process gets the last successful answer while it is no older than
// vendorQueryMaxAge, and ErrCLIUnresponsive after that. A completed run's
// own failure is returned as is.
func QueryVendorCLI(ctx context.Context, name string, args ...string) (string, error) {
	out, at, err := queryVendorCLI(ctx, name, args...)
	if err == nil {
		RecordResultTime(ctx, at)
	}
	return out, err
}

// queryLogged is QueryVendorCLI with the probers' usual command logging.
func queryLogged(ctx context.Context, vendor, name string, args ...string) (string, error) {
	slog.Debug("gpu command", "vendor", vendor, "args", args)
	out, err := QueryVendorCLI(ctx, name, args...)
	if err != nil {
		slog.Warn("gpu command failed", "vendor", vendor, "args", args, "err", err, "output_tail", tail(out, 400))
	}
	return out, err
}

// QueryVendorCLIStdout keeps stderr diagnostics out of structured stdout.
// It shares the per-binary process slot, but never a combined-output cache entry.
func QueryVendorCLIStdout(ctx context.Context, name string, args ...string) (string, error) {
	out, at, err := queryVendorCLIOutput(ctx, true, name, args...)
	if err == nil {
		RecordResultTime(ctx, at)
	}
	return out, err
}

func queryVendorCLI(ctx context.Context, name string, args ...string) (string, time.Time, error) {
	return queryVendorCLIOutput(ctx, false, name, args...)
}

func queryVendorCLIOutput(ctx context.Context, stdoutOnly bool, name string, args ...string) (string, time.Time, error) {
	key := name + "\x00" + strings.Join(args, "\x00")
	if stdoutOnly {
		key = "stdout\x00" + key
	}
	vendorQueries.Lock()
	q := vendorQueries.byKey[key]
	if q == nil {
		q = &vendorQuery{}
		vendorQueries.byKey[key] = q
	}
	b := vendorQueries.binaries[binaryKey(name)]
	if b == nil {
		b = &vendorBinary{slot: make(chan struct{}, 1)}
		vendorQueries.binaries[binaryKey(name)] = b
	}
	if q.running == nil {
		q.running = make(chan struct{})
		go runVendorQuery(q, b, q.running, name, args, stdoutOnly)
	}
	running := q.running
	// A slow CLI's earlier answer is served at once: waiting would only
	// spend the caller's time on a query that will not finish in it.
	if vendorCLISlowLocked(name) && fresh(q.okAt) {
		out, at := q.okOut, q.okAt
		vendorQueries.Unlock()
		return out, at, nil
	}
	vendorQueries.Unlock()

	wait := time.NewTimer(vendorCLITimeout)
	defer wait.Stop()
	select {
	case <-running:
		vendorQueries.Lock()
		defer vendorQueries.Unlock()
		return q.out, q.at, q.err
	case <-ctx.Done():
		return "", time.Time{}, ctx.Err()
	case <-wait.C:
	}
	vendorQueries.Lock()
	okOut, okAt := q.okOut, q.okAt
	vendorQueries.Unlock()
	if fresh(okAt) {
		return okOut, okAt, nil
	}
	return "", time.Time{}, fmt.Errorf("%s: no answer within %s: %w", name, vendorCLITimeout, ErrCLIUnresponsive)
}

func fresh(at time.Time) bool { return !at.IsZero() && time.Since(at) <= vendorQueryMaxAge }

func runVendorQuery(q *vendorQuery, b *vendorBinary, done chan struct{}, name string, args []string, stdoutOnly bool) {
	b.slot <- struct{}{}
	defer func() { <-b.slot }()

	start := time.Now()
	vendorQueries.Lock()
	b.runningSince = start
	vendorQueries.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), vendorQueryLimit)
	defer cancel()
	var buf bytes.Buffer
	cmd := cliCommand(ctx, name, args...)
	killGroupOnCancel(cmd)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	var stderr limitedDiagnostic
	if stdoutOnly {
		cmd.Stderr = &stderr
	}
	cmd.WaitDelay = time.Second
	// A process stuck in the driver past the kill keeps the slot until it
	// exits, so no second one joins it; callers are served the last answer
	// meanwhile.
	err := cmd.Run()
	if stdoutOnly && err != nil {
		slog.Warn("structured gpu query failed", "command", name, "stderr_tail", stderr.String())
	}
	took := time.Since(start)
	if ctx.Err() != nil {
		err = fmt.Errorf("%s: no answer within %s: %w", name, vendorQueryLimit, ErrCLIUnresponsive)
		slog.Warn("gpu query killed at its time limit", "command", name, "args", strings.Join(args, " "), "took", took.Round(time.Second))
	} else if took > vendorCLITimeout {
		slog.Warn("gpu query slow; callers are served its last answer meanwhile", "command", name, "args", strings.Join(args, " "), "took", took.Round(time.Second))
	}

	now := time.Now()
	vendorQueries.Lock()
	q.out, q.err, q.at = buf.String(), err, now
	if err == nil {
		q.okOut, q.okAt = q.out, now
	}
	q.running = nil
	b.runningSince, b.lastTook = time.Time{}, took
	vendorQueries.Unlock()
	close(done)
}

// ResultTime collects when the answers used during one call were produced.
type ResultTime struct {
	mu     sync.Mutex
	oldest time.Time
}

type resultTimeKey struct{}

// WithResultTime returns a context under which every vendor query records
// when its answer was produced, so a caller can date what it built from a
// cached answer (the metrics collector stamps samples with it).
func WithResultTime(ctx context.Context) (context.Context, *ResultTime) {
	rt := &ResultTime{}
	return context.WithValue(ctx, resultTimeKey{}, rt), rt
}

// Oldest is when the oldest answer used was produced; zero when no vendor
// query answered.
func (rt *ResultTime) Oldest() time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.oldest
}

// RecordResultTime notes, under a WithResultTime context, that an answer
// produced at at was used. QueryVendorCLI calls it; a prober that gets its
// answers elsewhere may too.
func RecordResultTime(ctx context.Context, at time.Time) {
	rt, _ := ctx.Value(resultTimeKey{}).(*ResultTime)
	if rt == nil || at.IsZero() {
		return
	}
	rt.mu.Lock()
	if rt.oldest.IsZero() || at.Before(rt.oldest) {
		rt.oldest = at
	}
	rt.mu.Unlock()
}

// binaryKey is the name a CLI's health is tracked under: its base name, so a
// tool run by its full path (hy-smi under /opt/dtk/bin when it is not on
// PATH) is the same binary to VendorCLISlow("hy-smi") as one found on PATH.
func binaryKey(name string) string { return filepath.Base(name) }
