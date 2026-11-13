package batch

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// cpuTime is the process's user and system CPU time so far.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Skip("getrusage unavailable: " + err.Error())
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// The batch executor runs inside the control-plane process, so its load has a
// budget: a full executor (32 batches at once) over many requests keeps the
// gateway-side in-flight count at the global cap, writes each batch's row a
// handful of times rather than per request, and holds memory and CPU down.
func TestExecutorScaleBudget(t *testing.T) {
	const (
		batches = 32
		cap     = 64
	)
	requests := 2000
	if raceEnabled {
		requests = 200
	}
	// A short sleep cannot prove saturation on a loaded scheduler. Hold the
	// first wave until the configured cap is reached, then measure the same
	// request workload. Cancellation still releases every blocked sender.
	var entered atomic.Int32
	firstWave := make(chan struct{})
	r := newRig(t, Config{PerBatch: 8, GlobalCap: cap, MaxRunning: batches}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		if entered.Add(1) == cap {
			close(firstWave)
		}
		select {
		case <-firstWave:
		case <-ctx.Done():
			return Reply{}
		}
		time.Sleep(50 * time.Microsecond)
		return okReply(`{"id":"x","choices":[{"finish_reason":"stop"}]}`)
	})
	r.start()
	content := inputLines(requests)
	file, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	cpuBefore := cpuTime(t)
	var peakHeap uint64
	var mu sync.Mutex
	stopSampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				mu.Lock()
				peakHeap = max(peakHeap, m.HeapAlloc)
				mu.Unlock()
			}
		}
	}()

	var stopOnce sync.Once
	stopSampler := func() {
		stopOnce.Do(func() { close(stopSampling) })
		<-sampled
	}
	t.Cleanup(stopSampler)

	var ids []string
	for i := 0; i < batches; i++ {
		b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: file.ID, Endpoint: EndpointChat, CompletionWindow: "24h"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	started := time.Now()
	for _, id := range ids {
		done := r.await(id, StatusCompleted)
		if done.Completed != requests || done.Failed != 0 {
			t.Fatalf("batch %s = %+v", id, done)
		}
	}
	elapsed := time.Since(started)
	stopSampler()
	cpu := cpuTime(t) - cpuBefore

	if peak := r.send.peakInFlight(); peak > cap || (!raceEnabled && peak < cap*3/4) {
		t.Fatalf("peak in-flight requests = %d, want the global cap %d held and nearly reached", peak, cap)
	}
	// Row writes: each batch writes its row on a start, a finalize and a
	// finish, plus at most a couple of checkpoints (the fake clock never
	// reaches the 5s period); never one per request.
	if saves := r.repo.saveCount(); saves > batches*3 {
		t.Fatalf("%d progress writes for %d requests (%d batches)", saves, batches*requests, batches)
	}
	total := batches * requests
	const heapBudget = 120 << 20 // above the base, measured at about 50
	if raceEnabled {
		return
	}
	if peakHeap > base.HeapAlloc+heapBudget {
		t.Fatalf("heap peaked at %d MiB over a %d MiB base; budget %d MiB for %d requests in %d batches",
			peakHeap>>20, base.HeapAlloc>>20, heapBudget>>20, total, batches)
	}
	// 64,000 requests through the executor: results written, files assembled.
	// Generous against slow CI machines; a per-request full scan or per-request
	// transaction costs orders of magnitude more.
	if budget := 15 * time.Second; cpu > budget {
		t.Fatalf("executor used %v CPU for %d requests (budget %v)", cpu, total, budget)
	}
	t.Logf("%d requests in %v wall, %v CPU, heap peak %d MiB over %d MiB base, %d progress writes",
		total, elapsed.Round(time.Millisecond), cpu.Round(time.Millisecond), peakHeap>>20, base.HeapAlloc>>20, r.repo.saveCount())
}
