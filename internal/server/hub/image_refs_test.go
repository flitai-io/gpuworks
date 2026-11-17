package hub

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// nextRefs is the next image refs update a session sends within d.
func nextRefs(t *testing.T, frames <-chan proto.Envelope, d time.Duration) ([]string, bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return nil, false
			}
			var su proto.StateUpdate
			if frame.T == proto.TStateUpdate && json.Unmarshal(frame.D, &su) == nil && su.ImageRefs != nil {
				return su.ImageRefs, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

// A node that stops reading holds up only its own refs: the others get each
// list at once, and the stalled node gets the newest list - once - when it
// reads again, not a write for each list it missed. Before, the controller
// wrote the refs to one node after another, so a stalled node held every
// node after it.
func TestAStalledNodeHoldsUpOnlyItsOwnImageRefs(t *testing.T) {
	h := newTestHub(t)
	stalledRead := make(chan struct{})
	stalled, stalledFrames, began := resyncSession(t, h, stalledRead)
	open := make(chan struct{})
	close(open)
	var others []<-chan proto.Envelope
	var uuids []string
	for i := 0; i < 3; i++ {
		s, frames, _ := resyncSession(t, h, open)
		others = append(others, frames)
		uuids = append(uuids, s.NodeUUID)
	}
	lists := [][]string{{"a:v1"}, {"a:v1", "b:v1"}, {"a:v1", "b:v1", "c:v1"}}

	h.RequestImageRefs(stalled.NodeUUID, lists[0])
	waitBegan(t, began) // the stalled node's write is under way
	for i, list := range lists {
		start := time.Now()
		if i > 0 {
			h.RequestImageRefs(stalled.NodeUUID, list)
		}
		for _, uuid := range uuids {
			h.RequestImageRefs(uuid, list)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("handing out the refs took %v: it waited for an agent", took)
		}
		for j, frames := range others {
			got, ok := nextRefs(t, frames, 3*time.Second)
			if !ok || len(got) != len(list) {
				t.Fatalf("list %d: node %d got %v (ok=%v) while another node stalled, want %v", i, j, got, ok, list)
			}
		}
	}
	close(stalledRead)
	first, ok := nextRefs(t, stalledFrames, 3*time.Second)
	if !ok || len(first) != 1 {
		t.Fatalf("the stalled node's held write = %v (ok=%v), want the first list", first, ok)
	}
	latest, ok := nextRefs(t, stalledFrames, 3*time.Second)
	if !ok || len(latest) != 3 {
		t.Fatalf("after its write ended the stalled node got %v (ok=%v), want the newest list", latest, ok)
	}
	if extra, ok := nextRefs(t, stalledFrames, 500*time.Millisecond); ok {
		t.Fatalf("the stalled node got another write %v: one per missed list instead of the newest once", extra)
	}
}

// Refs handed to a node that is not connected, or whose session closed, are
// dropped: its next session's full replay carries them.
func TestImageRefsForAGoneSessionAreDropped(t *testing.T) {
	h := newTestHub(t)
	h.RequestImageRefs("no-such-node", []string{"a:v1"})
	open := make(chan struct{})
	close(open)
	s, frames, _ := resyncSession(t, h, open)
	s.Close()
	h.RequestImageRefs(s.NodeUUID, []string{"a:v1"})
	if got, ok := nextRefs(t, frames, 300*time.Millisecond); ok {
		t.Fatalf("a closed session was sent refs %v", got)
	}
}

// Refs handed to a session whose first replay has not landed wait for it and
// are sent after it, even when the replay's buffer of incremental updates is
// full. Before, the refs went into that buffer, and a full one dropped them
// until the catalog changed again.
func TestImageRefsWaitForTheFirstReplay(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	left, right := net.Pipe()
	s := newSession(h, nil, left)
	s.NodeUUID = uuid
	h.mu.Lock()
	h.sessions[uuid] = s
	h.mu.Unlock()
	t.Cleanup(func() { s.Close(); _ = right.Close() })
	frames := make(chan proto.Envelope, 16)
	go func() {
		defer close(frames)
		decoder := json.NewDecoder(right)
		for {
			var frame proto.Envelope
			if decoder.Decode(&frame) != nil {
				return
			}
			frames <- frame
		}
	}()
	s.stateMu.Lock()
	for len(s.pendingUpdates) < pendingUpdatesMax {
		s.pendingUpdates = append(s.pendingUpdates, proto.StateUpdate{})
	}
	s.stateMu.Unlock()

	h.RequestImageRefs(uuid, []string{"a:v1"})
	if got, ok := nextRefs(t, frames, 300*time.Millisecond); ok {
		t.Fatalf("refs %v sent before the first replay landed", got)
	}
	s.finishInitialPush(pendingUpdatesMax) // the replay read after every buffered update
	if got, ok := nextRefs(t, frames, 3*time.Second); !ok || len(got) != 1 || got[0] != "a:v1" {
		t.Fatalf("after the first replay the refs were %v (ok=%v), want them sent", got, ok)
	}
}
