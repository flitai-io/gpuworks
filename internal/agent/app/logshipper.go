package app

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/logline"
	"gpuworks.ai/gpuworks/internal/logtext"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// logDrainQuiet is how long a draining follower waits for another line
	// before deciding the container's output is all shipped. `tail -F` without
	// inotify polls once a second, so anything shorter can cut it off.
	logDrainQuiet = time.Second
	// logDrainMax bounds a drain whose container keeps writing.
	logDrainMax = 3 * time.Second
	// finalLogLines is how much of a never-followed container's output a
	// removal ships: enough for a usage text or a Python traceback.
	finalLogLines = 200
)

// instanceLogs is the per-instance state the log shipper and container
// removals share.
type instanceLogs struct {
	mu   sync.Mutex
	byID map[string]*instanceLog
	// quiet and max override logDrainQuiet and logDrainMax in tests.
	quiet, max time.Duration
}

type instanceLog struct {
	run *logFollower // nil while no follower runs
	// streamed: a follower opened this container's log stream, so its output
	// has been shipped from then on and a removal must not ship it again.
	streamed bool
	// removing: a drain holds the instance until its container is removed.
	// A follower started meanwhile would ship the tail a second time.
	removing bool
}

// logFollower is one follower goroutine's handle.
type logFollower struct {
	ctx    context.Context
	cancel context.CancelFunc
	// drain asks the follower to ship what is left and stop; it closes the
	// channel it receives once done.
	drain chan chan struct{}
	// opened is set once the follower has a log stream.
	opened atomic.Bool
	// release runs when the follower goroutine exits.
	release func()
}

func (l *instanceLogs) drainQuiet() time.Duration {
	if l.quiet > 0 {
		return l.quiet
	}
	return logDrainQuiet
}

func (l *instanceLogs) drainMax() time.Duration {
	if l.max > 0 {
		return l.max
	}
	return logDrainMax
}

// wantsFollower reports whether uuid needs a follower started.
func (l *instanceLogs) wantsFollower(uuid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.byID[uuid]
	return rec == nil || (rec.run == nil && !rec.removing)
}

// start registers a follower for uuid, or returns nil when one was started or
// a removal began since wantsFollower.
func (l *instanceLogs) start(ctx context.Context, uuid string) *logFollower {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = map[string]*instanceLog{}
	}
	rec := l.byID[uuid]
	if rec == nil {
		rec = &instanceLog{}
		l.byID[uuid] = rec
	}
	if rec.run != nil || rec.removing {
		return nil
	}
	fctx, cancel := context.WithCancel(ctx)
	f := &logFollower{ctx: fctx, cancel: cancel, drain: make(chan chan struct{})}
	f.release = func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if rec.run == f {
			rec.run = nil
		}
	}
	rec.run = f
	return f
}

// markStreamed records that f opened its instance's log stream.
func (l *instanceLogs) markStreamed(uuid string, f *logFollower) {
	f.opened.Store(true)
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec := l.byID[uuid]; rec != nil && rec.run == f {
		rec.streamed = true
	}
}

// forgetStopped stops the followers of instances no longer running and drops
// their state; removals in progress keep theirs until they finish.
func (l *instanceLogs) forgetStopped(running map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for uuid, rec := range l.byID {
		if running[uuid] || rec.removing {
			continue
		}
		if rec.run != nil {
			rec.run.cancel()
		}
		delete(l.byID, uuid)
	}
}

// shipLogs follows each running instance's logs and pushes batches for
// persistence + search. One follower per instance, started as they appear.
func (n *NodeRuntime) shipLogs(ctx context.Context) {
	n.runLogShipper(ctx, 5*time.Second, logShipperDeps{
		running: func() []string {
			var out []string
			for _, s := range n.engine.RunningSpecs() {
				out = append(out, s.UUID)
			}
			return out
		},
		containerExists: n.instanceContainerExists,
		follow: func(uuid string, f *logFollower) {
			go n.followOne(f.ctx, uuid, f)
		},
	})
}

// logShipperDeps isolates the engine touchpoints of the shipper loop so
// the restart behavior can be exercised without an engine.
type logShipperDeps struct {
	running func() []string
	// containerExists gates follower startup on the runtime actually having
	// the container: pulling instances sit in the desired set long before
	// one exists, and a follower would hammer `logs` against a missing id
	// every reconcile tick. Nil skips the gate (tests, drivers without
	// Status).
	containerExists func(ctx context.Context, uuid string) bool
	follow          func(uuid string, f *logFollower)
}

// runLogShipper holds one follower per running instance. A follower that exits
// while its instance is still running — a container restart closes the old
// stream, and the restarted container keeps producing logs under the same
// instance UUID — is released here so the next tick starts a fresh one. Before
// the release hook, a stream end silently marked the UUID as followed forever
// and the instance shipped no further logs.
func (n *NodeRuntime) runLogShipper(ctx context.Context, tick time.Duration, deps logShipperDeps) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			running := map[string]bool{}
			for _, uuid := range deps.running() {
				running[uuid] = true
				if !n.instanceLogs.wantsFollower(uuid) {
					continue
				}
				if deps.containerExists != nil && !deps.containerExists(ctx, uuid) {
					continue
				}
				if f := n.instanceLogs.start(ctx, uuid); f != nil {
					deps.follow(uuid, f)
				}
			}
			n.instanceLogs.forgetStopped(running)
		}
	}
}

// drainInstanceLogs is the engine's LogDrain: called after a container has
// stopped and before it is removed, it makes sure the output removal is about
// to delete has reached the server.
//
// A follower that has the stream ships what is left and stops. Without one,
// the container had no follower yet - the shipper ticks every few seconds, and
// an engine that cannot even load its libraries exits well within that - so
// its last lines are read and shipped here, but only when it exited on its
// own: a container stopped by the removal while nobody followed it (asleep,
// or just adopted after an agent restart) has nothing to report that a
// follower did not already ship.
func (n *NodeRuntime) drainInstanceLogs(ctx context.Context, uuid string, exited bool) func() {
	l := &n.instanceLogs
	l.mu.Lock()
	if l.byID == nil {
		l.byID = map[string]*instanceLog{}
	}
	rec := l.byID[uuid]
	if rec == nil {
		rec = &instanceLog{}
		l.byID[uuid] = rec
	}
	rec.removing = true
	f, streamed := rec.run, rec.streamed
	l.mu.Unlock()

	switch {
	case f != nil && f.opened.Load():
		l.finish(ctx, f)
	default:
		if f != nil {
			f.cancel()
		}
		if exited && !streamed {
			n.shipFinalLogs(ctx, uuid)
		}
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.byID[uuid] == rec {
			delete(l.byID, uuid)
		}
	}
}

// finish asks f to ship what is left, waits for it, and stops it.
func (l *instanceLogs) finish(ctx context.Context, f *logFollower) {
	defer f.cancel()
	done := make(chan struct{})
	select {
	case f.drain <- done:
	case <-f.ctx.Done():
		return
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
		// The follower is between its stream ending and its exit.
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(l.drainMax() + time.Second):
	}
}

// shipFinalLogs ships the last lines of a container no follower streamed.
func (n *NodeRuntime) shipFinalLogs(ctx context.Context, uuid string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rc, err := n.engine.Driver().Logs(ctx, n.engine.ContainerNameFor(uuid), false, finalLogLines)
	if err != nil {
		n.log.Warn("read exited container's logs before removal", "instance", uuid, "err", err)
		return
	}
	defer rc.Close()
	sc := logtext.NewScanner(rc)
	// Drivers honor the tail; the window guards against one that does not.
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 2*finalLogLines {
			lines = append(lines[:0], lines[len(lines)-finalLogLines:]...)
		}
	}
	if len(lines) > finalLogLines {
		lines = lines[len(lines)-finalLogLines:]
	}
	batch := make([]proto.LogLine, 0, 64)
	for _, line := range lines {
		if ll, ok := n.instanceLogLine(uuid, line); ok {
			batch = append(batch, ll)
		}
		if len(batch) == 64 {
			n.emit(proto.TLogs, proto.LogBatch{Lines: batch})
			batch = make([]proto.LogLine, 0, 64)
		}
	}
	if len(batch) > 0 {
		n.emit(proto.TLogs, proto.LogBatch{Lines: batch})
	}
}

// instanceLogLine stamps one line of container output for shipping. Access-log
// noise (the platform's own scraper traffic) is dropped rather than shipped:
// on a live deployment it was 52% of all stored rows while carrying no
// diagnostic signal. Application endpoints never match the filter.
func (n *NodeRuntime) instanceLogLine(uuid, line string) (proto.LogLine, bool) {
	if logline.IsNoise(line) {
		if dropped := n.logNoiseDropped.Add(1); dropped%10000 == 1 {
			n.log.Info("dropping scraper access-log noise", "instance", uuid, "dropped", dropped)
		}
		return proto.LogLine{}, false
	}
	return proto.LogLine{Instance: uuid, TimeMS: time.Now().UnixMilli(), Line: line, Level: logline.Level(line), Source: "instance"}, true
}

// instanceContainerExists reports whether the instance's container exists
// yet. Optimistic on any doubt (no engine, probe error): a flaky probe must
// not stop log shipping, and followOne's own retry still covers a container
// that disappears right after the check.
func (n *NodeRuntime) instanceContainerExists(ctx context.Context, uuid string) bool {
	driver := n.engine.Driver()
	if driver == nil {
		return true
	}
	st, err := driver.Status(ctx, n.engine.ContainerNameFor(uuid))
	if err != nil {
		return true
	}
	return st.Exists
}

// followOne ships one instance's container output until its context ends, its
// stream ends, or a drain has shipped the rest. f is nil in tests that drive
// the follower without a shipper.
func (n *NodeRuntime) followOne(ctx context.Context, uuid string, f *logFollower) {
	var drainReq chan chan struct{}
	if f != nil {
		defer f.release()
		drainReq = f.drain
	}
	name := n.engine.ContainerNameFor(uuid)
	var rc io.ReadCloser
	for {
		var err error
		rc, err = n.engine.Driver().Logs(ctx, name, true, 50)
		if err == nil {
			break
		}
		// Desired state is visible before a slow image pull/create has produced
		// the runtime log file. Keep this follower alive so startup failures are
		// still shipped after the file appears; otherwise the first ENOENT marks
		// the UUID as followed forever and L4 diagnostics lose the real stderr.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	defer rc.Close()
	go func() { <-ctx.Done(); _ = rc.Close() }()
	if f != nil {
		n.instanceLogs.markStreamed(uuid, f)
	}

	sc := logtext.NewScanner(rc)
	batch := make([]proto.LogLine, 0, 64)
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()

	lineCh := make(chan string, 256)
	go func() {
		for sc.Scan() {
			select {
			case lineCh <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		close(lineCh)
	}()

	send := func() {
		if len(batch) == 0 {
			return
		}
		n.emit(proto.TLogs, proto.LogBatch{Lines: batch})
		// Hand over ownership: the connection may marshal the batch
		// asynchronously, so the next lines must append into a fresh slice
		// instead of reusing the handed-off backing array.
		batch = make([]proto.LogLine, 0, 64)
	}
	// While draining, the follower stops once the stream has been quiet for
	// a while or the drain has run its course.
	var drained chan struct{}
	var quiet *time.Timer
	var quietC, deadlineC <-chan time.Time
	finishDrain := func() {
		send()
		close(drained)
	}
	for {
		select {
		case <-ctx.Done():
			send()
			return
		case drained = <-drainReq:
			drainReq = nil
			quiet = time.NewTimer(n.instanceLogs.drainQuiet())
			defer quiet.Stop()
			quietC = quiet.C
			deadlineC = time.After(n.instanceLogs.drainMax())
		case <-quietC:
			finishDrain()
			return
		case <-deadlineC:
			finishDrain()
			return
		case line, ok := <-lineCh:
			if !ok {
				if drained != nil {
					finishDrain()
					return
				}
				send()
				// A stream end while the instance is still running is a
				// container restart (or runtime hiccup); the shipper will
				// re-follow on its next tick. Count it so the gap is visible.
				if ctx.Err() == nil {
					restarts := n.logFollowerRestarts.Add(1)
					n.log.Warn("instance log stream ended; re-following", "instance", uuid, "restarts", restarts)
				}
				return
			}
			if quiet != nil {
				quiet.Reset(n.instanceLogs.drainQuiet())
			}
			// Real lines are never dropped — a slow consumer backpressures
			// instead, which the runtime pipe propagates to `logs -f`.
			ll, ok := n.instanceLogLine(uuid, line)
			if !ok {
				continue
			}
			batch = append(batch, ll)
			if len(batch) >= 64 {
				send()
			}
		case <-flush.C:
			send()
		}
	}
}
