package modelsync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type rateLimitSink struct{ writes atomic.Int32 }

func (w *rateLimitSink) Write(p []byte) (int, error) { w.writes.Add(1); return len(p), nil }

func TestRateLimitedWriterCancellationInterruptsPendingAllowance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &rateLimitSink{}
	limited := RateLimitedWriter(ctx, sink, 1)
	payload := make([]byte, 1024)
	if n, err := limited.Write(payload); n != len(payload) || err != nil {
		t.Fatalf("initial write=%d %v", n, err)
	}
	result := make(chan error, 1)
	go func() {
		n, err := limited.Write(payload)
		if n != 0 {
			result <- errors.New("canceled allowance wrote archive bytes")
			return
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("rate limit did not hold second write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled write=%v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("rate limit wait ignores cancellation")
	}
	if writes := sink.writes.Load(); writes != 1 {
		t.Fatalf("underlying writes=%d want initial write only", writes)
	}
	if n, err := limited.Write(payload); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("later canceled write=%d %v", n, err)
	}
	if writes := sink.writes.Load(); writes != 1 {
		t.Fatalf("later cancellation wrote to underlying transport: %d", writes)
	}
}

func TestRateLimitedWriterUnlimitedPreservesWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, rate := range []int{0, -1} {
		sink := &rateLimitSink{}
		writer := RateLimitedWriter(ctx, sink, rate)
		if writer != sink {
			t.Fatal("unlimited path wrapped the original writer")
		}
		if n, err := writer.Write([]byte("unlimited")); n != 9 || err != nil {
			t.Fatalf("unlimited write=%d %v", n, err)
		}
	}
}

func TestRateLimitedWriterAllowsWriteWhenTimerExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sink := &rateLimitSink{}
	limited := RateLimitedWriter(ctx, sink, 64)
	payload := make([]byte, 1024)
	for i := 0; i < 2; i++ {
		if n, err := limited.Write(payload); n != len(payload) || err != nil {
			t.Fatalf("write after allowance=%d %v", n, err)
		}
	}
	if got := sink.writes.Load(); got != 2 {
		t.Fatalf("underlying writes=%d", got)
	}
}
