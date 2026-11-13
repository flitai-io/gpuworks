package conn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type relayTestHandlers struct {
	inboundHandlers
	send    func(context.Context, string, json.RawMessage, net.Conn) (any, error)
	receive func(context.Context, string, string) (net.Conn, error)
}

func (h *relayTestHandlers) StreamPeerSend(ctx context.Context, m string, p json.RawMessage, c net.Conn) (any, error) {
	return h.send(ctx, m, p, c)
}
func (h *relayTestHandlers) PeerRelayReceiver(ctx context.Context, k, id string) (net.Conn, error) {
	return h.receive(ctx, k, id)
}

func TestPeerRelaySenderReturnsOnlyControlResult(t *testing.T) {
	for _, method := range []string{proto.RPCModelSyncSend, proto.RPCImageSyncProbe, "arbitrary.proxy"} {
		t.Run(method, func(t *testing.T) {
			rig := newStorageStreamRig(t)
			ctrl := &nopRWC{}
			rig.conn.ctrl = ctrl
			called := false
			rig.conn.handlers = &relayTestHandlers{send: func(_ context.Context, m string, p json.RawMessage, c net.Conn) (any, error) {
				called = true
				if m != method || string(p) != `{"allow_relay":true}` {
					return nil, errors.New("wrong method or parameters")
				}
				if _, err := io.WriteString(c, "TLS-only"); err != nil {
					return nil, err
				}
				return nil, errors.New("archive confirmation failed")
			}}
			stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamPeerRelaySend})
			params, _ := json.Marshal(rpcReqBody{Method: method, Params: json.RawMessage(`{"allow_relay":true}`)})
			if err := json.NewEncoder(stream).Encode(proto.Envelope{T: proto.TRPCReq, ID: 42, D: params}); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, done)
			allowed := method != "arbitrary.proxy"
			if called != allowed {
				t.Fatalf("handler called=%v", called)
			}
			if allowed && string(data) != "TLS-only" {
				t.Fatalf("RPC result polluted data stream: %q", data)
			}
			var env proto.Envelope
			if err := json.Unmarshal(ctrl.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			var body rpcRespBody
			if err := json.Unmarshal(env.D, &body); err != nil {
				t.Fatal(err)
			}
			if env.ID != 42 || env.T != proto.TRPCResp || body.Error == "" {
				t.Fatalf("lost control result: %s", ctrl.Bytes())
			}
		})
	}
}

func TestPeerRelayReceiverReadyAndDuplexResponse(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "refused"}[fail], func(t *testing.T) {
			rig := newStorageStreamRig(t)
			rig.conn.handlers = &relayTestHandlers{receive: func(ctx context.Context, kind, id string) (net.Conn, error) {
				if kind != "model" || id != "transfer" {
					return nil, errors.New("wrong transfer")
				}
				if fail {
					return nil, errors.New(strings.Repeat("unavailable", 500))
				}
				local, peer := net.Pipe()
				go func() {
					defer peer.Close()
					buf := make([]byte, 7)
					if _, err := io.ReadFull(peer, buf); err == nil && string(buf) == "payload" {
						io.WriteString(peer, "confirmed")
					}
				}()
				return local, nil
			}}
			stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamPeerRelayReceive, Target: "model", Ref: "transfer"})
			reader := bufio.NewReader(stream)
			line, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var ready proto.PeerRelayReady
			if err = json.Unmarshal(line, &ready); err != nil {
				t.Fatal(err)
			}
			if fail {
				if len(ready.Error) == 0 || len(line) > 1100 {
					t.Fatalf("unbounded or missing rejection: %d", len(line))
				}
			} else {
				if ready.Error != "" {
					t.Fatal(ready.Error)
				}
				if _, err = io.WriteString(stream, "payload"); err != nil {
					t.Fatal(err)
				}
				response, err := io.ReadAll(reader)
				if err != nil || string(response) != "confirmed" {
					t.Fatalf("response=%q err=%v", response, err)
				}
			}
			stream.Close()
			waitDone(t, done)
		})
	}
}

func TestPeerRelaySenderRejectsOversizedInvocation(t *testing.T) {
	rig := newStorageStreamRig(t)
	rig.conn.handlers = &relayTestHandlers{send: func(context.Context, string, json.RawMessage, net.Conn) (any, error) {
		t.Error("oversized invocation reached handler")
		return nil, nil
	}}
	stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamPeerRelaySend})
	_, _ = io.WriteString(stream, strings.Repeat("x", 64*1024+1)+"\n")
	waitDone(t, done)
	stream.Close()
}

func TestPeerRelayReceiverCancellationClosesBothSides(t *testing.T) {
	client, server := pipe(t)
	local, peer := net.Pipe()
	defer peer.Close()
	c := &Conn{handlers: &relayTestHandlers{receive: func(context.Context, string, string) (net.Conn, error) { return local, nil }}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, err := server.AcceptStream()
		if err == nil {
			c.handlePeerRelayReceive(ctx, proto.StreamHeader{Target: "model", Ref: "owned"}, s)
		}
	}()
	stream, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := bufio.NewReader(stream).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitDone(t, done)
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("local receiver connection remained open")
	}
}

func TestPeerRelaySenderStreamCloseCancelsBlockedProducer(t *testing.T) {
	rig := newStorageStreamRig(t)
	ctrl := &nopRWC{}
	rig.conn.ctrl = ctrl
	started := make(chan struct{})
	canceled := make(chan struct{})
	rig.conn.handlers = &relayTestHandlers{send: func(ctx context.Context, _ string, _ json.RawMessage, _ net.Conn) (any, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}}
	stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamPeerRelaySend})
	req, _ := json.Marshal(rpcReqBody{Method: proto.RPCImageSyncSend, Params: json.RawMessage(`{"allow_relay":true}`)})
	if err := json.NewEncoder(stream).Encode(proto.Envelope{T: proto.TRPCReq, ID: 7, D: req}); err != nil {
		t.Fatal(err)
	}
	<-started
	stream.Close()
	waitDone(t, done)
	select {
	case <-canceled:
	default:
		t.Fatal("source producer context remained active")
	}
	if !strings.Contains(string(ctrl.BytesCopy()), "context canceled") {
		t.Fatalf("canceled producer lost ctrl result: %s", ctrl.BytesCopy())
	}
}

func TestPeerRelayReadPumpDrainsFragmentedAcknowledgementBeforeEOF(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffered := newPeerRelaySourceConn(raw, cancel)
	defer buffered.Close()
	const ack = "an acknowledgement split into single byte reads"
	go func() {
		for _, b := range []byte(ack) {
			if _, err := peer.Write([]byte{b}); err != nil {
				return
			}
		}
		peer.Close()
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("read pump did not observe EOF")
	}
	data, err := io.ReadAll(buffered)
	if err != nil || string(data) != ack {
		t.Fatalf("buffered acknowledgement=%q err=%v", data, err)
	}
}

func TestPeerRelayReadPumpBoundsUnconsumedInput(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffered := newPeerRelaySourceConn(raw, cancel)
	defer buffered.Close()
	go func() { _, _ = peer.Write(make([]byte, 128*1024+1)) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("unconsumed input did not cancel sender")
	}
	data, err := io.ReadAll(buffered)
	if err == nil || !strings.Contains(err.Error(), "buffer limit") || len(data) > 128*1024 {
		t.Fatalf("data=%d err=%v", len(data), err)
	}
}
