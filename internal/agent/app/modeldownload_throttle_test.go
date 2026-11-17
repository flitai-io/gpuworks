package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The source answers the first request for a file with 429 and asks for a
// second: the download waits, says so while it does, completes, and drops
// the note once the file's bytes arrive (GWKS-272).
func TestDownloadRepoWaitsOutARateLimitedSource(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := bytes.Repeat([]byte("modelscope-vocabulary\n"), 2048)

	var throttled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			if throttled.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			http.ServeContent(w, r, "vocab.json", time.Time{}, bytes.NewReader(vocab))
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	n.downloadRepo(context.Background(), task, target, target)

	got := lastModelFileStatus(t, statuses)
	if got.State != proto.MFPresent {
		t.Fatalf("final status = %+v, want present after one rate-limited answer", got)
	}
	downloaded, err := os.ReadFile(filepath.Join(target, "vocab.json"))
	if err != nil || !bytes.Equal(downloaded, vocab) {
		t.Fatalf("vocab.json: %d bytes, err=%v; want %d", len(downloaded), err, len(vocab))
	}
	waiting, after := -1, -1
	for i, st := range *statuses {
		if strings.Contains(st.Message, "rate limited by the source; retrying in 1s") {
			if waiting < 0 {
				waiting = i
			}
		} else if waiting >= 0 && st.State == proto.MFDownloading && st.BytesDone > 0 && after < 0 {
			after = i
		}
	}
	if waiting < 0 {
		t.Fatalf("no status said the source asked to wait: %+v", statusTrail(*statuses))
	}
	if after < 0 || strings.Contains((*statuses)[after].Message, "rate limited") {
		t.Fatalf("the rate-limit note outlived the wait: %+v", statusTrail(*statuses))
	}
	// The wait is announced with the bytes already on disk (config.json and
	// the weights), and the download never appears to go backwards.
	if done := int64(len(config) + len(weights)); (*statuses)[waiting].BytesDone != done {
		t.Fatalf("the wait was announced at %d bytes, want the %d already done: %+v", (*statuses)[waiting].BytesDone, done, statusTrail(*statuses))
	}
	var high int64
	for _, st := range *statuses {
		if st.State != proto.MFDownloading {
			continue
		}
		if st.BytesDone < high {
			t.Fatalf("progress went back from %d to %d: %+v", high, st.BytesDone, statusTrail(*statuses))
		}
		high = st.BytesDone
	}
}

// The listing answers 429 first: the download waits and lists again
// instead of ending, with the only address there is.
func TestDownloadRepoWaitsOutARateLimitedListing(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := []byte("fixture-vocabulary")
	var listings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			if listings.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			http.ServeContent(w, r, "vocab.json", time.Time{}, bytes.NewReader(vocab))
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	n.downloadRepo(context.Background(), task, target, target)
	if got := lastModelFileStatus(t, statuses); got.State != proto.MFPresent {
		t.Fatalf("final status = %+v, want present after one rate-limited listing", got)
	}
	if listings.Load() != 2 {
		t.Fatalf("listed %d times, want the throttled one and its retry", listings.Load())
	}
}

func statusTrail(statuses []proto.ModelFileStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, fmt.Sprintf("%s %d %q", st.State, st.BytesDone, st.Message))
	}
	return out
}
