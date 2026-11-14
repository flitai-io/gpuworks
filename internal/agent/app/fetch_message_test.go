package app

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

type sentStatuses struct {
	mu   sync.Mutex
	list []proto.ModelFileStatus
}

func (s *sentStatuses) send(st proto.ModelFileStatus) {
	s.mu.Lock()
	s.list = append(s.list, st)
	s.mu.Unlock()
}

func (s *sentStatuses) last() proto.ModelFileStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) == 0 {
		return proto.ModelFileStatus{}
	}
	return s.list[len(s.list)-1]
}

func newTestFetchMessage() (*fetchMessage, *sentStatuses) {
	sent := &sentStatuses{}
	base := proto.ModelFileStatus{Key: "k", State: proto.MFDownloading, Path: "/models/m", Acquisition: proto.ModelAcquireDownload}
	return newFetchMessage(base, sent.send), sent
}

func throttle(retryAfter time.Duration) error {
	return &download.StatusError{Code: http.StatusTooManyRequests, Detail: "download: unexpected probe status 429", RetryAfter: retryAfter}
}

// While the source has asked the download to wait, the status says so, at
// once and with the progress already reached; bytes beyond that point put the
// address back.
func TestTheStatusSaysWhenTheSourceAskedTheDownloadToWait(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.progress(proto.ModelFileStatus{Key: "k", State: proto.MFDownloading, BytesDone: 40 << 30, BytesTotal: 60 << 30, RateBytesPerS: 1 << 20, Revision: "abc"})
	m.retrying(throttle(110*time.Second), 110*time.Second)

	want := "downloading from huggingface.co (rate limited by the source; retrying in 1m50s)"
	got := sent.last()
	if got.Message != want {
		t.Fatalf("message %q, want %q", got.Message, want)
	}
	if got.BytesDone != 40<<30 || got.BytesTotal != 60<<30 || got.Revision != "abc" || got.RateBytesPerS != 0 {
		t.Fatalf("the wait was announced as %+v: the progress reached must stay, with no rate", got)
	}

	// A resumed file reports the bytes already on disk first: not new bytes.
	m.progress(proto.ModelFileStatus{BytesDone: 40 << 30, BytesTotal: 60 << 30})
	if sent.last().Message != want {
		t.Fatalf("a report of the bytes already there ended the wait: %q", sent.last().Message)
	}
	m.progress(proto.ModelFileStatus{BytesDone: 40<<30 + 4096, BytesTotal: 60 << 30})
	if sent.last().Message != downloadingFrom("huggingface.co") {
		t.Fatalf("new bytes left %q", sent.last().Message)
	}
}

// Other retries (a reset, a server error) are not waits the source asked for
// and announce nothing.
func TestOtherRetriesDoNotSayRateLimited(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.retrying(errors.New("read: connection reset by peer"), time.Second)
	m.retrying(&download.StatusError{Code: http.StatusInternalServerError, Detail: "500"}, time.Second)
	if len(sent.list) != 1 || strings.Contains(sent.last().Message, "rate limited") {
		t.Fatalf("statuses %+v", sent.list)
	}
}

// A throttled listing (a modelrepo error, not a download one) is a wait too.
func TestAThrottledListingSaysRateLimited(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(&modelrepo.StatusError{Code: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}, 30*time.Second)
	if !strings.Contains(sent.last().Message, "rate limited by the source; retrying in 30s") {
		t.Fatalf("message %q", sent.last().Message)
	}
}

// Moving to another address ends the wait the last address asked for, and the
// switch is announced with the progress reached.
func TestASwitchEndsTheWaitOfTheAddressBefore(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(7<<20, 9<<20, "abc")
	m.retrying(throttle(5*time.Second), 5*time.Second)
	m.set(downloadingFrom("huggingface.co") + " (switched: hf-mirror.com failed 6 times)")
	got := sent.last()
	if strings.Contains(got.Message, "rate limited") || !strings.HasPrefix(got.Message, "downloading from huggingface.co") {
		t.Fatalf("message %q", got.Message)
	}
	if got.BytesDone != 7<<20 || got.BytesTotal != 9<<20 {
		t.Fatalf("the switch was announced as %+v", got)
	}
}
