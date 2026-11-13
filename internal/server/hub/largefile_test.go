package hub

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newLargeFileHub returns a hub with one registered node, so a test can drive
// the stream openers against a real session.
func newLargeFileHub(t *testing.T) (*Hub, string) {
	t.Helper()
	h := newTestHub(t)
	node, err := h.store.RegisterNode("large-file-node", store.HashToken("large-file-token"), "large-file-node")
	if err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close(); _ = clientConn.Close() })
	serverYS, _ := yamuxPairData(t)
	session := newSession(h, serverYS, serverConn)
	session.NodeUUID = node.UUID
	h.register(session)
	t.Cleanup(func() { h.deregister(session) })
	return h, node.UUID
}

func TestOpenLargeFileReadRequiresADataLane(t *testing.T) {
	h, uuid := newLargeFileHub(t)

	// Without a lane the read is refused outright. dataLaneRoute would
	// otherwise route it over ctrl (the "none" state's normal behavior for
	// other data-plane kinds), which is exactly what this stream exists to
	// avoid: multi-megabyte transfers must not share the connection that
	// carries the node's heartbeats and desired state.
	conn, err := h.OpenLargeFileRead(context.Background(), uuid, "/models/a/tokenizer.json")
	if err == nil {
		_ = conn.Close()
		t.Fatal("a read without a data lane must fail instead of falling back to ctrl")
	}
	if !strings.Contains(err.Error(), "data lane") {
		t.Fatalf("err = %v, want it to name the missing lane", err)
	}

	if _, err := h.OpenLargeFileRead(context.Background(), "unknown-node", "/models/a/tokenizer.json"); err == nil {
		t.Fatal("an offline node must fail")
	}

	dataServer, _ := yamuxPairData(t)
	session := h.session(uuid)
	session.attachData(dataServer)
	if got := session.DataLaneState(); got != "attached" {
		t.Fatalf("lane state = %q", got)
	}

	// Detached (attached once, now dropped) must also be refused outright,
	// with no multi-second wait for reattachment: this stream's contract is
	// "no lane, no fetch, take the other path" now, not "wait and see".
	_ = dataServer.Close()
	waitState(t, session, "detached")
	start := time.Now()
	if _, err := h.OpenLargeFileRead(context.Background(), uuid, "/models/a/tokenizer.json"); err == nil {
		t.Fatal("a detached data lane must fail immediately, not wait for reattachment")
	} else if !strings.Contains(err.Error(), "data lane") {
		t.Fatalf("err = %v, want it to name the missing lane", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("detached OpenLargeFileRead took %v, want an immediate failure (no reattach wait)", elapsed)
	}

	dataServer2, dataClient := yamuxPairData(t)
	session.attachData(dataServer2)
	waitState(t, session, "attached")

	opened, err := h.OpenLargeFileRead(context.Background(), uuid, "/models/a/tokenizer.json")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	// The lane the server opened on is the data session, and the header names
	// the absolute path it is reading.
	stream, err := dataClient.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var header proto.StreamHeader
	if err := json.NewDecoder(stream).Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.Kind != proto.StreamLargeFileRead || header.Dst != "/models/a/tokenizer.json" {
		t.Fatalf("header = %+v", header)
	}
	if proto.LaneFor(proto.StreamLargeFileRead) != proto.LaneData {
		t.Fatalf("large file reads must ride the data lane, got %q", proto.LaneFor(proto.StreamLargeFileRead))
	}
	_, _ = io.WriteString(stream, `{"model":{}}`)
	// The agent side of a bounded read ends the stream at EOF, which is what
	// tells the server the file is complete.
	_ = stream.Close()
	raw, err := io.ReadAll(opened)
	if err != nil || string(raw) != `{"model":{}}` {
		t.Fatalf("read %q err=%v", raw, err)
	}
}
