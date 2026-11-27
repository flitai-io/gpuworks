package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestOpenTunnelTraceIsOptionalOnTheWire(t *testing.T) {
	server, agent := yamuxPairData(t)
	h := &Hub{sessions: map[string]*Session{"node": {ysess: server}}}
	ctx := context.Background()
	traced := proto.WithTunnelTrace(ctx, &proto.TunnelTrace{RequestID: "example", Attempt: 2})
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{
		{ctx, `{"kind":"tunnel","dst":"localhost:1234"}`},
		{traced, `{"kind":"tunnel","dst":"localhost:1234","trace":{"request_id":"example","attempt":2}}`},
	} {
		conn, err := h.OpenTunnel(tc.ctx, "node", "localhost:1234")
		if err != nil {
			t.Fatal(err)
		}
		peer, err := agent.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(peer).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var hdr proto.StreamHeader
		if err := json.Unmarshal(line, &hdr); err != nil {
			t.Fatal(err)
		}
		if string(line) != tc.want+"\n" || hdr.Dst != "localhost:1234" {
			t.Fatalf("unexpected stream header %s", line)
		}
		_ = peer.Close()
		_ = conn.Close()
	}
}
