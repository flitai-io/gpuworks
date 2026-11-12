package modelsync

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// bridgeToReceiver models an opaque relay: it sees encrypted bytes only and
// uses the same receiver as a direct connection.
func bridgeToReceiver(t *testing.T, receiver *Receiver) net.Conn {
	t.Helper()
	source, relay := net.Pipe()
	target, err := net.Dial("tcp", receiver.Endpoints[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close(); _ = relay.Close(); _ = target.Close() })
	go func() { _, _ = io.Copy(target, relay); _ = target.Close(); _ = relay.Close() }()
	go func() { _, _ = io.Copy(relay, target); _ = target.Close(); _ = relay.Close() }()
	return source
}

func TestDirectUnavailableOnlyBeforeAConnectionIsEstablished(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	_ = closed.Close()
	r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "token", Receive: func(context.Context, io.Reader) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	for _, tc := range []struct {
		name                  string
		endpoints             []string
		canceled, unavailable bool
	}{
		{"refused", []string{address}, false, true},
		{"wrong fingerprint", r.Endpoints, false, false},
		{"mixed refused and wrong fingerprint", append([]string{address}, r.Endpoints...), false, false},
		{"canceled", []string{address}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if tc.canceled {
				cancel()
			}
			calls := 0
			_, err := Send(ctx, tc.endpoints, nil, "token", strings.Repeat("0", 64), func(io.Writer) error { calls++; return nil })
			if err == nil || errors.Is(err, ErrDirectUnavailable) != tc.unavailable {
				t.Fatalf("err=%v, want unavailable=%v", err, tc.unavailable)
			}
			if calls != 0 {
				t.Fatal("archive callback ran before verified TLS")
			}
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestSendOnConnRejectsAuthenticationAndReceiverFailures(t *testing.T) {
	for _, tc := range []string{"fingerprint", "token", "archive kind", "receiver limit", "source read"} {
		t.Run(tc, func(t *testing.T) {
			r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "token", MaxBytes: 8, Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Stop)
			token, fp, magic := "token", r.Fingerprint, protocolMagic
			switch tc {
			case "fingerprint":
				fp = strings.Repeat("0", 64)
			case "token":
				token = "wrong"
			case "archive kind":
				magic = "wrong"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			calls := 0
			sourceErr := errors.New("source archive read failed")
			_, err = SendOnConnWithMagic(ctx, bridgeToReceiver(t, r), token, fp, magic, func(w io.Writer) error {
				calls++
				if tc == "source read" {
					return sourceErr
				}
				_, err := io.WriteString(w, "payload exceeds limit")
				return err
			})
			if err == nil || errors.Is(err, ErrDirectUnavailable) {
				t.Fatalf("unsafe fallback/success: %v", err)
			}
			if calls > 1 {
				t.Fatalf("archive callback replayed: %d", calls)
			}
			if tc == "fingerprint" && calls != 0 {
				t.Fatal("wrong target received archive callback")
			}
			if tc == "source read" && !errors.Is(err, sourceErr) {
				t.Fatalf("source error lost: %v", err)
			}
			if tc == "receiver limit" && !strings.Contains(err.Error(), "8-byte limit") {
				t.Fatalf("receiver rejection lost: %v", err)
			}
		})
	}
}

func TestSendOnConnRejectsAFalseAcknowledgement(t *testing.T) {
	cert, fp, err := ephemeralCertificate()
	if err != nil {
		t.Fatal(err)
	}
	raw, peer := net.Pipe()
	t.Cleanup(func() { _ = raw.Close(); _ = peer.Close() })
	serverDone := make(chan error, 1)
	go func() {
		conn := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := readLine(reader, 64<<10); err != nil {
			serverDone <- err
			return
		}
		if _, err := io.Copy(io.Discard, &chunkReader{r: reader}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- writeJSONLine(conn, acknowledgement{OK: true, Bytes: 1, SHA256: strings.Repeat("0", 64)})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = SendOnConnWithMagic(ctx, raw, "token", fp, "", func(w io.Writer) error { _, err := io.WriteString(w, "archive"); return err })
	if err == nil || !strings.Contains(err.Error(), "confirmation mismatch") || errors.Is(err, ErrDirectUnavailable) {
		t.Fatalf("false ack: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSendOnConnClosesOnValidationAndCancellation(t *testing.T) {
	for _, kind := range []string{"credentials", "callback", "fingerprint", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			raw, peer := net.Pipe()
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			token, fp := "token", strings.Repeat("0", 64)
			writer := func(io.Writer) error { return nil }
			switch kind {
			case "credentials":
				token = ""
			case "callback":
				writer = nil
			case "fingerprint":
				fp = "bad"
			case "cancel":
				cancel()
			}
			_, err := SendOnConnWithMagic(ctx, raw, token, fp, "", writer)
			if err == nil {
				t.Fatal("invalid transport succeeded")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("owned raw connection not closed: %v", err)
			}
		})
	}
}

func TestSendOnConnCancellationInterruptsPayload(t *testing.T) {
	r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "token", Receive: func(_ context.Context, in io.Reader) error { _, err := io.Copy(io.Discard, in); return err }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	raw := bridgeToReceiver(t, r)
	go func() {
		_, err := SendOnConnWithMagic(ctx, raw, "token", r.Fingerprint, "", func(w io.Writer) error {
			close(started)
			payload := make([]byte, 128<<10)
			for {
				if _, err := w.Write(payload); err != nil {
					return err
				}
			}
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("payload did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel cause lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sender remained blocked")
	}
}

func TestSendOnConnPreservesConfirmedArchiveProtocol(t *testing.T) {
	for _, magic := range []string{protocolMagic, "GPUWORKS-IMAGE/1"} {
		t.Run(magic, func(t *testing.T) {
			payload := bytes.Repeat([]byte("opaque-relay-payload"), 4096)
			var got bytes.Buffer
			r, err := StartReceiver(context.Background(), ReceiveOptions{
				BindHost: "127.0.0.1", Token: "one-use-token", Magic: magic,
				Receive: func(_ context.Context, in io.Reader) error { _, err := io.Copy(&got, in); return err },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sent, err := SendOnConnWithMagic(ctx, bridgeToReceiver(t, r), "one-use-token", r.Fingerprint, magic, func(out io.Writer) error { _, err := out.Write(payload); return err })
			if err != nil {
				t.Fatal(err)
			}
			received, err := r.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(payload)
			if !bytes.Equal(payload, got.Bytes()) || sent != received || sent.Bytes != int64(len(payload)) || sent.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("sent=%+v received=%+v payload=%d", sent, received, got.Len())
			}
		})
	}
}
