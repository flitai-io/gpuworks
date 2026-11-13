//go:build linux || darwin

package conn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

type ownedTerminalHandlers struct {
	streamTestHandlers
	argv       []string
	prepareErr error
	closeErr   error
	closed     chan error
	fallback   bool
}

func (h *ownedTerminalHandlers) ContainerTerminalArgv(context.Context, string, []string) ([]string, error) {
	h.fallback = true
	return nil, errors.New("legacy fallback called")
}
func (h *ownedTerminalHandlers) ContainerTerminalSession(ctx context.Context, _ string, _ []string) ([]string, func(context.Context) error, error) {
	if h.prepareErr != nil {
		return nil, nil, h.prepareErr
	}
	return h.argv, func(closeCtx context.Context) error {
		if ctx.Err() != context.Canceled {
			h.closed <- errors.New("prepare context not finished")
			return h.closeErr
		}
		if closeCtx.Err() != nil {
			h.closed <- errors.New("close reused canceled context")
			return h.closeErr
		}
		deadline, ok := closeCtx.Deadline()
		if !ok || time.Until(deadline) > 11*time.Second {
			h.closed <- errors.New("close context unbounded")
			return h.closeErr
		}
		h.closed <- nil
		return h.closeErr
	}, nil
}

func TestContainerTerminalOwnsCloseOnPeerDisconnectAndOpenFailure(t *testing.T) {
	for _, name := range []string{"normal", "nonzero-shell", "disconnect", "disconnect-close-error", "open-error", "open-and-close-error", "prepare-error", "close-error"} {
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			client, err := yamux.Client(left, yamux.DefaultConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := yamux.Server(right, yamux.DefaultConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			h := &ownedTerminalHandlers{argv: []string{"/bin/sh", "-c", "printf OWNED_READY; read line"}, closed: make(chan error, 1)}
			switch name {
			case "normal":
				h.argv = []string{"/bin/sh", "-c", "exit 0"}
			case "nonzero-shell":
				h.argv = []string{"/bin/sh", "-c", "exit 7"}
			case "open-error", "open-and-close-error":
				h.argv = []string{"/missing-gpuworks-shell"}
				if name == "open-and-close-error" {
					h.closeErr = errors.New("owned close failed")
				}
			case "prepare-error":
				h.prepareErr = errors.New("prepare denied")
			case "disconnect-close-error":
				h.closeErr = errors.New("owned close failed")
			case "close-error":
				h.argv = []string{"/bin/sh", "-c", "exit 0"}
				h.closeErr = errors.New("owned close failed")
			}
			var logs bytes.Buffer
			c := &Conn{handlers: h, log: slog.New(slog.NewTextHandler(&logs, nil))}
			peer, err := client.OpenStream(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				accepted, err := server.AcceptStream()
				if err == nil {
					c.handleTerminal(proto.StreamHeader{Target: proto.TermTargetContainer, Ref: "owned-container", Rows: 24, Cols: 80}, accepted)
				}
			}()
			_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			if strings.HasPrefix(name, "disconnect") {
				frames := proto.NewTermFrameReader(peer)
				for {
					frame, err := frames.Next()
					if err != nil {
						t.Fatal(err)
					}
					if frame.Type == proto.TermData && strings.Contains(string(frame.Payload), "OWNED_READY") {
						break
					}
				}
				_ = peer.Close()
			} else {
				raw, readErr := io.ReadAll(peer)
				if readErr != nil {
					t.Fatal(readErr)
				}
				frames := proto.NewTermFrameReader(bytes.NewReader(raw))
				var codes []int32
				for {
					frame, err := frames.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if frame.Type == proto.TermExit {
						code, _, err := proto.DecodeExit(frame.Payload)
						if err != nil {
							t.Fatal(err)
						}
						codes = append(codes, code)
					}
				}
				want := int32(-1)
				if name == "normal" {
					want = 0
				} else if name == "nonzero-shell" {
					want = 7
				}
				if len(codes) != 1 || codes[0] != want {
					t.Errorf("first and only terminal outcome must be %d: codes=%v", want, codes)
				}
				_ = peer.Close()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("container handler did not release")
			}
			if h.fallback {
				t.Fatal("owned session failure fell back to unmanaged argv")
			}
			if name == "prepare-error" {
				select {
				case <-h.closed:
					t.Fatal("failed prepare returned a close owner")
				default:
				}
				return
			}
			select {
			case err := <-h.closed:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("remote close callback was skipped")
			}
			if strings.Contains(name, "close-error") && !strings.Contains(logs.String(), "owned close failed") {
				t.Fatalf("close failure was hidden: %s", logs.String())
			}
		})
	}
}
