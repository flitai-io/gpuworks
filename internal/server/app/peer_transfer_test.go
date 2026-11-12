package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Exercise real Hub producers with the same nonblocking ingestion callbacks as
// app.New. Full/stalled persistence queues must never hold a transfer open.
func TestPeerTransferProducersDoNotBlockOnFullProductionIngestQueues(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o := &observability{now: time.Now, metricsQueue: make(chan []tsdb.Sample, 1), logQueue: make(chan []logstore.Entry, 1)}
	o.metricsQueue <- nil
	o.logQueue <- nil
	h := hub.New(st, slog.Default())
	h.SetSinks(o.ingestMetrics, o.ingestLogs)
	done := make(chan struct{})
	go func() {
		_ = h.ProbeModelPath(context.Background(), proto.ModelSyncCopyRequest{TransferID: "probe-full", Key: "probe", SourceNodeUUID: "offline-source", TargetNodeUUID: "offline-target", SourcePath: "source", TargetPath: "target"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.RunPeerTransferMetrics(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full ingest queue blocked producer")
	}
	if o.logDropped.Load() != 2 || o.metricsDropped.Load() != 15 {
		t.Fatalf("drops: logs=%d metrics=%d", o.logDropped.Load(), o.metricsDropped.Load())
	}
	if view := h.PeerTransferSnapshot(); view.Totals.ProbeFailed != 1 || view.Totals.Failed != 0 || len(view.Active) != 0 {
		t.Fatalf("probe/resource accounting: %+v", view)
	}
}
