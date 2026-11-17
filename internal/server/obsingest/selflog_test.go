package obsingest

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/logstore"
)

// TestDrainSelfLogsWritesServerSource pins the server-side self-log contract:
// error-level slog records reach the logstore as source=server rows with
// their severity stamped, info records stay out, and an empty queue is a
// no-op. The server's own logs bypass the TLogs hop entirely — the writer is
// local.
func TestDrainSelfLogsWritesServerSource(t *testing.T) {
	st, err := logstore.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatalf("open logstore: %v", err)
	}
	defer st.Close()
	o := &Pipeline{logs: st}

	collector := selflog.NewCollector(slog.NewTextHandler(testDiscard{}, nil), slog.LevelError, 16)
	logger := slog.New(collector)
	logger.Info("controller ticked") // below the floor
	logger.Error("deployment reconcile failed", "deployment", 42)
	logger.Error("store compaction overdue")

	o.drainSelfLogs(collector)

	res, err := st.Search(logstore.Filter{Source: logstore.SourceServer, Limit: 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("source=server search returned %d rows, want 2: %+v", len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		if e.Source != logstore.SourceServer {
			t.Fatalf("row source = %q, want server: %+v", e.Source, e)
		}
		if e.Level != "error" {
			t.Fatalf("row level = %q, want error: %+v", e.Level, e)
		}
		if e.Instance != "" || e.Node != "" {
			t.Fatalf("server rows must not carry instance/node identity: %+v", e)
		}
	}
	if !strings.Contains(res.Entries[0].Line+res.Entries[1].Line, "deployment=42") {
		t.Fatalf("attr text missing from captured lines: %+v", res.Entries)
	}

	// A second drain with nothing queued must not duplicate rows.
	o.drainSelfLogs(collector)
	res, err = st.Search(logstore.Filter{Source: logstore.SourceServer, Limit: 100})
	if err != nil {
		t.Fatalf("search after second drain: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("second drain changed the corpus: %d rows", len(res.Entries))
	}
}

type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }
