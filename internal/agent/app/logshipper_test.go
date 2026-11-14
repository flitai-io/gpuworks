package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// drainTestDriver serves logs the way the udocker driver does: a follow is an
// endless `tail -F` stream that only the caller's context ends (or, with
// followEOF, a `docker logs -f` that ends when the container has exited), and
// a non-follow read returns the file. It records every read.
type drainTestDriver struct {
	imagePullTestDriver
	mu         sync.Mutex
	final      string // what a non-follow read returns; tail is ignored
	finalTails []int
	follows    int
	followEOF  string
	stream     *io.PipeWriter
}

func (d *drainTestDriver) Logs(ctx context.Context, _ string, follow bool, tail int) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !follow {
		d.finalTails = append(d.finalTails, tail)
		return io.NopCloser(strings.NewReader(d.final)), nil
	}
	d.follows++
	if d.followEOF != "" {
		return io.NopCloser(strings.NewReader(d.followEOF)), nil
	}
	pr, pw := io.Pipe()
	d.stream = pw
	go func() {
		<-ctx.Done()
		pw.Close()
	}()
	return pr, nil
}

func (d *drainTestDriver) write(t *testing.T, lines ...string) {
	t.Helper()
	d.mu.Lock()
	pw := d.stream
	d.mu.Unlock()
	for _, line := range lines {
		if _, err := pw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write %q to the followed stream: %v", line, err)
		}
	}
}

func (d *drainTestDriver) counts() (follows int, finalTails []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.follows, append([]int(nil), d.finalTails...)
}

// instanceLinesSink collects every instance line the node emits.
type instanceLinesSink struct {
	mu    sync.Mutex
	lines []proto.LogLine
}

func (s *instanceLinesSink) bind(n *NodeRuntime) {
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			s.mu.Lock()
			s.lines = append(s.lines, body.(proto.LogBatch).Lines...)
			s.mu.Unlock()
		}
		return nil
	})
}

func (s *instanceLinesSink) text() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	for i, l := range s.lines {
		out[i] = l.Line
	}
	return out
}

func (s *instanceLinesSink) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Join(s.text(), "|") == strings.Join(want, "|") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("shipped %q, want %q", s.text(), want)
}

func newDrainTestNode(t *testing.T, driver *drainTestDriver) (*NodeRuntime, *instanceLinesSink) {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir()+"/models", "", metricsOnlyProber{}, nil)
	n.instanceLogs.quiet = 50 * time.Millisecond
	n.instanceLogs.max = time.Second
	shipped := &instanceLinesSink{}
	shipped.bind(n)
	return n, shipped
}

// The ninfer case: the engine could not load a library, printed one line and
// exited within two seconds - before the shipper's first tick, and removal
// deletes the output with the container. The drain must ship it.
func TestDrainShipsTheLastLinesOfAContainerThatExitedUnfollowed(t *testing.T) {
	driver := &drainTestDriver{}
	var lines []string
	for i := 1; i <= finalLogLines+50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	// Scraper noise is dropped here exactly as on the follower path.
	lines = append(lines, `(APIServer pid=1) INFO:     127.0.0.1:49254 - "GET /metrics HTTP/1.1" 200 OK`)
	lines = append(lines, "ninfer-serve: error while loading shared libraries: libcudart.so.13: cannot open shared object file")
	driver.final = strings.Join(lines, "\n") + "\n"
	n, shipped := newDrainTestNode(t, driver)

	n.drainInstanceLogs(t.Context(), "inst-quick-exit", true)()

	got := shipped.text()
	if len(got) != finalLogLines-1 {
		t.Fatalf("shipped %d lines, want the last %d minus the noise line", len(got), finalLogLines)
	}
	// The window is the last 200 lines read: line 53 … line 250, the noise
	// line and the loader error.
	if got[0] != "line 53" || got[len(got)-1] != lines[len(lines)-1] {
		t.Fatalf("shipped %q … %q, want line 53 … the loader error", got[0], got[len(got)-1])
	}
	shipped.mu.Lock()
	last := shipped.lines[len(shipped.lines)-1]
	shipped.mu.Unlock()
	if last.Instance != "inst-quick-exit" || last.Source != "instance" || last.TimeMS == 0 {
		t.Fatalf("last line = %+v, want an instance line of inst-quick-exit", last)
	}
	if _, tails := driver.counts(); len(tails) != 1 || tails[0] != finalLogLines {
		t.Fatalf("non-follow reads = %v, want one asking for the last %d lines", tails, finalLogLines)
	}
}

// A container the removal itself stopped, with no follower (asleep, or just
// adopted after an agent restart), exited for no reason worth reporting; its
// output is not read again.
func TestDrainShipsNothingForAContainerTheRemovalStopped(t *testing.T) {
	driver := &drainTestDriver{final: "already shipped by an earlier agent\n"}
	n, shipped := newDrainTestNode(t, driver)

	n.drainInstanceLogs(t.Context(), "inst-asleep", false)()

	if got := shipped.text(); len(got) != 0 {
		t.Fatalf("shipped %q for a container the removal stopped", got)
	}
	if _, tails := driver.counts(); len(tails) != 0 {
		t.Fatalf("non-follow reads = %v, want none", tails)
	}
}

// A container whose follower already streamed it to the end (`docker logs -f`
// returns once the container exits) has nothing left to ship; reading its
// tail again would only duplicate it.
func TestDrainDoesNotReshipAContainerAFollowerStreamed(t *testing.T) {
	driver := &drainTestDriver{followEOF: "starting\ncrashed\n", final: "starting\ncrashed\n"}
	n, shipped := newDrainTestNode(t, driver)
	f := n.instanceLogs.start(t.Context(), "inst-streamed")
	n.followOne(f.ctx, "inst-streamed", f)
	shipped.waitFor(t, "starting", "crashed")

	n.drainInstanceLogs(t.Context(), "inst-streamed", true)()

	shipped.waitFor(t, "starting", "crashed")
	if _, tails := driver.counts(); len(tails) != 0 {
		t.Fatalf("non-follow reads = %v, want none", tails)
	}
}

// A follower that has the stream ships what the container writes until it
// goes quiet, then stops; while the removal runs no new follower starts, and
// once it is over a container still running under the same instance is
// followed again.
func TestDrainLetsTheFollowerShipWhatIsLeft(t *testing.T) {
	driver := &drainTestDriver{}
	n, shipped := newDrainTestNode(t, driver)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go n.runLogShipper(ctx, 10*time.Millisecond, logShipperDeps{
		running: func() []string { return []string{"inst-followed"} },
		follow: func(uuid string, f *logFollower) {
			go n.followOne(f.ctx, uuid, f)
		},
	})
	waitForFollows(t, driver, 1)
	waitStreamed(t, n, "inst-followed")
	driver.write(t, "serving")

	drained := make(chan func())
	go func() { drained <- n.drainInstanceLogs(t.Context(), "inst-followed", false) }()
	// Lines written once the container stopped, before the stream went quiet.
	time.Sleep(10 * time.Millisecond)
	driver.write(t, "shutting down", "server stopped")
	var release func()
	select {
	case release = <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not finish")
	}
	shipped.waitFor(t, "serving", "shutting down", "server stopped")
	if _, tails := driver.counts(); len(tails) != 0 {
		t.Fatalf("non-follow reads = %v, want the follower to ship everything", tails)
	}

	time.Sleep(50 * time.Millisecond)
	if follows, _ := driver.counts(); follows != 1 {
		t.Fatalf("follows = %d during the removal, want no new follower", follows)
	}
	release()
	waitForFollows(t, driver, 2)
}

// A container that never stops writing is drained for logDrainMax at most.
func TestDrainOfAChattyContainerIsBounded(t *testing.T) {
	driver := &drainTestDriver{}
	n, _ := newDrainTestNode(t, driver)
	n.instanceLogs.max = 200 * time.Millisecond
	f := n.instanceLogs.start(t.Context(), "inst-chatty")
	go n.followOne(f.ctx, "inst-chatty", f)
	waitForFollows(t, driver, 1)
	waitStreamed(t, n, "inst-chatty")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			driver.mu.Lock()
			pw := driver.stream
			driver.mu.Unlock()
			if _, err := fmt.Fprintf(pw, "tick %d\n", i); err != nil {
				return
			}
		}
	}()

	start := time.Now()
	n.drainInstanceLogs(t.Context(), "inst-chatty", false)()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("drain took %v, want it bounded by the drain maximum", took)
	}
	select {
	case <-f.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the drained follower was not stopped")
	}
}

// A follower still waiting for the log to appear has shipped nothing, so a
// container that exited is read once.
func TestDrainReadsTheTailWhenTheFollowerHadNoStreamYet(t *testing.T) {
	driver := &drainTestDriver{final: "engine boot\nfatal startup detail\n"}
	n, shipped := newDrainTestNode(t, driver)
	// The follower was registered but has not opened a stream.
	f := n.instanceLogs.start(t.Context(), "inst-waiting")

	n.drainInstanceLogs(t.Context(), "inst-waiting", true)()

	select {
	case <-f.ctx.Done():
	default:
		t.Fatal("the waiting follower was not stopped")
	}
	shipped.waitFor(t, "engine boot", "fatal startup detail")
}

func waitForFollows(t *testing.T, driver *drainTestDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if follows, _ := driver.counts(); follows >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	follows, _ := driver.counts()
	t.Fatalf("follows = %d, want %d", follows, want)
}

func waitStreamed(t *testing.T, n *NodeRuntime, uuid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n.instanceLogs.mu.Lock()
		rec := n.instanceLogs.byID[uuid]
		streamed := rec != nil && rec.streamed
		n.instanceLogs.mu.Unlock()
		if streamed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s was never streamed", uuid)
}

// tqdm redraws its bar with '\r' and ends the line only when it closes, so the
// followed stream delivers every redraw as one line. The log center stores
// what a terminal shows: the final frame, with the lines around it intact.
func TestFollowerShipsTheFinalProgressFrame(t *testing.T) {
	driver := &drainTestDriver{}
	n, shipped := newDrainTestNode(t, driver)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go n.runLogShipper(ctx, 10*time.Millisecond, logShipperDeps{
		running: func() []string { return []string{"inst-vllm"} },
		follow: func(uuid string, f *logFollower) {
			go n.followOne(f.ctx, uuid, f)
		},
	})
	waitForFollows(t, driver, 1)
	waitStreamed(t, n, "inst-vllm")
	driver.write(t,
		"(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s",
		"Capturing CUDA graphs (FULL):   0%|          | 0/2 [00:00<?, ?it/s]\r"+
			"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.17it/s]\r"+
			"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.14it/s]",
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs, took 0.05 GiB")
	shipped.waitFor(t,
		"(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s",
		"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.14it/s]",
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs, took 0.05 GiB")
}

// The live log view (logtail) gets the same terminal view as the log center:
// the vLLM CUDA-graph bar arrives as its final frame, CRLF loses its '\r', and
// a last line the container left unterminated is still delivered.
func TestStreamLogsCollapsesProgressBarRedraws(t *testing.T) {
	driver := &drainTestDriver{followEOF: "(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s\n" +
		"Capturing CUDA graphs (FULL):   0%|          | 0/2 [00:00<?, ?it/s]\r" +
		"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.17it/s]\r" +
		"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.14it/s]\n" +
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs\r\n" +
		"\tunterminated"}
	n, _ := newDrainTestNode(t, driver)
	var out strings.Builder
	if err := n.StreamLogs(t.Context(), "inst-vllm", true, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	want := "(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s\n" +
		"Capturing CUDA graphs (FULL): 100%|██████████| 2/2 [00:00<00:00, 18.14it/s]\n" +
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs\n" +
		"\tunterminated\n"
	if out.String() != want {
		t.Fatalf("streamed\n%q\nwant\n%q", out.String(), want)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A server that hung up mid-stream surfaces as the write error, not success.
func TestStreamLogsReturnsTheWriteError(t *testing.T) {
	driver := &drainTestDriver{followEOF: "one\ntwo\n"}
	n, _ := newDrainTestNode(t, driver)
	hungUp := errors.New("stream closed")
	if err := n.StreamLogs(t.Context(), "inst", true, failingWriter{err: hungUp}); !errors.Is(err, hungUp) {
		t.Fatalf("StreamLogs = %v, want %v", err, hungUp)
	}
}
