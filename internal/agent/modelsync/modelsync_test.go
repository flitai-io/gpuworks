package modelsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/traffic"
)

func TestDirectTLSTransferConfirmsBytesAndDigest(t *testing.T) {
	payload := bytes.Repeat([]byte("gpuworks-model-sync"), 200000)
	before := traffic.Totals()[traffic.RoleStorageSync]
	var received bytes.Buffer
	var progress int64
	receiver, err := StartReceiver(context.Background(), ReceiveOptions{
		BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"},
		Token: "one-time-token", MaxBytes: int64(len(payload)) + 1,
		Receive: func(_ context.Context, r io.Reader) error {
			_, err := io.Copy(&received, r)
			return err
		},
		Progress: func(n int64) { progress = n },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sent, err := Send(ctx, receiver.Endpoints, nil, "one-time-token", receiver.Fingerprint, func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	receivedResult, err := receiver.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Bytes(), payload) {
		t.Fatalf("received %d bytes, want %d", received.Len(), len(payload))
	}
	if sent != receivedResult || sent.Bytes != int64(len(payload)) || len(sent.SHA256) != 64 {
		t.Fatalf("sender/receiver result = %+v / %+v", sent, receivedResult)
	}
	if progress == 0 {
		t.Fatal("expected receive progress callback")
	}
	// Both ends are this process: the payload counts once as sent and once
	// as received storage-sync traffic.
	after := traffic.Totals()[traffic.RoleStorageSync]
	if after[0]-before[0] != uint64(len(payload)) || after[1]-before[1] != uint64(len(payload)) {
		t.Fatalf("storage sync traffic grew by rx %d / tx %d, want %d each", after[0]-before[0], after[1]-before[1], len(payload))
	}
}

func TestDirectTransferProbesEndpointsConcurrently(t *testing.T) {
	// Keep the first candidate connected but silent: a sequential dialer would
	// wait for the caller's handshake deadline here before trying the usable address.
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	defer stall.Close()
	go func() {
		conn, acceptErr := stall.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		<-release
	}()

	payload := []byte("concurrent model sync probe")
	var received bytes.Buffer
	receiver, err := StartReceiver(context.Background(), ReceiveOptions{
		BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "probe-token",
		Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(&received, r); return err },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	sent, err := Send(ctx, append([]string{stall.Addr().String()}, receiver.Endpoints...), nil, "probe-token", receiver.Fingerprint, func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("usable endpoint took %s after a stalled candidate", elapsed)
	}
	if received.String() != string(payload) || sent.Bytes != int64(len(payload)) {
		t.Fatalf("transfer result = %+v, received %q", sent, received.Bytes())
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if _, err := receiver.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDirectTransferRejectsWrongTokenAndFingerprint(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		receiver, err := StartReceiver(context.Background(), ReceiveOptions{
			BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "correct",
			Receive: func(context.Context, io.Reader) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = Send(ctx, receiver.Endpoints, nil, "wrong", receiver.Fingerprint, func(io.Writer) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("send error = %v", err)
		}
		if _, err := Send(ctx, receiver.Endpoints, nil, "correct", receiver.Fingerprint, func(io.Writer) error { return nil }); err != nil {
			t.Fatalf("valid sender after rejected token: %v", err)
		}
		if _, err := receiver.Wait(ctx); err != nil {
			t.Fatalf("receiver after rejected token: %v", err)
		}
	})

	t.Run("fingerprint", func(t *testing.T) {
		receiver, err := StartReceiver(context.Background(), ReceiveOptions{
			BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "correct",
			Receive: func(context.Context, io.Reader) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = Send(ctx, receiver.Endpoints, nil, "correct", strings.Repeat("0", 64), func(io.Writer) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
			t.Fatalf("send error = %v", err)
		}
		if _, err := Send(ctx, receiver.Endpoints, nil, "correct", receiver.Fingerprint, func(io.Writer) error { return nil }); err != nil {
			t.Fatalf("valid sender after rejected fingerprint: %v", err)
		}
		if _, err := receiver.Wait(ctx); err != nil {
			t.Fatalf("receiver after rejected fingerprint: %v", err)
		}
	})
}

func TestDirectTransferLimitAndCancellation(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		receiver, err := StartReceiver(context.Background(), ReceiveOptions{
			BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "token", MaxBytes: 8,
			Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = Send(ctx, receiver.Endpoints, nil, "token", receiver.Fingerprint, func(w io.Writer) error {
			_, err := io.WriteString(w, "payload larger than limit")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds 8-byte limit") {
			t.Fatalf("send error = %v", err)
		}
		if _, err := receiver.Wait(ctx); err == nil || !strings.Contains(err.Error(), "exceeds 8-byte limit") {
			t.Fatalf("receiver error = %v", err)
		}
	})

	t.Run("cancel before accept", func(t *testing.T) {
		receiver, err := StartReceiver(context.Background(), ReceiveOptions{
			BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "token",
			Receive: func(context.Context, io.Reader) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		receiver.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := receiver.Wait(ctx); err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("receiver error = %v", err)
		}
	})

	t.Run("cancel active sender", func(t *testing.T) {
		receiver, err := StartReceiver(context.Background(), ReceiveOptions{
			BindHost: "127.0.0.1", AdvertiseHosts: []string{"127.0.0.1"}, Token: "token",
			Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		sendDone := make(chan error, 1)
		go func() {
			_, err := Send(ctx, receiver.Endpoints, nil, "token", receiver.Fingerprint, func(w io.Writer) error {
				payload := bytes.Repeat([]byte("x"), maxChunkSize)
				close(started)
				for {
					if _, err := w.Write(payload); err != nil {
						return err
					}
				}
			})
			sendDone <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("sender did not start")
		}
		cancel()
		select {
		case err := <-sendDone:
			if err == nil {
				t.Fatal("active sender unexpectedly succeeded after cancellation")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("active sender ignored context cancellation")
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if _, err := receiver.Wait(waitCtx); err == nil {
			t.Fatal("receiver unexpectedly accepted canceled transfer")
		}
	})
}

func TestChunkReaderRejectsTruncatedFrame(t *testing.T) {
	var framed bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 10)
	framed.Write(size[:])
	framed.WriteString("short")
	_, err := io.ReadAll(&chunkReader{r: &framed})
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("truncated frame error = %v", err)
	}
}

func TestChunkReaderRejectsMissingTerminator(t *testing.T) {
	var framed bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 5)
	framed.Write(size[:])
	framed.WriteString("hello")
	_, err := io.ReadAll(&chunkReader{r: &framed})
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("missing terminator error = %v", err)
	}
}

func TestLocalIPAcceptsOnlyThisHostsAddresses(t *testing.T) {
	if ip, err := LocalIP(""); err != nil || ip != nil {
		t.Fatalf("no preference = %v, %v", ip, err)
	}
	if ip, err := LocalIP("127.0.0.1"); err != nil || !ip.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("loopback = %v, %v", ip, err)
	}
	if _, err := LocalIP("not-an-ip"); err == nil {
		t.Fatal("a hostname was accepted")
	}
	// TEST-NET-1 is never configured on a host.
	if _, err := LocalIP("192.0.2.77"); err == nil || !strings.Contains(err.Error(), "not an address of this node") {
		t.Fatalf("foreign address err = %v", err)
	}
}

func TestListenAddrsBindsOnlyTheChosenAddress(t *testing.T) {
	bind, advertise, err := ListenAddrs("127.0.0.1")
	if err != nil || bind != "127.0.0.1" || len(advertise) != 1 || advertise[0] != "127.0.0.1" {
		t.Fatalf("chosen = %q %v %v", bind, advertise, err)
	}
	bind, _, err = ListenAddrs("")
	if err != nil || bind != "0.0.0.0" {
		t.Fatalf("no preference = %q %v", bind, err)
	}
	if _, _, err := ListenAddrs("192.0.2.77"); err == nil {
		t.Fatal("a foreign address was bound")
	}
}
