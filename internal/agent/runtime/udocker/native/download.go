package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
)

// Timing of a blob transfer; variables so tests can shrink them.
var (
	// idleTimeout fires when a connection delivers nothing for this long. The
	// HTTP client has no overall deadline (multi-gigabyte blobs on throttled
	// links legitimately take many minutes), so this is what distinguishes a
	// slow transfer from a dead one.
	idleTimeout = 2 * time.Minute
	// slowWindow and slowMinRate recycle a connection that is alive but
	// crawling. A CDN connection that degrades to tens of KB/s rarely recovers
	// on its own, while a fresh one usually runs at line rate again, so waiting
	// for the idle timeout (which a trickle never trips) is the wrong call. The
	// transfer resumes where it stopped (see fetchSpan), so recycling loses
	// nothing.
	//
	// The judgement is made by the connection budget (connbudget.Lease.Observe)
	// against the other connections of the same pull: a connection that moved
	// under slowMinRate bytes per second on average over slowWindow, or far
	// under the median of its siblings, or collapsed from its own peak while
	// they held up. slowMinRate is the absolute floor and sits far below a slow
	// link's share of a pull's connections (16 KiB/s each is still under
	// 400 KiB/s across the default 16): mainland to an overseas CDN at peak is
	// tens to hundreds of KB/s in total, and a node that is merely slow must
	// not lose every connection every window. The budget also never recycles
	// more than half of a pull's open connections in one window.
	slowWindow  = 30 * time.Second
	slowMinRate = int64(16 << 10)
	// retryPause is the unit of the pause before a reconnect that follows a
	// failure which delivered nothing; it grows with consecutive such failures.
	retryPause = time.Second
)

const (
	// blobAttempts bounds consecutive failures that make no headway, both for
	// one connection's resumes and for a whole blob.
	blobAttempts = 3
	// throttleAttempts bounds consecutive "slow down" answers (429/503) one
	// span waits out. The pauses in between come from the connection budget
	// (the server's Retry-After, or a doubling default), so this is minutes of
	// patience, not a retry storm; past it the registry is not going to serve
	// this pull and the caller may try another source.
	throttleAttempts = 5
)

var (
	errIdle = errors.New("registry download stalled")
	// errSlow marks a connection recycled for crawling (see slowMinRate).
	errSlow = errors.New("registry download too slow")
	// errRangeIgnored marks a registry that answered a range request with the
	// whole blob.
	errRangeIgnored = errors.New("registry ignored range request")
	// errBlobMismatch marks a downloaded blob whose size or digest is not the
	// one its descriptor names; a fresh download may well match.
	errBlobMismatch = errors.New("blob does not match its descriptor")
)

// Downloader fetches blobs into a content-addressed directory. Layers are
// fetched concurrently and a single large layer is additionally split into
// byte ranges, because one TCP stream rarely saturates the link on the
// throttled nodes this driver targets.
//
// How many connections that really opens is not Workers x RangeParts: every
// request holds a token from the connection budget of the registry host (see
// Budget), which starts low, climbs while more connections still raise the
// pull's throughput, and backs off when the registry or a proxy pushes back.
type Downloader struct {
	Client     *Client
	Dir        string // layer store; files are named "sha256:<hex>"
	Workers    int    // layers in flight at once (each one's connections are budgeted)
	RangeParts int    // fewest parts a large blob is split into (more for very large ones)
	RangeMin   int64  // blobs at least this large are split
	Chown      *Owner // when set, blobs are handed to this uid/gid

	// Budget is the per-registry-host connection budget shared by every pull
	// on the node. Nil gives each FetchAll a private, fixed budget of
	// Workers x RangeParts connections (the behaviour before the budget existed).
	Budget *connbudget.Hosts

	// Tune, when set, may adjust the budget session a FetchAll opens (tests
	// inject a clock and shrink the windows through it).
	Tune func(*connbudget.SessionConfig)

	// Stream, when set, is invoked concurrently with each blob's download and
	// receives that blob's bytes in order as they arrive. It exists so the CPU
	// can work on a layer while the network is still the bottleneck: during a
	// pull the cores are otherwise idle, and inflating a layer is the single
	// most expensive part of creating a container from it.
	//
	// The bytes are not yet verified when Stream sees them, so a consumer must
	// treat its output as provisional; the downloader reports whether the blob
	// went on to pass its digest check through the returned commit callback.
	// A Stream error never fails the download -- the work it does is an
	// optimization -- and a retried transfer restarts it from the beginning.
	Stream func(ctx context.Context, desc Descriptor, r io.Reader) (commit func(verified bool), err error)

	// StreamWant filters which blobs Stream is offered. It is consulted before
	// a download starts so the prefix bookkeeping is only set up for blobs that
	// will actually use it -- most layers in an image are small enough not to
	// be worth streaming. Nil means every blob is offered.
	StreamWant func(Descriptor) bool

	// Fetched, when set, is called once per descriptor as soon as its blob is
	// in the layer store and verified (immediately for blobs already there),
	// so work that needs whole blobs can start before the last one arrives.
	// It must be safe for concurrent use.
	Fetched func(Descriptor)
}

// streaming reports whether desc should be handed to Stream as it downloads.
func (d *Downloader) streaming(desc Descriptor) bool {
	return d.Stream != nil && (d.StreamWant == nil || d.StreamWant(desc))
}

// Owner is the uid/gid that must own everything written, so that udocker --
// which the driver runs as a dedicated non-root user -- can read and replace
// what the agent (usually root) produced.
type Owner struct {
	UID int
	GID int
}

// BlobPath is where digest's content lives in the layer store.
func (d *Downloader) BlobPath(digest string) string {
	return filepath.Join(d.Dir, digest)
}

// Present reports whether the blob is already on disk at its expected size.
// Content is trusted on size match: the file only ever appears through an
// atomic rename after full sha256 verification.
func (d *Downloader) Present(desc Descriptor) bool {
	info, err := os.Stat(d.BlobPath(desc.Digest))
	return err == nil && info.Mode().IsRegular() && info.Size() == desc.Size
}

// FetchAll downloads every missing descriptor concurrently. progress is called
// with the number of bytes just written for a digest; it must be safe for
// concurrent use.
func (d *Downloader) FetchAll(ctx context.Context, ref Ref, descs []Descriptor, progress func(digest string, n int64), restart func(digest string)) error {
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	sess := d.openSession(ctx, ref)
	defer sess.Close()
	workers := d.Workers
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, desc := range descs {
		if d.Present(desc) {
			if progress != nil {
				progress(desc.Digest, desc.Size)
			}
			if d.Fetched != nil {
				d.Fetched(desc)
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(desc Descriptor) {
			defer wg.Done()
			defer func() { <-sem }()
			err := d.fetch(ctx, sess, ref, desc, progress, restart)
			if err == nil && d.Fetched != nil {
				d.Fetched(desc)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("layer %s: %w", shortDigest(desc.Digest), err)
				}
				mu.Unlock()
			}
		}(desc)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if d.Chown != nil {
		for _, desc := range descs {
			_ = os.Lchown(d.BlobPath(desc.Digest), d.Chown.UID, d.Chown.GID)
		}
	}
	return ctx.Err()
}

func (d *Downloader) fetch(ctx context.Context, sess *connbudget.Session, ref Ref, desc Descriptor, progress func(string, int64), restart func(string)) error {
	var lastErr error
	// whole is set once this blob's registry answered a range request with the
	// whole blob: this blob is then read over one plain connection. It is the
	// blob that is learned about, not the pull: a registry that serves some
	// content through a Range-less path need not serve all of it that way.
	whole := false
	for attempt := 1; attempt <= blobAttempts; attempt++ {
		if attempt > 1 {
			if restart != nil {
				restart(desc.Digest)
			}
			// A registry that does not do ranges needs no pause, only another
			// way of asking; anything else gets a growing one.
			if !errors.Is(lastErr, errRangeIgnored) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt-1) * time.Second):
				}
			}
		}
		lastErr = d.fetchOnce(ctx, sess, ref, desc, whole, progress)
		if lastErr == nil || ctx.Err() != nil || !retryable(lastErr) {
			return lastErr
		}
		if errors.Is(lastErr, errRangeIgnored) && !whole {
			whole = true
			slog.Info("registry ignores Range: reading the layer whole", "layer", shortDigest(desc.Digest), "registry", ref.Registry)
		}
		if isThrottled(lastErr) {
			// The spans already waited out the registry's pauses; starting the
			// blob over would only ask again sooner than it said.
			return lastErr
		}
	}
	return lastErr
}

func (d *Downloader) fetchOnce(ctx context.Context, sess *connbudget.Session, ref Ref, desc Descriptor, whole bool, progress func(string, int64)) error {
	tmp, err := os.CreateTemp(d.Dir, ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	url := d.Client.BlobURL(ref, desc.Digest)
	report := func(n int64) {
		if progress != nil {
			progress(desc.Digest, n)
		}
	}
	// A registry found to ignore Range is read whole, over one connection.
	ranged := d.RangeMin > 0 && desc.Size >= d.RangeMin && d.RangeParts > 1 && !whole
	parts, chunk := 1, desc.Size
	if ranged {
		parts = d.partsFor(desc.Size)
		chunk = (desc.Size + int64(parts) - 1) / int64(parts)
	}

	// When a consumer wants the bytes as they arrive, track the contiguous
	// prefix so it can read sequentially even though range parts land out of
	// order, and hand it a reader that follows that prefix.
	var growth *growingFile
	var stream *streamHandle
	if d.streaming(desc) {
		growth = newGrowingFile(desc.Size, chunk, parts)
		stream = d.startStream(ctx, desc, tmpName, growth)
		defer stream.stop()
	}
	advance := func(part int, n int64) {
		if growth != nil {
			growth.advance(part, n)
		}
	}
	if ranged {
		err = d.fetchRanged(ctx, sess, ref, url, desc, tmp, parts, chunk, report, advance)
	} else {
		err = d.fetchWhole(ctx, sess, ref, url, desc.Size, tmp, report, advance)
	}
	if growth != nil {
		growth.finish(err)
	}
	if err != nil {
		return err
	}
	if info, statErr := tmp.Stat(); statErr != nil || info.Size() != desc.Size {
		return fmt.Errorf("%w: size mismatch: got %d want %d", errBlobMismatch, fileSize(tmp), desc.Size)
	}
	// Verifying after the write (rather than hashing the stream) keeps the
	// ranged path -- whose parts arrive out of order -- on the same code path
	// as the whole-blob one, and reads back from page cache.
	if err := verifyDigest(tmpName, desc.Digest); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, d.BlobPath(desc.Digest)); err != nil {
		return err
	}
	if stream != nil {
		// The blob is verified and in place, so whatever the consumer derived
		// from it may now be published.
		stream.finish()
	}
	return nil
}

func (d *Downloader) fetchWhole(ctx context.Context, sess *connbudget.Session, ref Ref, url string, size int64, dst *os.File, report func(int64), advance func(int, int64)) error {
	return d.fetchSpan(ctx, sess, ref, url, dst, 0, size-1, false, report, func(n int64) { advance(0, n) })
}

// advanceWriter reports each completed write so a growingFile's contiguous
// prefix only ever covers bytes already durable in the file.
type advanceWriter struct {
	w       io.Writer
	part    int
	advance func(int, int64)
}

func (w *advanceWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.advance != nil {
		w.advance(w.part, int64(n))
	}
	return n, err
}

func verifyDigest(path, digest string) error {
	algo, want, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" {
		return fmt.Errorf("unsupported digest %q", digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var hasher hash.Hash = sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
		return fmt.Errorf("%w: digest mismatch: got sha256:%s want %s", errBlobMismatch, got, digest)
	}
	return nil
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errIdle) || errors.Is(err, errSlow) || errors.Is(err, errRangeIgnored) || errors.Is(err, errBlobMismatch) || isThrottled(err) || isReset(err) {
		return true
	}
	var status *statusError
	if errors.As(err, &status) {
		switch status.status {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// A timed-out dial, TLS handshake or read, and a DNS lookup that failed
	// (including "no such host" from a resolver that is still coming up) are
	// all worth another attempt.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func fileSize(f *os.File) int64 {
	if info, err := f.Stat(); err == nil {
		return info.Size()
	}
	return -1
}

func shortDigest(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
