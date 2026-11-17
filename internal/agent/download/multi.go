package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// chunk is one contiguous byte range of the file, fetched by a single worker.
type chunk struct {
	index int
	start int64
	end   int64 // inclusive
	done  int64 // absolute offset of the next byte to write; end+1 == complete
}

// downloadState is the JSON sidecar recording per-chunk progress so an
// interrupted multi-connection download can resume.
type downloadState struct {
	URL    string  `json:"url"` // Options.ResumeKey, which defaults to the URL
	Size   int64   `json:"size"`
	Chunks []int64 `json:"chunks"` // done offset per chunk
}

// multiDownload splits [0,size) into contiguous chunks fetched in parallel,
// each written at its correct offset via WriteAt into a preallocated .part file.
func multiDownload(ctx context.Context, opts Options, size int64, partPath, statePath string, limiter *rateLimiter, onProgress func(Progress)) error {
	n := opts.Concurrency
	if int64(n) > size {
		n = int(size)
	}
	if n < 1 {
		n = 1
	}

	// A sidecar describes offsets in one concrete part file. If that file was
	// deleted or truncated, trusting the sidecar would let every "completed"
	// chunk skip its request and rename an empty sparse file into the model.
	resumable := partHasExpectedSize(partPath, size)
	if !resumable {
		_ = os.Remove(statePath)
	}
	f, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}

	chunks := splitChunks(n, size)
	if resumable {
		loadState(statePath, opts.resumeKey(), size, chunks)
	}

	// Aggregate counter seeded with any already-completed bytes from resume.
	var agg int64
	for _, c := range chunks {
		agg += atomic.LoadInt64(&c.done) - c.start
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rep := startReporter(wctx, size, func() int64 { return atomic.LoadInt64(&agg) }, onProgress)
	stopState := startStateWriter(statePath, opts.resumeKey(), size, chunks)

	var wg sync.WaitGroup
	errCh := make(chan error, len(chunks))
	for _, c := range chunks {
		wg.Add(1)
		go func(c *chunk) {
			defer wg.Done()
			if err := fetchChunk(wctx, opts, c, f, limiter, &agg); err != nil {
				// The channel is buffered for every worker, so record the root
				// failure before canceling siblings. Otherwise a sibling can win
				// the race with context.Canceled and hide a retryable transport
				// error such as io.ErrUnexpectedEOF.
				errCh <- err
				cancel()
			}
		}(c)
	}
	wg.Wait()
	stopState()
	rep.stop()
	close(errCh)

	if err := firstErr(errCh); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return finalize(f, partPath, opts.Dest, statePath)
}

func partHasExpectedSize(partPath string, size int64) bool {
	info, err := os.Stat(partPath)
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}

// splitChunks divides [0,size) into n contiguous chunks.
func splitChunks(n int, size int64) []*chunk {
	chunkSize := size / int64(n)
	chunks := make([]*chunk, 0, n)
	for i := 0; i < n; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == n-1 {
			end = size - 1
		}
		chunks = append(chunks, &chunk{index: i, start: start, end: end, done: start})
	}
	return chunks
}

// fetchChunk downloads the remaining bytes of one chunk with a Range request.
func fetchChunk(ctx context.Context, opts Options, c *chunk, f *os.File, limiter *rateLimiter, agg *int64) error {
	off := atomic.LoadInt64(&c.done)
	if off > c.end {
		return nil // already complete (resumed)
	}

	resp, err := guardedGet(ctx, opts, "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(c.end, 10))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return statusError(resp, fmt.Sprintf("download: chunk %d expected 206, got %d", c.index, resp.StatusCode))
	}
	rangeStart, rangeEnd, ok := parseContentRangeBounds(resp.Header.Get("Content-Range"))
	if !ok || rangeStart != off || rangeEnd != c.end {
		return fmt.Errorf("download: chunk %d response range %q does not match bytes %d-%d", c.index, resp.Header.Get("Content-Range"), off, c.end)
	}

	buf := make([]byte, readBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nr, rerr := resp.Body.Read(buf)
		if nr > 0 {
			woff := atomic.LoadInt64(&c.done)
			if int64(nr) > c.end+1-woff {
				return fmt.Errorf("download: chunk %d response exceeded bytes %d-%d", c.index, off, c.end)
			}
			if _, err := f.WriteAt(buf[:nr], woff); err != nil {
				return err
			}
			atomic.AddInt64(&c.done, int64(nr))
			atomic.AddInt64(agg, int64(nr))
			if err := limiter.Wait(ctx, nr); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			if done := atomic.LoadInt64(&c.done); done != c.end+1 {
				return fmt.Errorf("download: chunk %d ended at byte %d, want %d: %w", c.index, done, c.end+1, io.ErrUnexpectedEOF)
			}
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func parseContentRangeBounds(value string) (start, end int64, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, false
	}
	bounds, _, found := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !found {
		return 0, 0, false
	}
	startText, endText, found := strings.Cut(strings.TrimSpace(bounds), "-")
	if !found {
		return 0, 0, false
	}
	start, errStart := strconv.ParseInt(strings.TrimSpace(startText), 10, 64)
	end, errEnd := strconv.ParseInt(strings.TrimSpace(endText), 10, 64)
	if errStart != nil || errEnd != nil || start < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// loadState populates chunk done offsets from a valid sidecar, if present.
func loadState(statePath, url string, size int64, chunks []*chunk) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	var st downloadState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	if st.URL != url || st.Size != size || len(st.Chunks) != len(chunks) {
		return
	}
	for i, c := range chunks {
		d := st.Chunks[i]
		if d < c.start {
			d = c.start
		}
		if d > c.end+1 {
			d = c.end + 1
		}
		c.done = d
	}
}

// startStateWriter periodically persists chunk progress and returns a stop func.
func startStateWriter(statePath, url string, size int64, chunks []*chunk) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	write := func() {
		st := downloadState{URL: url, Size: size, Chunks: make([]int64, len(chunks))}
		for i, c := range chunks {
			st.Chunks[i] = atomic.LoadInt64(&c.done)
		}
		writeState(statePath, st)
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				write()
				return
			case <-ticker.C:
				write()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func writeState(path string, st downloadState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func firstErr(ch <-chan error) error {
	var cancellation error
	for err := range ch {
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			if cancellation == nil {
				cancellation = err
			}
			continue
		}
		return err
	}
	return cancellation
}
