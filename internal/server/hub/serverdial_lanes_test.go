package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestServerDialIndependentDataLaneLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, token, err := st.CreateNodePending("reverse-data")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(st, log)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	uuid := ""
	registered := make(chan *agentconn.Conn, 1)
	done := make(chan error, 1)
	go func() {
		done <- agentconn.ServeServerDial(ctx, ln, func() *agentconn.Conn {
			mu.Lock()
			defer mu.Unlock()
			var c *agentconn.Conn
			c = agentconn.New(agentconn.Credentials{NodeUUID: uuid, NodeToken: token, ConnectMode: "server_dial", DataLane: true}, func(id, newToken string) error {
				mu.Lock()
				defer mu.Unlock()
				uuid = id
				if newToken != "" {
					token = newToken
				}
				registered <- c
				return nil
			}, reverseDataHandlers{}, emptyProber{}, log)
			return c
		}, nil)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("listener workers leaked")
		}
	}()
	mu.Lock()
	bootstrapToken := token
	mu.Unlock()
	dialCtx, cancelDial := context.WithCancel(ctx)
	if err := h.DialAgentWithSecret(dialCtx, ln.Addr().String(), bootstrapToken); err != nil {
		t.Fatal(err)
	}
	cancelDial() // the short onboarding request does not own either physical lane
	s := h.session(node.UUID)
	if s == nil {
		t.Fatal("control session missing")
	}
	defer s.Close()
	if s.sessionID == "" {
		t.Fatal("reverse control handshake did not issue a data parent session")
	}
	waitFor(t, 5*time.Second, func() bool { return s.DataLaneState() == "attached" }, "reverse data attach")
	agent := <-registered
	waitFor(t, time.Second, func() bool {
		return proto.ConnectionValue(agent.ConnectionReport(), "connection.data_lane") == "attached"
	}, "agent reverse data report")
	s.dataMu.Lock()
	first := s.data
	s.dataMu.Unlock()
	if first == s.ysess {
		t.Fatal("data shares physical control connection")
	}

	assertReverseModelData(t, s)
	if _, err := probeServerDialLane(ln.Addr().String(), bootstrapToken, proto.LaneCtrl, "", false); err == nil {
		t.Fatal("second control connection displaced current control")
	}
	if _, err := probeServerDialLane(ln.Addr().String(), bootstrapToken, "", "", true); err == nil {
		t.Fatal("legacy connection displaced current control")
	}
	for _, bad := range []string{"old-session", ""} {
		ack, err := probeServerDialLane(ln.Addr().String(), bootstrapToken, proto.LaneData, bad, false)
		if bad == "" {
			if err == nil {
				t.Fatal("empty parent session was accepted")
			}
			continue
		}
		if err != nil || ack.T != proto.TReject {
			t.Fatalf("stale parent response=%+v err=%v", ack, err)
		}
		var reject proto.Reject
		if err := json.Unmarshal(ack.D, &reject); err != nil || reject.Reason != proto.RejectStaleSession {
			t.Fatalf("stale reject=%+v err=%v", reject, err)
		}
	}
	if _, err := probeServerDialLane(ln.Addr().String(), bootstrapToken, proto.LaneData, s.sessionID, true); err == nil {
		t.Fatal("legacy proof acquired data lane")
	}
	if _, err := probeServerDialLane(ln.Addr().String(), "wrong-token", proto.LaneData, s.sessionID, false); err == nil {
		t.Fatal("wrong node token acquired data lane")
	}
	if !h.Online(node.UUID) || first.IsClosed() {
		t.Fatal("rejected data handshake disturbed active session")
	}
	_ = first.Close()
	waitFor(t, 5*time.Second, func() bool {
		s.dataMu.Lock()
		defer s.dataMu.Unlock()
		return s.data != nil && s.data != first && !s.data.IsClosed()
	}, "independent data reconnect")
	if !h.Online(node.UUID) || s.ysess.IsClosed() {
		t.Fatal("data loss disconnected control")
	}
	s.dataMu.Lock()
	second := s.data
	s.dataMu.Unlock()
	s.Close()
	waitFor(t, 5*time.Second, second.IsClosed, "child data close")
	waitFor(t, 5*time.Second, func() bool { return !h.Online(node.UUID) }, "old control removal")
	waitFor(t, 5*time.Second, func() bool { return h.DialAgentWithSecret(ctx, ln.Addr().String(), bootstrapToken) == nil }, "new control registration")
	next := h.session(node.UUID)
	if next == nil {
		t.Fatal("new control missing")
	}
	defer next.Close()
	if next.sessionID == s.sessionID {
		t.Fatal("control generation was reused")
	}
	waitFor(t, 5*time.Second, func() bool { return next.DataLaneState() == "attached" }, "new generation data attach")
	stale, err := probeServerDialLane(ln.Addr().String(), bootstrapToken, proto.LaneData, s.sessionID, false)
	if err != nil || stale.T != proto.TReject {
		t.Fatalf("previous control proof attached to replacement: %+v %v", stale, err)
	}
	assertReverseModelData(t, next)
}

// probeServerDialLane acts as an authenticated server on a second physical
// socket. legacy deliberately signs only the nonce to test downgrade rejection.
func probeServerDialLane(addr, token, lane, parent string, legacy bool) (proto.Envelope, error) {
	var ack proto.Envelope
	raw, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return ack, err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(time.Second))
	line, err := readLine(raw)
	if err != nil {
		return ack, err
	}
	var challenge proto.ServerDialChallenge
	if err := json.Unmarshal(line, &challenge); err != nil {
		return ack, err
	}
	secret := token
	if challenge.NodeUUID != "" {
		secret = proto.ServerDialNodeSecret(secret)
	}
	proof := proto.ServerDialProof{Lane: lane, SessionID: parent, HMAC: proto.ServerDialLaneHMAC(secret, challenge.Nonce, lane, parent)}
	if lane == proto.LaneData {
		proof.AckNonce = strings.Repeat("ab", 32)
	}
	if legacy {
		proof.Lane = ""
		proof.HMAC = proto.ServerDialHMAC(secret, challenge.Nonce)
	}
	wire, _ := json.Marshal(proof)
	if _, err := raw.Write(append(wire, '\n')); err != nil {
		return ack, err
	}
	line, err = readLine(raw)
	if err != nil {
		return ack, err
	}
	err = json.Unmarshal(line, &ack)
	return ack, err
}

type reverseDataHandlers struct{ noopHandlers }

func (reverseDataHandlers) StreamModelArchive(_ context.Context, path string, w io.Writer) error {
	if path != "fixture" {
		return io.ErrUnexpectedEOF
	}
	_, err := w.Write(bytes.Repeat([]byte("model-data"), 8192))
	return err
}
func assertReverseModelData(t *testing.T, s *Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := s.openStream(ctx, proto.StreamHeader{Kind: proto.StreamModelRead, Dst: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("model-data"), 8192)) {
		t.Fatalf("reverse model data=%d bytes", len(got))
	}
	if s.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("model archive used control connection")
	}
}

func TestServerDialRejectsLegacySigningOracle(t *testing.T) {
	server, agent := net.Pipe()
	defer server.Close()
	defer agent.Close()
	done := make(chan error, 1)
	go func() {
		_, err := (&Hub{}).serverDialServerAuthLane(server, "secret", proto.LaneCtrl, "", "")
		done <- err
	}()
	challenge := proto.ServerDialChallenge{Nonce: "gpuworks/server-dial/lane/v1\x00victim-nonce\x00data\x00parent"}
	wire, _ := json.Marshal(challenge)
	if _, err := agent.Write(append(wire, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("crafted legacy challenge was signed")
		}
	case <-time.After(time.Second):
		t.Fatal("crafted nonce reached the proof writer")
	}
}

// An endpoint at a server_dial node's address that is not the node can still
// complete the handshake up to the server's own proof (the server proves
// itself, not the endpoint). It must not become the session's data lane: the
// data lane answers a fresh challenge with the node secret, as the control
// lane does with the node token.
func TestServerDialDataLaneRejectsEndpointWithoutNodeSecret(t *testing.T) {
	for name, ack := range map[string]func(proof proto.ServerDialProof) proto.Envelope{
		"bare ack": func(proto.ServerDialProof) proto.Envelope { return proto.Envelope{T: proto.TDataAck} },
		"wrong secret": func(proof proto.ServerDialProof) proto.Envelope {
			d, _ := json.Marshal(proto.ServerDialDataAck{HMAC: proto.ServerDialDataAckHMAC("not-the-node", proof.AckNonce, proof.SessionID)})
			return proto.Envelope{T: proto.TDataAck, D: d}
		},
		"replayed server proof": func(proof proto.ServerDialProof) proto.Envelope {
			d, _ := json.Marshal(proto.ServerDialDataAck{HMAC: proof.HMAC})
			return proto.Envelope{T: proto.TDataAck, D: d}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHub(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			impostor := make(chan error, 1)
			go func() {
				raw, err := ln.Accept()
				if err != nil {
					impostor <- err
					return
				}
				defer raw.Close()
				challenge, _ := json.Marshal(proto.ServerDialChallenge{NodeUUID: "node-1", Nonce: strings.Repeat("cd", 32), DataLane: true})
				if _, err := raw.Write(append(challenge, '\n')); err != nil {
					impostor <- err
					return
				}
				line, err := readLine(raw)
				if err != nil {
					impostor <- err
					return
				}
				var proof proto.ServerDialProof
				if err := json.Unmarshal(line, &proof); err != nil {
					impostor <- err
					return
				}
				if len(proof.AckNonce) != 64 {
					impostor <- errors.New("server sent no data-lane challenge")
					return
				}
				wire, _ := json.Marshal(ack(proof))
				_, err = raw.Write(append(wire, '\n'))
				impostor <- err
				_, _ = io.Copy(io.Discard, raw)
			}()
			s := &Session{NodeUUID: "node-1", sessionID: "parent-1"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			attached, err := h.dialServerDialDataLane(ctx, s, ln.Addr().String(), "node-secret")
			if e := <-impostor; e != nil {
				t.Fatalf("impostor: %v", e)
			}
			if attached || err == nil || !strings.Contains(err.Error(), "agent proof invalid") {
				t.Fatalf("impostor data lane: attached=%v err=%v", attached, err)
			}
			if s.DataLaneState() == "attached" {
				t.Fatal("impostor became the data lane")
			}
		})
	}
}
