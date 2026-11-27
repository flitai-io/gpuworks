package conn

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type tunnelLog struct {
	sync.Mutex
	buf bytes.Buffer
}

func (l *tunnelLog) Write(p []byte) (int, error) { l.Lock(); defer l.Unlock(); return l.buf.Write(p) }

func (l *tunnelLog) records(t *testing.T) []map[string]any {
	t.Helper()
	l.Lock()
	defer l.Unlock()
	dec := json.NewDecoder(bytes.NewReader(l.buf.Bytes()))
	var records []map[string]any
	for {
		var event map[string]any
		err := dec.Decode(&event)
		if err == io.EOF {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, event)
	}
}

func TestTunnelCaptureDistinguishesFailedDialAndEngineEOF(t *testing.T) {
	for _, failDial := range []bool{true, false} {
		name := "engine_eof"
		if failDial {
			name = "dial_refused"
		}
		t.Run(name, func(t *testing.T) {
			rig := newStorageStreamRig(t)
			logs := &tunnelLog{}
			rig.conn.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			dst := ln.Addr().String()
			if failDial {
				_ = ln.Close()
			} else {
				defer ln.Close()
				go func() {
					engine, err := ln.Accept()
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, engine)
					_ = engine.Close()
				}()
			}
			stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamTunnel, Dst: dst, Trace: &proto.TunnelTrace{RequestID: "req-example", Attempt: 2}})
			defer stream.Close()
			if err := stream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if !failDial {
				if _, err := io.WriteString(stream, "ping"); err != nil {
					t.Fatal(err)
				}
				if err := stream.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}
			body, err := io.ReadAll(stream)
			if err != nil || len(body) != 0 {
				t.Fatalf("unexpected tunnel end: %q %v", body, err)
			}
			waitDone(t, done)
			events := logs.records(t)
			for _, e := range events {
				if e["request_id"] != "req-example" || e["attempt"] != float64(2) || e["stream_id"] != float64(stream.StreamID()) {
					t.Fatalf("unmatched stream: %v", e)
				}
			}
			if len(events) == 0 || events[0]["msg"] != "tunnel dialing" {
				t.Fatalf("missing dial start: %v", events)
			}
			events = events[1:]
			if failDial {
				if len(events) != 1 || events[0]["msg"] != "tunnel dial failed" || events[0]["dst"] != dst || events[0]["err"] == nil {
					t.Fatalf("dial evidence: %v", events)
				}
				return
			}
			if len(events) != 3 || events[0]["msg"] != "tunnel connected" || events[0]["local_addr"] == nil {
				t.Fatalf("connection evidence: %v", events)
			}
			upload, reply := events[1], events[2]
			if upload["side"] != "tunnel" || upload["operation"] != "read" || upload["bytes"] != float64(4) || upload["on_end"] != "half_close_engine_write" ||
				reply["side"] != "engine" || reply["operation"] != "read" || reply["err"] != "EOF" || reply["bytes"] != float64(0) || reply["on_end"] != "close_tunnel" {
				t.Fatalf("copy evidence: %v", events)
			}
		})
	}
}

func TestTunnelCaptureIdentifiesReplyWriteFailure(t *testing.T) {
	rig := newStorageStreamRig(t)
	logs := &tunnelLog{}
	rig.conn.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stream, done := rig.open(t, proto.StreamHeader{Kind: proto.StreamTunnel, Dst: ln.Addr().String(), Trace: &proto.TunnelTrace{RequestID: "reset-example", Attempt: 1}})
	engine, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Reset(); err != nil {
		t.Fatal(err)
	}
	// The agent sees the reset, half-closes the engine socket, and still
	// attempts to deliver any reply. That write must name the tunnel.
	if _, err := io.Copy(io.Discard, engine); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(engine, "reply"); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	for _, e := range logs.records(t) {
		if e["on_end"] == "close_tunnel" && e["side"] == "tunnel" && e["operation"] == "write" && e["err"] != nil && e["err"] != "EOF" {
			return
		}
	}
	t.Fatalf("missing tunnel write error: %v", logs.records(t))
}
