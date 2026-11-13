package api

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// outboundDNSFixture answers ordinary A/AAAA queries over a real local UDP
// socket. Unknown names return NXDOMAIN; "silent*.gwks.test" never replies.
// The product's resolver, not a replacement policy function, consumes it.
func outboundDNSFixture(t *testing.T, records map[string][]netip.Addr) *atomic.Int32 {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			packet := buffer[:n]
			if len(packet) < 12 || binary.BigEndian.Uint16(packet[4:6]) != 1 {
				continue
			}
			end := 12
			var labels []string
			for end < len(packet) && packet[end] > 0 && packet[end] <= 63 {
				size := int(packet[end])
				end++
				if end+size > len(packet) {
					break
				}
				labels = append(labels, string(packet[end:end+size]))
				end += size
			}
			if end+5 > len(packet) || packet[end] != 0 {
				continue
			}
			name := strings.ToLower(strings.Join(labels, "."))
			queries.Add(1)
			if strings.HasPrefix(name, "silent") && strings.HasSuffix(name, ".gwks.test") {
				continue
			}
			kind := binary.BigEndian.Uint16(packet[end+1 : end+3])
			response := make([]byte, 12)
			copy(response[:2], packet[:2])
			response[2], response[3] = 0x81, 0x80 // response, recursion desired/available
			binary.BigEndian.PutUint16(response[4:6], 1)
			response = append(response, packet[12:end+5]...)
			addresses, found := records[name]
			if !found {
				response[3] |= 3 // NXDOMAIN
			}
			var count uint16
			for _, address := range addresses {
				if kind != 1 && kind != 28 || kind == 1 && !address.Is4() || kind == 28 && !address.Is6() {
					continue
				}
				answer := address.AsSlice()
				response = append(response, 0xc0, 0x0c, byte(kind>>8), byte(kind), 0, 1, 0, 0, 0, 60, 0, byte(len(answer)))
				response = append(response, answer...)
				count++
			}
			binary.BigEndian.PutUint16(response[6:8], count)
			_, _ = conn.WriteTo(response, peer)
		}
	}()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	t.Cleanup(func() {
		net.DefaultResolver = previous
		_ = conn.Close()
		<-done
	})
	return &queries
}
