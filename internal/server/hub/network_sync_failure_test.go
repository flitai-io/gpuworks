package hub

import (
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func networkWireSession(t *testing.T, h *Hub, modern bool) (*Session, <-chan proto.Envelope) {
	t.Helper()
	uuid, _ := newTestNode(t, h.store)
	left, right := net.Pipe()
	s := newSession(h, nil, left)
	s.NodeUUID = uuid
	if modern {
		s.caps = []string{proto.CapNetworkUpdate}
	}
	close(s.initialPushDone)
	h.sessions[uuid] = s
	frames := make(chan proto.Envelope, 8)
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
	t.Cleanup(func() { s.Close(); _ = right.Close() })
	return s, frames
}

// failProxyReads makes every read of the proxy entries fail while enabled.
// The entries are cached between writes, so a test enables it after a write
// and before the read it means to fail.
func failProxyReads(t *testing.T, h *Hub) (*atomic.Bool, <-chan struct{}) {
	t.Helper()
	enabled := &atomic.Bool{}
	enabled.Store(true)
	failed := make(chan struct{}, 8)
	err := h.store.DB().Callback().Query().Before("gorm:query").Register("proxy_read_failure", func(tx *gorm.DB) {
		if !enabled.Load() || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "pull_proxies" {
			return
		}
		tx.AddError(errors.New("temporary proxy read failure"))
		select {
		case failed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return enabled, failed
}

// A proxy that cannot be read is never sent as "no proxy": the node keeps
// what it has, nothing blocks other nodes' updates, and the update retries on
// its own and sends the latest entries once they can be read.
func TestNetworkSyncReadFailureKeepsPolicyUntilRecovery(t *testing.T) {
	for _, modern := range []bool{true, false} {
		t.Run(map[bool]string{true: "incremental", false: "legacy"}[modern], func(t *testing.T) {
			h := newTestHub(t)
			target, frames := networkWireSession(t, h, modern)
			other, otherFrames := networkWireSession(t, h, true)
			setProxies(t, h.store, forNode(target.NodeUUID, "http://node.invalid", "http://secure.invalid"), everyNode("http://global.invalid", "http://secure.invalid"))
			failing, failed := failProxyReads(t, h)
			h.RequestNetworkSync(target.NodeUUID)
			select {
			case <-failed:
			case <-time.After(2 * time.Second):
				t.Fatal("the proxy entries were not read")
			}
			select {
			case frame := <-frames:
				t.Fatalf("failed read published empty or partial policy: %s", frame.D)
			case <-time.After(100 * time.Millisecond):
			}
			// A policy read failure on one worker cannot block independent
			// control updates.
			if err := h.PushToNode(other.NodeUUID, proto.StateUpdate{ImageRefs: []string{"example/healthy:v1"}}); err != nil {
				t.Fatal(err)
			}
			frame := receiveNetworkEnvelope(t, otherFrames)
			var healthy proto.StateUpdate
			if err := json.Unmarshal(frame.D, &healthy); err != nil || len(healthy.ImageRefs) != 1 {
				t.Fatalf("healthy update=%s err=%v", frame.D, err)
			}
			failing.Store(false)
			setProxies(t, h.store, forNode(target.NodeUUID, "http://latest-node.invalid", "http://latest-secure.invalid"), everyNode("http://latest-global.invalid", "http://latest-secure.invalid"))
			// No second notification: the failed update must retry on its own.
			frame = receiveNetworkEnvelope(t, frames)
			var got struct {
				Network proto.NetworkConfig `json:"network"`
			}
			if err := json.Unmarshal(frame.D, &got); err != nil {
				t.Fatal(err)
			}
			if got.Network.HTTPProxy != "http://latest-node.invalid" || got.Network.HTTPSProxy != "http://latest-secure.invalid" {
				t.Fatalf("recovery did not send latest complete policy: %s", frame.D)
			}
			if want := map[bool]string{true: proto.TStateUpdate, false: proto.TStateSync}[modern]; frame.T != want {
				t.Fatalf("kind=%s want=%s", frame.T, want)
			}
		})
	}
}

func TestNetworkSyncSessionCloseStopsReadFailureRetry(t *testing.T) {
	h := newTestHub(t)
	target, frames := networkWireSession(t, h, true)
	_, failed := failProxyReads(t, h)
	h.RequestNetworkSync(target.NodeUUID)
	for range 2 {
		select {
		case <-failed:
		case frame := <-frames:
			t.Fatalf("failed read sent policy: %s", frame.D)
		case <-time.After(2 * time.Second):
			t.Fatal("failed read was not retried")
		}
	}
	target.Close()
	select {
	case <-failed:
		t.Fatal("closed session retried policy lookup")
	case <-time.After(1200 * time.Millisecond):
	}
}

func TestNetworkSyncWaitsForSnapshotCapacityAndCancelsWhenClosed(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(map[bool]string{true: "close", false: "resume"}[closeSession], func(t *testing.T) {
			h := newTestHub(t)
			h.pushLimiter = make(chan struct{}, 1)
			h.pushLimiter <- struct{}{}
			target, frames := networkWireSession(t, h, true)
			h.RequestNetworkSync(target.NodeUUID)
			select {
			case frame := <-frames:
				t.Fatalf("policy bypassed occupied snapshot capacity: %s", frame.D)
			case <-time.After(100 * time.Millisecond):
			}
			if closeSession {
				target.Close()
			}
			<-h.pushLimiter
			if closeSession {
				select {
				case frame, ok := <-frames:
					if ok {
						t.Fatalf("closed session sent policy: %s", frame.D)
					}
				case <-time.After(time.Second):
					t.Fatal("closed wire reader did not stop")
				}
			} else {
				if frame := receiveNetworkEnvelope(t, frames); frame.T != proto.TStateUpdate {
					t.Fatalf("resume sent %s", frame.T)
				}
			}
		})
	}
}
