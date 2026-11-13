package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// panicOnlineHub makes exactly one node's reconcile pass blow up, the way a
// genuine defect in a launch path does. Every other node is answered honestly
// (offline), and the push paths refuse to pretend they succeeded.
type panicOnlineHub struct {
	mu          sync.Mutex
	panicNode   string
	onlineCalls map[string]int
}

func (h *panicOnlineHub) Online(uuid string) bool {
	h.mu.Lock()
	h.onlineCalls[uuid]++
	h.mu.Unlock()
	if uuid == h.panicNode {
		panic("simulated defect while reconciling node " + uuid)
	}
	return false
}

func (h *panicOnlineHub) calls(uuid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.onlineCalls[uuid]
}

// RequestImageRefs records the refs as the push the hub would send.
func (h *panicOnlineHub) RequestImageRefs(uuid string, refs []string) {
	_ = h.PushToNode(uuid, proto.StateUpdate{ImageRefs: refs})
}

func (h *panicOnlineHub) PushToNode(uuid string, _ proto.StateUpdate) error {
	return fmt.Errorf("unexpected push to node %s", uuid)
}

func (h *panicOnlineHub) SyncNode(uuid string) error {
	return fmt.Errorf("unexpected sync of node %s", uuid)
}

func (h *panicOnlineHub) CopyModelPath(context.Context, proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error) {
	return proto.ModelSyncCopyResult{}, errors.New("unexpected model copy")
}

func (h *panicOnlineHub) StopModelSync(context.Context, string, string, string) error {
	return errors.New("unexpected model sync stop")
}

// lockedBuffer lets the test read the worker goroutine's log output safely.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWorkerSurvivesReconcilePanicAndKeepsDrainingTheQueue proves one broken
// deployment cannot take the server down with it. Before the guard, a panic in
// reconcile unwound through the single worker goroutine and killed the process;
// the supervisor restarted it, the same deployment was primed back onto the
// queue, and the server crash-looped until someone deleted the deployment.
func TestWorkerSurvivesReconcilePanicAndKeepsDrainingTheQueue(t *testing.T) {
	st, err := store.Open("file:controller_worker_panic_guard?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/panic-guard", Revision: "main", Name: "panic-guard", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	deployments := map[string]*store.Deployment{}
	nodes := map[string]*store.Node{}
	for _, name := range []string{"panic-node", "healthy-node"} {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: 0, UUID: name + "-gpu", Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia",
			ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 80 << 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/panic-guard"}); err != nil {
			t.Fatal(err)
		}
		dep, err := createTestDeployment(t, st, &store.Deployment{
			Name: name + "-deployment", ModelID: model.ID, NodeID: node.ID, ServedName: name,
			Engine: "vllm", State: "active", QuickParams: `{"max_model_len":512}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes[name], deployments[name] = node, dep
	}

	logs := &lockedBuffer{}
	hub := &panicOnlineHub{panicNode: nodes["panic-node"].UUID, onlineCalls: map[string]int{}}
	c := New(st, hub, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c.q.close()
	})
	go c.worker(ctx)

	c.q.add(depKey(deployments["panic-node"].ID))
	waitFor(t, "panic to be logged", func() bool { return strings.Contains(logs.String(), "reconcile deployment panicked") })

	// The worker goroutine is still alive: it picks up the next key enqueued
	// after the panic and reconciles a different deployment normally.
	c.q.add(depKey(deployments["healthy-node"].ID))
	waitFor(t, "the next queued deployment to be reconciled", func() bool {
		return hub.calls(nodes["healthy-node"].UUID) > 0
	})

	out := logs.String()
	if !strings.Contains(out, "simulated defect while reconciling node "+nodes["panic-node"].UUID) {
		t.Fatalf("log does not carry the panic value:\n%s", out)
	}
	if !strings.Contains(out, "goroutine") || !strings.Contains(out, "controller.(*Controller).reconcileDeployment") {
		t.Fatalf("log does not carry the panic stack:\n%s", out)
	}
	// The panic reaches the worker as an ordinary reconcile failure, on the same
	// failure path an error already took - no new deployment state is invented.
	if !strings.Contains(out, "reconcile deployment failed") {
		t.Fatalf("panic was not reported on the existing reconcile failure path:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("deployment %d reconcile panicked", deployments["panic-node"].ID)) {
		t.Fatalf("failure line does not name the deployment that panicked:\n%s", out)
	}
	// The panicking deployment is not retried in a hot loop by the guard itself.
	if got := hub.calls(nodes["panic-node"].UUID); got != 1 {
		t.Fatalf("panicking deployment was reconciled %d times, want 1", got)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
