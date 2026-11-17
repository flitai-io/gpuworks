package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
)

// upgradeHandshakePeer uses the actual handshake and control decoder. Legacy
// sessions deliberately have no data-lane session ID.
func upgradeHandshakePeer(t *testing.T, h *Hub, token, version, laneID string, smoke bool) (*Session, net.Conn, error) {
	t.Helper()
	server, peer := net.Pipe()
	s := newSession(h, nil, server)
	s.sessionID = laneID
	handshake := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		err := s.handshake()
		handshake <- err
		if err == nil {
			s.ctrlLoop()
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = server.Close()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("upgrade control peer did not stop")
		}
		if h.session(s.NodeUUID) == s {
			h.deregister(s)
		}
	})
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(peer).Encode(proto.Envelope{T: proto.THello, D: mustJSON(t, proto.Hello{ProtoMin: 1, ProtoMax: 1, NodeToken: token, Version: version, Smoke: smoke})}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(peer)
	for _, kind := range []string{proto.TRegistered, proto.THello} {
		var env proto.Envelope
		if err := dec.Decode(&env); err != nil || env.T != kind {
			t.Fatalf("handshake reply=%s err=%v", env.T, err)
		}
	}
	return s, peer, <-handshake
}

func TestAgentUpgradeObservationRequiresNewFormalControlTraffic(t *testing.T) {
	for _, laneID := range []string{"", "dual-lane-id"} {
		t.Run("lane="+laneID, func(t *testing.T) {
			h := newTestHub(t)
			n, token, err := h.store.CreateNodePending("upgrade-ready")
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan agentupgrade.Observation, 8)
			h.SetAgentUpgradeObserver(func(e agentupgrade.Observation) { events <- e })
			smoke, _, err := upgradeHandshakePeer(t, h, token, "target", laneID, true)
			if !errors.Is(err, errSmokeHandshake) || smoke.upgradeReady.Load() {
				t.Fatalf("smoke was an upgrade confirmation: %v", err)
			}
			if _, online := h.AgentUpgradeConnection(n.UUID); online {
				t.Fatal("smoke created a formal connection")
			}
			select {
			case e := <-events:
				t.Fatalf("smoke emitted %+v", e)
			default:
			}
			s, peer, err := upgradeHandshakePeer(t, h, token, "target", laneID, false)
			if err != nil {
				t.Fatal(err)
			}
			connection, ok := h.AgentUpgradeConnection(n.UUID)
			if !ok || connection.Ready || connection.SessionID == "" || connection.Version != "target" {
				t.Fatalf("before traffic=%+v connected=%t", connection, ok)
			}
			if laneID == "" && s.sessionID != "" {
				t.Fatal("upgrade identity changed legacy data-lane semantics")
			}
			s.observeUpgradeControl(proto.Envelope{T: proto.TStatusNode, D: json.RawMessage(`[]`)})
			if s.upgradeReady.Load() {
				t.Fatal("malformed status made a session ready")
			}
			frame := proto.Envelope{T: proto.THeartbeat}
			if laneID != "" {
				frame = proto.Envelope{T: proto.TStatusNode, D: mustJSON(t, proto.NodeStatus{})}
			}
			if err := json.NewEncoder(peer).Encode(frame); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				if event.State != "registered" || event.SessionID != connection.SessionID || event.NodeUUID != n.UUID || event.Version != "target" {
					t.Fatalf("formal observation=%+v", event)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no formal registration observation")
			}
			current, _ := h.AgentUpgradeConnection(n.UUID)
			if !current.Ready {
				t.Fatal("formal control traffic did not make session ready")
			}
			s.observeUpgradeControl(proto.Envelope{T: proto.THeartbeat})
			select {
			case e := <-events:
				t.Fatalf("duplicate registered observation=%+v", e)
			default:
			}
		})
	}
}

func TestAgentUpgradeTransportBindsTheAttemptSession(t *testing.T) {
	h, uuid, client := newUpgradeHub(t, []string{proto.CapUpgradePush})
	s := h.session(uuid)
	s.upgradeSessionID = "old-control-session"
	s.upgradeVersion = "old-version"
	s.upgradeHandshakeComplete.Store(true)
	events := make(chan agentupgrade.Observation, 4)
	h.SetAgentUpgradeObserver(func(e agentupgrade.Observation) { events <- e })
	if err := h.PushAgentUpgrade(context.Background(), uuid, "stale-session", "gpuworks-agent-linux-amd64", []byte("agent")); err == nil {
		t.Fatal("stale session accepted a push")
	}
	if err := h.RequestAgentUpgrade(context.Background(), uuid, "stale-session"); err == nil {
		t.Fatal("stale session accepted an RPC")
	}
	got := fakeUpgradeAgent(t, client, nil)
	if err := h.PushAgentUpgrade(context.Background(), uuid, s.upgradeSessionID, "gpuworks-agent-linux-amd64", []byte("agent")); err != nil {
		t.Fatal(err)
	}
	select {
	case transfer := <-got:
		if string(transfer.body) != "agent" {
			t.Fatalf("binary=%q", transfer.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no pushed bytes")
	}
	select {
	case e := <-events:
		t.Fatalf("verified became completed: %+v", e)
	default:
	}
	if current, _ := h.AgentUpgradeConnection(uuid); current.Ready {
		t.Fatal("verified made the original session ready")
	}
	raw := mustJSON(t, proto.AgentUpgradeStatus{State: "failed", Error: "install failed after verification"})
	s.handle(proto.Envelope{T: proto.TAgentUpgradeStatus, D: raw})
	e := <-events
	if e.State != "failed" || e.SessionID != s.upgradeSessionID || e.Version != "old-version" || e.Error != "install failed after verification" {
		t.Fatalf("failure lost attempt identity: %+v", e)
	}
}

func TestAgentUpgradePushRefusalIsDistinctFromLostTransport(t *testing.T) {
	h, uuid, client := newUpgradeHub(t, []string{proto.CapUpgradePush})
	s := h.session(uuid)
	s.upgradeSessionID = "attempt"
	fakeUpgradeAgent(t, client, func(pushedUpgrade) proto.AgentUpgradeStatus {
		return proto.AgentUpgradeStatus{State: "failed", Error: "agent upgrade already in progress"}
	})
	err := h.PushAgentUpgrade(context.Background(), uuid, "attempt", "gpuworks-agent-linux-amd64", []byte(strings.Repeat("x", 1<<20)))
	var refused *agentupgrade.RejectedError
	if !errors.As(err, &refused) || !refused.Busy {
		t.Fatalf("busy response=%v", err)
	}
	h2, uuid2, _ := newUpgradeHub(t, []string{proto.CapUpgradePush})
	h2.session(uuid2).upgradeSessionID = "attempt2"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = h2.PushAgentUpgrade(ctx, uuid2, "attempt2", "gpuworks-agent-linux-amd64", []byte("agent"))
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("lost transport classified as rejection: %v", err)
	}
}

func TestAgentUpgradeRPCPreservesRemoteRefusalClassification(t *testing.T) {
	for _, detail := range []string{"", "agent upgrade already in progress", "self-upgrade unsupported"} {
		t.Run("reply="+detail, func(t *testing.T) {
			h, uuid, _ := newUpgradeHub(t, nil)
			s := h.session(uuid)
			s.upgradeSessionID = "rpc-attempt"
			s.sendFn = func(kind string, id uint64, body any) error {
				req, ok := body.(rpcRequest)
				if kind != proto.TRPCReq || !ok || req.Method != proto.RPCAgentUpgrade {
					t.Errorf("request=%s %#v", kind, body)
				}
				s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: id, D: mustJSON(t, rpcResponse{Error: detail, Result: json.RawMessage(`{"state":"upgrading"}`)})})
				return nil
			}
			err := h.RequestAgentUpgrade(context.Background(), uuid, "rpc-attempt")
			var refused *agentupgrade.RejectedError
			if detail == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &refused) || refused.Busy != strings.Contains(detail, "already in progress") {
				t.Fatalf("RPC refusal=%v", err)
			}
		})
	}
}

func TestAgentUpgradeConnectionDoesNotExposeAClosedSession(t *testing.T) {
	h, uuid, _ := newUpgradeHub(t, nil)
	s := h.session(uuid)
	s.upgradeSessionID = "closed-attempt"
	s.upgradeReady.Store(true)
	s.markClosed()
	if _, connected := h.AgentUpgradeConnection(uuid); connected {
		t.Fatal("closed control session was reported connected")
	}
	if err := h.RequestAgentUpgrade(context.Background(), uuid, "closed-attempt"); err == nil {
		t.Fatal("closed session accepted upgrade")
	}
}

func TestAgentUpgradeLateObservationKeepsItsOriginalIdentity(t *testing.T) {
	h := newTestHub(t)
	_, token, err := h.store.CreateNodePending("upgrade-late")
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan agentupgrade.Observation, 8)
	h.SetAgentUpgradeObserver(func(e agentupgrade.Observation) { events <- e })
	old, _, err := upgradeHandshakePeer(t, h, token, "old", "", false)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := upgradeHandshakePeer(t, h, token, "target", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if next.upgradeSessionID == old.upgradeSessionID {
		t.Fatal("legacy reconnect reused the upgrade session identity")
	}
	old.observeUpgradeControl(proto.Envelope{T: proto.THeartbeat})
	select {
	case e := <-events:
		t.Fatalf("replaced session emitted registration: %+v", e)
	default:
	}
	old.observeUpgradeFailure(proto.AgentUpgradeStatus{State: "failed", Error: "late installation failure"})
	event := <-events
	if event.SessionID != old.upgradeSessionID || event.Version != "old" || event.State != "failed" {
		t.Fatalf("late event rebound to replacement: %+v", event)
	}
	h.SetAgentUpgradeObserver(nil)
	old.observeUpgradeFailure(proto.AgentUpgradeStatus{State: "failed", Error: "observer removed"})
}
