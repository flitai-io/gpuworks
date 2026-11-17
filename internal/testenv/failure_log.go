package testenv

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// failureLogTail is how much of a case's server and agent log is kept for a
// failure: the end of the run is what explains it, and a bound keeps a long
// or chatty case from growing memory or the CI artifact.
const failureLogTail = 1 << 20

// failureLogs holds one tail buffer per running case, shared by the case's
// server and agents so they land in one file in the order they were written.
var failureLogs sync.Map // testing.TB -> *tailBuffer

// failureLogger is the harness logger when GPUWORKS_TESTENV_FAILURE_LOG_DIR
// is set: debug logs go to a bounded in-memory tail, written to
// <dir>/<case>.log only if the case fails and dropped if it passes. It is how
// a CI flake that cannot be reproduced locally leaves its evidence behind.
func failureLogger(t testing.TB, dir string) *slog.Logger {
	fresh := &tailBuffer{limit: failureLogTail}
	actual, loaded := failureLogs.LoadOrStore(t, fresh)
	buf := actual.(*tailBuffer)
	if !loaded {
		t.Cleanup(func() {
			failureLogs.Delete(t)
			if !t.Failed() {
				return
			}
			if err := buf.writeFile(filepath.Join(dir, failureLogName(t.Name()))); err != nil {
				t.Logf("keep harness log for failure: %v", err)
			}
		})
	}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

var unsafeLogName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// failureLogName turns a test name (subtests carry "/") into one file name.
func failureLogName(name string) string {
	name = strings.Trim(unsafeLogName.ReplaceAllString(name, "_"), "_")
	if len(name) > 180 {
		name = name[:180]
	}
	return name + ".log"
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu      sync.Mutex
	limit   int
	data    []byte
	dropped int64
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.dropped += int64(over)
		b.data = append(b.data[:0], b.data[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) writeFile(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := b.data
	if b.dropped > 0 {
		body = append([]byte(fmt.Sprintf("... %d earlier bytes dropped, last %d kept ...\n", b.dropped, len(b.data))), body...)
	}
	return os.WriteFile(path, body, 0o644)
}
