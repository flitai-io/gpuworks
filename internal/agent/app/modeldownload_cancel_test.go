package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestWithdrawDownloadStopsNetworkRequests(t *testing.T) {
	endpoint, started, abandoned := newHeldWeightHub(t)
	n, _ := newEvictionRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); waitCanceledDownloadWorkers(t, n) })
	task := proto.ModelFileTask{Key: "f17", Source: "huggingface", SourceID: "org/model", Revision: "main", Endpoint: endpoint, TargetPath: "org/model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload, TransferID: "download-1"}
	n.reconcileModelFile(ctx, task)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	task.DesiredState = "paused"
	n.OnStateUpdate(ctx, proto.StateUpdate{ModelFiles: []proto.ModelFileTask{task}})
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("withdrawn download kept making network requests")
	}
}

func TestPausedDownloadFencesStaleReplayAndKeepsNewRetry(t *testing.T) {
	n, _ := newEvictionRuntime(t)
	// A cancellation fences the exact old attempt even after its worker exits.
	old := proto.ModelFileTask{Key: "f17", DesiredState: "paused", TransferID: "old", Acquisition: proto.ModelAcquireDownload}
	n.reconcileModelFile(context.Background(), old)
	old.DesiredState = "present"
	if !n.copyTaskRunning(old, false) {
		t.Fatal("stale pending snapshot can revive a canceled transfer")
	}
	var canceled bool
	n.downloading[old.Key] = &modelDownload{transferID: "new", cancel: func() { canceled = true }}
	old.DesiredState = "paused"
	n.OnStateSync(context.Background(), proto.DesiredState{ModelFiles: []proto.ModelFileTask{old}})
	if canceled {
		t.Fatal("late old pause canceled newer retry")
	}
}

func TestFullReplayWithdrawsDownloadMissingAfterWriterReassignment(t *testing.T) {
	endpoint, started, abandoned := newHeldWeightHub(t)
	n, _ := newEvictionRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); waitCanceledDownloadWorkers(t, n) })
	task := proto.ModelFileTask{Key: "f17", Source: "huggingface", SourceID: "org/model", Revision: "main", Endpoint: endpoint, TargetPath: "org/model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload, TransferID: "old-writer"}
	n.reconcileModelFile(ctx, task)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	// This is the old writer's reconnect state after its shared row moved.
	n.OnStateSync(ctx, proto.DesiredState{})
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("unlisted old writer kept downloading after reconnect")
	}
	if !n.copyTaskRunning(task, false) {
		t.Fatal("stale old-writer task can restart after withdrawal")
	}
}

func TestDownloadStopRPCAcknowledgesWorkerExitAndFencesReplay(t *testing.T) {
	endpoint, started, abandoned := newHeldWeightHub(t)
	n, _ := newEvictionRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); waitCanceledDownloadWorkers(t, n) })
	task := proto.ModelFileTask{Key: "f17", Source: "huggingface", SourceID: "org/model", Revision: "main", Endpoint: endpoint, TargetPath: "org/model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload, TransferID: "download-1"}
	n.reconcileModelFile(ctx, task)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("download never started")
	}
	raw, _ := json.Marshal(proto.ModelCopyStopParams{Key: task.Key, TransferID: task.TransferID})
	if _, err := n.OnRPC(ctx, proto.RPCModelDownloadStop, raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-abandoned:
	case <-ctx.Done():
		t.Fatal("acknowledged download kept making network requests")
	}
	if n.runningDownloads() != 0 {
		t.Fatal("stop acknowledged before worker released its slot")
	}
	if !n.copyTaskRunning(task, false) {
		t.Fatal("old task replay can restart stopped download")
	}
}

func TestCanceledAttemptStaysFencedAfterMultipleNewRetryIDs(t *testing.T) {
	n, _ := newEvictionRuntime(t)
	old := proto.ModelFileTask{Key: "f17", DesiredState: "paused", TransferID: "old", Acquisition: proto.ModelAcquireDownload}
	n.reconcileModelFile(context.Background(), old)
	// First new task consumes the normal same-key/new-attempt path.
	newer := old
	newer.DesiredState = "present"
	newer.TransferID = "new"
	if n.copyTaskRunning(newer, false) {
		t.Fatal("fresh retry was fenced")
	}
	var canceled bool
	n.downloading[old.Key] = &modelDownload{transferID: "new", cancel: func() { canceled = true }}
	old.DesiredState = "present"
	if !n.copyTaskRunning(old, false) || canceled {
		t.Fatal("old snapshot superseded the fresh retry")
	}
	// Canceling a second attempt must not erase the first tombstone either.
	newer.DesiredState = "paused"
	n.reconcileModelFile(context.Background(), newer)
	n.downloading[old.Key] = &modelDownload{transferID: "third", cancel: func() { t.Error("old snapshot canceled third retry") }}
	if !n.copyTaskRunning(old, false) {
		t.Fatal("first canceled attempt revived after later cancellation")
	}
}

func TestLateCanceledTaskKeepsParkedRetry(t *testing.T) {
	n := newStorageTestRuntime(t)
	old := proto.ModelFileTask{Key: "f17", DesiredState: "paused", TransferID: "old", Acquisition: proto.ModelAcquireDownload}
	n.reconcileModelFile(context.Background(), old)
	newer := old
	newer.DesiredState = "present"
	newer.TransferID = "new"
	n.parkWriterModelFile(newer, false)
	for _, state := range []string{"paused", "present"} {
		old.DesiredState = state
		n.reconcileModelFile(context.Background(), old)
		got, ok := n.parkedModelFile(newer.Key)
		if !ok || got.TransferID != newer.TransferID {
			t.Fatalf("late %s erased new retry: %+v %v", state, got, ok)
		}
	}
}

func waitCanceledDownloadWorkers(t *testing.T, n *NodeRuntime) {
	t.Helper()
	n.dlMu.Lock()
	active := make([]*modelDownload, 0, len(n.downloading))
	for _, run := range n.downloading {
		active = append(active, run)
	}
	n.dlMu.Unlock()
	for _, run := range active {
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
			t.Error("canceled download did not release its worker")
		}
	}
}
