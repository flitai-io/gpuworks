package native

import (
	"context"
	"os"
	"sync"
)

// streamHandle tracks one consumer that is reading a blob while it downloads.
//
// The consumer's output is provisional: it is derived from bytes that have not
// been checksummed yet, so it may only be published once the blob it came from
// passes its digest check. That is why committing is explicit rather than
// implicit on exit -- finish publishes, stop discards, and whichever the caller
// reaches first decides.
type streamHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	commit func(verified bool)
	once   sync.Once
}

// startStream runs d.Stream against the contiguous prefix of the partially
// downloaded file at path. The consumer gets its own file descriptor: the
// download is still writing through another one, possibly at several offsets.
func (d *Downloader) startStream(ctx context.Context, desc Descriptor, path string, growth *growingFile) *streamHandle {
	streamCtx, cancel := context.WithCancel(ctx)
	handle := &streamHandle{cancel: cancel, done: make(chan struct{})}
	// Pin the file before returning: a fast pull may rename it to its digest
	// before the consumer goroutine is scheduled.
	file, err := os.Open(path)
	if err != nil {
		close(handle.done)
		return handle
	}
	go func() {
		defer close(handle.done)
		defer file.Close()
		commit, err := d.Stream(streamCtx, desc, newPrefixReader(file, growth))
		if err != nil {
			return // a failed consumer is not a failed download
		}
		handle.commit = commit
	}()
	return handle
}

// finish waits for the consumer to drain the blob and publishes its output. It
// blocks: when the consumer is slower than the network -- inflating a layer
// usually is -- this is where the remaining work is absorbed, which is the
// point of starting it early.
func (h *streamHandle) finish() {
	h.once.Do(func() {
		<-h.done
		if h.commit != nil {
			h.commit(true)
		}
	})
	h.cancel()
}

// stop abandons the consumer and discards its output. Safe to defer alongside
// finish; it becomes a no-op once either has run.
func (h *streamHandle) stop() {
	h.once.Do(func() {
		h.cancel()
		<-h.done
		if h.commit != nil {
			h.commit(false)
		}
	})
	h.cancel()
}
