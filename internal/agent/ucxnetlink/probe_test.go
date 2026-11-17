package ucxnetlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// message is one netlink message of the given type with a payload, padded to
// 4 bytes as the kernel does.
func message(typ uint16, payload int) []byte {
	length := nlmsgHdrLen + payload
	msg := make([]byte, (length+3)&^3)
	binary.NativeEndian.PutUint32(msg[0:], uint32(length))
	binary.NativeEndian.PutUint16(msg[4:], typ)
	return msg
}

func datagram(msgs ...[]byte) []byte { return bytes.Join(msgs, nil) }

func route() []byte { return message(rtmNewRoute, 52) }
func done() []byte  { return message(nlmsgDone, 4) }

func TestAnalyzeFindsADoneBehindRoutesInOneDatagram(t *testing.T) {
	cases := []struct {
		name      string
		in        [][]byte
		affected  bool
		routes    int
		wantError string
	}{
		{name: "newer kernel: last routes and DONE in one datagram", in: [][]byte{datagram(route(), route()), datagram(route(), done())}, affected: true, routes: 3},
		{name: "DONE right behind the only route", in: [][]byte{datagram(route(), done())}, affected: true, routes: 1},
		{name: "older kernel: DONE is a datagram of its own", in: [][]byte{datagram(route(), route()), datagram(done())}, routes: 2},
		{name: "no IPv6 routes: the dump is just DONE", in: [][]byte{datagram(done())}},
		{name: "DONE first in a datagram after routes in earlier ones", in: [][]byte{datagram(route()), datagram(done())}, routes: 1},
		{name: "a dump that ended in an error message", in: [][]byte{datagram(message(nlmsgError, 20))}, wantError: "error"},
		{name: "a dump that never says DONE", in: [][]byte{datagram(route())}, routes: 1, wantError: "without NLMSG_DONE"},
		{name: "no datagrams", wantError: "without NLMSG_DONE"},
		{name: "trailing bytes shorter than a header", in: [][]byte{append(datagram(route()), 1, 2, 3)}, routes: 1, wantError: "trailing"},
		{name: "a message longer than its datagram", in: [][]byte{datagram(route())[:30]}, wantError: "does not fit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Analyze(tc.in)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Affected != tc.affected || res.Routes != tc.routes || res.Datagrams != len(tc.in) {
				t.Fatalf("result = %+v, want affected=%t routes=%d datagrams=%d", res, tc.affected, tc.routes, len(tc.in))
			}
		})
	}
}

// recvFrom serves datagrams one per read, as a socket does.
func recvFrom(datagrams ...[]byte) func([]byte) (int, error) {
	i := 0
	return func(buf []byte) (int, error) {
		if i >= len(datagrams) {
			return 0, errors.New("socket drained")
		}
		n := copy(buf, datagrams[i])
		i++
		return n, nil
	}
}

func TestCollectStopsAtDoneOrErrorAndAtTheCaps(t *testing.T) {
	lim := limits{datagrams: 8, bytes: 1 << 10, buffer: 512}
	t.Run("stops after the datagram that holds DONE", func(t *testing.T) {
		got, err := collect(context.Background(), recvFrom(datagram(route()), datagram(route(), done()), datagram(route())), lim)
		if err != nil || len(got) != 2 {
			t.Fatalf("got %d datagrams, err %v; want the two up to DONE", len(got), err)
		}
	})
	t.Run("stops after an error message", func(t *testing.T) {
		got, err := collect(context.Background(), recvFrom(datagram(message(nlmsgError, 20)), datagram(route())), lim)
		if err != nil || len(got) != 1 {
			t.Fatalf("got %d datagrams, err %v", len(got), err)
		}
	})
	t.Run("an endless stream of routes is cut off by the datagram cap", func(t *testing.T) {
		reads := 0
		endless := func(buf []byte) (int, error) { reads++; return copy(buf, route()), nil }
		got, err := collect(context.Background(), endless, lim)
		if err == nil || !strings.Contains(err.Error(), "datagrams") || len(got) != lim.datagrams || reads != lim.datagrams {
			t.Fatalf("got %d datagrams after %d reads, err %v; want the cap at %d", len(got), reads, err, lim.datagrams)
		}
	})
	t.Run("a stream of large datagrams is cut off by the byte cap", func(t *testing.T) {
		reads := 0
		big := func(buf []byte) (int, error) { reads++; return copy(buf, message(rtmNewRoute, 300)), nil }
		_, err := collect(context.Background(), big, limits{datagrams: 1000, bytes: 1 << 10, buffer: 512})
		if err == nil || !strings.Contains(err.Error(), "bytes") || reads > 4 {
			t.Fatalf("err %v after %d reads; want the byte cap", err, reads)
		}
	})
	t.Run("a datagram that fills the buffer may be truncated", func(t *testing.T) {
		oversized := func(buf []byte) (int, error) { return len(buf), nil }
		if _, err := collect(context.Background(), oversized, lim); err == nil || !strings.Contains(err.Error(), "buffer") {
			t.Fatalf("err = %v, want the buffer-full refusal", err)
		}
	})
	t.Run("an empty read is an error rather than a spin", func(t *testing.T) {
		reads := 0
		if _, err := collect(context.Background(), func([]byte) (int, error) { reads++; return 0, nil }, lim); err == nil || reads != 1 {
			t.Fatalf("err %v after %d reads", err, reads)
		}
	})
	t.Run("a finished context stops the read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := collect(ctx, recvFrom(datagram(done())), lim); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testMonitor(dump Dumper, c *clock, log *slog.Logger) *Monitor {
	return &Monitor{
		Dump: dump, Kernel: func() string { return "5.15.0-203-generic" }, Now: c.now, Log: log, gate: new(atomic.Bool),
		Timeout: time.Second, Interval: 15 * time.Minute, RetryAfterError: time.Minute,
	}
}

func answers(datagrams ...[]byte) (Dumper, *atomic.Int32) {
	var calls atomic.Int32
	return func(context.Context) ([][]byte, error) { calls.Add(1); return datagrams, nil }, &calls
}

func TestMonitorReportsAffectedAndChecksOnlyOnItsInterval(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	var current atomic.Pointer[[][]byte]
	merged := [][]byte{datagram(route(), done())}
	current.Store(&merged)
	var calls atomic.Int32
	dump := func(context.Context) ([][]byte, error) { calls.Add(1); return *current.Load(), nil }
	m := testMonitor(dump, c, nil)

	hang := m.Status(context.Background())
	if hang == nil || hang.Kernel != "5.15.0-203-generic" {
		t.Fatalf("hang = %+v, want one with the kernel", hang)
	}
	if again := m.Status(context.Background()); again == nil || calls.Load() != 1 {
		t.Fatalf("a second status inside the interval probed again (%d probes)", calls.Load())
	}
	// A sysctl changed in between: the next round clears the fact.
	fixed := [][]byte{datagram(done())}
	current.Store(&fixed)
	c.advance(14 * time.Minute)
	if m.Status(context.Background()) == nil || calls.Load() != 1 {
		t.Fatal("probed before the interval passed")
	}
	c.advance(2 * time.Minute)
	if got := m.Status(context.Background()); got != nil || calls.Load() != 2 {
		t.Fatalf("hang = %+v after %d probes, want the fact cleared by the second", got, calls.Load())
	}
}

func TestMonitorKeepsTheLastAnswerWhenAProbeFailsAndTriesAgainSoon(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	var fail atomic.Bool
	var calls atomic.Int32
	merged := [][]byte{datagram(route(), done())}
	dump := func(context.Context) ([][]byte, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("recv: connection reset")
		}
		return merged, nil
	}
	m := testMonitor(dump, c, nil)
	if m.Status(context.Background()) == nil {
		t.Fatal("first probe did not find the hang")
	}
	fail.Store(true)
	c.advance(16 * time.Minute)
	if m.Status(context.Background()) == nil {
		t.Fatal("a failed probe cleared a known fact")
	}
	c.advance(30 * time.Second)
	m.Status(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("probed again after 30s of a 1 minute retry (%d probes)", calls.Load())
	}
	fail.Store(false)
	c.advance(time.Minute)
	if m.Status(context.Background()) == nil || calls.Load() != 3 {
		t.Fatalf("no retry after the retry delay (%d probes)", calls.Load())
	}
}

func TestMonitorUnknownProbeIsNotReportedAsAffected(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	dump, _ := answers(datagram(done()))
	// no routes, not affected, and known
	m := testMonitor(dump, c, nil)
	if hang, known := m.Fresh(context.Background()); hang != nil || !known {
		t.Fatalf("hang=%+v known=%t, want not affected and known", hang, known)
	}
	failing := testMonitor(func(context.Context) ([][]byte, error) { return nil, errors.New("boom") }, c, nil)
	if hang, known := failing.Fresh(context.Background()); hang != nil || known {
		t.Fatalf("hang=%+v known=%t, want unknown", hang, known)
	}
	off := testMonitor(func(context.Context) ([][]byte, error) { return nil, ErrUnsupported }, c, nil)
	if hang, known := off.Fresh(context.Background()); hang != nil || known {
		t.Fatalf("hang=%+v known=%t on an OS without netlink, want unknown", hang, known)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A receive that never returns is abandoned at the deadline and counts as
// unknown, never as affected; the next tick, while that probe is still out,
// is skipped with one log line rather than starting a second probe.
func TestMonitorAbandonsAProbeThatNeverReturnsAndSkipsTicksWhileItIsOut(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	release := make(chan struct{})
	var calls atomic.Int32
	hung := func(context.Context) ([][]byte, error) {
		calls.Add(1)
		<-release // ignores its context: a recv stuck in the kernel
		return nil, errors.New("released")
	}
	logs := &lockedBuffer{}
	m := testMonitor(hung, c, slog.New(slog.NewTextHandler(logs, nil)))
	m.Timeout = 50 * time.Millisecond

	start := time.Now()
	hang, known := m.Fresh(context.Background())
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the caller waited %s for a probe that never returns", took)
	}
	if hang != nil || known {
		t.Fatalf("hang=%+v known=%t, want unknown and not affected", hang, known)
	}
	for i := 0; i < 3; i++ {
		start := time.Now()
		if hang, known := m.Fresh(context.Background()); hang != nil || known || time.Since(start) > 20*time.Millisecond {
			t.Fatalf("tick %d: hang=%+v known=%t after %s, want an immediate skip", i, hang, known, time.Since(start))
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d probes started while the first was out, want 1", calls.Load())
	}
	if n := strings.Count(logs.String(), "has not returned; skipping"); n != 1 {
		t.Fatalf("the skip was logged %d times, want once:\n%s", n, logs.String())
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for m.gate.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the gate never opened after the abandoned probe returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// With the old probe gone, a later round runs again.
	c.advance(2 * time.Minute)
	m.Dump, _ = answers(datagram(route(), done()))
	if hang, known := m.Fresh(context.Background()); hang == nil || !known {
		t.Fatalf("hang=%+v known=%t after the gate opened, want the new probe's answer", hang, known)
	}
}

func TestMonitorWithoutADumperKnowsNothing(t *testing.T) {
	m := &Monitor{Now: time.Now}
	if hang, known := m.Fresh(context.Background()); hang != nil || known {
		t.Fatalf("hang=%+v known=%t", hang, known)
	}
}
