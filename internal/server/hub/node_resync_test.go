package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// resyncSession is a connected session whose agent end reads the first byte
// of each write as it starts, telling began, and the rest only once read is
// closed (never, when stalled): a pipe write blocks until the other end reads
// it all, as a write to a peer that stopped consuming its stream does.
func resyncSession(t *testing.T, h *Hub, read <-chan struct{}) (*Session, <-chan proto.Envelope, <-chan struct{}) {
	t.Helper()
	uuid, _ := newTestNode(t, h.store)
	left, right := net.Pipe()
	s := newSession(h, nil, left)
	s.NodeUUID = uuid
	close(s.initialPushDone)
	h.mu.Lock()
	h.sessions[uuid] = s
	h.mu.Unlock()
	frames := make(chan proto.Envelope, 16)
	began := make(chan struct{}, 16)
	go func() {
		defer close(frames)
		first := make([]byte, 1)
		if _, err := io.ReadFull(right, first); err != nil {
			return
		}
		began <- struct{}{}
		select {
		case <-read:
		case <-s.closed:
			return
		}
		decoder := json.NewDecoder(io.MultiReader(bytes.NewReader(first), right))
		for {
			var frame proto.Envelope
			if decoder.Decode(&frame) != nil {
				return
			}
			frames <- frame
		}
	}()
	t.Cleanup(func() { s.Close(); _ = right.Close() })
	return s, frames, began
}

// waitBegan waits for a write to the session to start.
func waitBegan(t *testing.T, began <-chan struct{}) {
	t.Helper()
	select {
	case <-began:
	case <-time.After(5 * time.Second):
		t.Fatal("no replay started")
	}
}

// fullSyncs counts the full replays that arrive within d.
func fullSyncs(frames <-chan proto.Envelope, d time.Duration) int {
	n := 0
	deadline := time.After(d)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return n
			}
			if frame.T == proto.TStateSync {
				n++
			}
		case <-deadline:
			return n
		}
	}
}

// Requests come back at once, and those made while a replay is under way
// come to one more replay of the latest state, not one each: a fleet-wide
// save asks every node, and several saves in a row must not queue a replay
// per save.
func TestRequestedSyncsCoalesceIntoOneReplayEach(t *testing.T) {
	h := newTestHub(t)
	read := make(chan struct{})
	s, frames, began := resyncSession(t, h, read)

	start := time.Now()
	h.RequestSync(s.NodeUUID)
	waitBegan(t, began) // its replay is on the write, which nobody reads yet
	for i := 0; i < 20; i++ {
		h.RequestSync(s.NodeUUID)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("requests took %v: they waited for the agent", took)
	}
	close(read)
	if got := fullSyncs(frames, 2*time.Second); got != 2 {
		t.Fatalf("%d full replays for 21 requests, want 2: the one under way and one for those during it", got)
	}
}

// A request for a node that is not connected, or whose session has closed,
// is dropped: its next session starts with a full replay anyway.
func TestARequestedSyncForAGoneSessionIsDropped(t *testing.T) {
	h := newTestHub(t)
	h.RequestSync("no-such-node")
	read := make(chan struct{})
	close(read)
	s, frames, _ := resyncSession(t, h, read)
	s.Close()
	h.RequestSync(s.NodeUUID)
	if got := fullSyncs(frames, 300*time.Millisecond); got != 0 {
		t.Fatalf("%d replays to a closed session", got)
	}
}

// Every node asked receives the change: a setting saved after the sessions
// connected reaches each of them in the replay its request schedules.
func TestRequestedSyncsDeliverTheLatestStateToEveryNode(t *testing.T) {
	h := newTestHub(t)
	read := make(chan struct{})
	close(read)
	type node struct {
		s      *Session
		frames <-chan proto.Envelope
	}
	var nodes []node
	for i := 0; i < 3*initialPushConcurrency; i++ {
		s, frames, _ := resyncSession(t, h, read)
		nodes = append(nodes, node{s, frames})
	}
	if err := h.store.SetSetting(store.SettingOrphanCleanup, "true"); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		h.RequestSync(n.s.NodeUUID)
	}
	// One deadline for them all: the test is about every node getting the
	// change, not how fast 48 replays build on a loaded runner.
	deadline := time.After(time.Minute)
	for i, n := range nodes {
		select {
		case frame := <-n.frames:
			var ds proto.DesiredState
			if frame.T != proto.TStateSync || json.Unmarshal(frame.D, &ds) != nil || !ds.OrphanCleanup {
				t.Fatalf("node %d got %s %s, want a full replay with the saved setting", i, frame.T, frame.D)
			}
		case <-deadline:
			t.Fatalf("node %d of %d received no replay within a minute", i, len(nodes))
		}
	}
}
