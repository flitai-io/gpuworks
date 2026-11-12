package modelsync

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type endpointListener struct {
	net.Listener
	address net.Addr
}

func (l endpointListener) Addr() net.Addr { return l.address }

func TestReceiverLocalCandidatesPreserveListenerAddress(t *testing.T) {
	for _, tc := range []struct {
		ip   net.IP
		zone string
		want []string
	}{
		{net.IPv4zero, "", []string{"127.0.0.1:23001"}},
		{net.IPv6unspecified, "", []string{"[::1]:23001", "127.0.0.1:23001"}},
		{net.ParseIP("192.0.2.1"), "", []string{"192.0.2.1:23001"}},
		{net.ParseIP("fe80::1"), "eth0", []string{"[fe80::1%eth0]:23001"}},
	} {
		address := &net.TCPAddr{IP: tc.ip, Port: 23001, Zone: tc.zone}
		r := &Receiver{listener: endpointListener{address: address}, Endpoints: []string{"untrusted.example.invalid:1"}}
		if got := r.localEndpoints(); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("local candidates=%v want=%v", got, tc.want)
		}
		if !address.IP.Equal(tc.ip) || address.Zone != tc.zone {
			t.Fatal("local candidates mutated listener address")
		}
	}
}

func TestReceiverDialLocalDoesNotUseAdvertisements(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} {
		t.Run(bind, func(t *testing.T) {
			r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: bind, AdvertiseHosts: []string{"untrusted.example.invalid"}, Token: "token", Receive: func(context.Context, io.Reader) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Stop)
			conn, err := r.DialLocal(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			address, ok := conn.RemoteAddr().(*net.TCPAddr)
			listenerAddress := r.listener.Addr().(*net.TCPAddr)
			if !ok || !address.IP.IsLoopback() || address.Port != listenerAddress.Port {
				t.Fatalf("local dial address=%v listener=%v", address, listenerAddress)
			}
			if bind == "127.0.0.1" && address.IP.String() != bind {
				t.Fatalf("specific bind changed: %v", address)
			}
			if bind == "0.0.0.0" && !strings.Contains(r.Endpoints[0], "untrusted.example.invalid") {
				t.Fatal("fixture lacks distinct advertisement")
			}
		})
	}
}

func TestReceiverDialLocalFallsBackToIPv4(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	// This reproduces the ambiguous [::] address of a dual-stack socket when
	// IPv6 loopback is disabled; only the owned port's IPv4 path is reachable.
	r := &Receiver{listener: endpointListener{Listener: listener, address: &net.TCPAddr{IP: net.IPv6unspecified, Port: address.Port}}}
	conn, err := r.DialLocal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().(*net.TCPAddr).IP.To4() == nil {
		t.Fatalf("expected IPv4 fallback, got %v", conn.RemoteAddr())
	}
}

func TestReceiverDialLocalPrefersIPv6ListenerOverOtherIPv4Service(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Skipf("IPv6 listener unavailable: %v", err)
	}
	defer listener.Close()
	ipv6Loopback, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = ipv6Loopback.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	unrelated, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer unrelated.Close()
	r := &Receiver{listener: listener}
	conn, err := r.DialLocal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().(*net.TCPAddr).IP.To4() != nil {
		t.Fatal("dialed unrelated IPv4 service instead of receiver")
	}
}

func TestReceiverDialLocalRejectsUnavailableAndCanceled(t *testing.T) {
	for _, r := range []*Receiver{nil, {}, {listener: endpointListener{address: &net.UnixAddr{Name: "bad", Net: "unix"}}}, {listener: endpointListener{address: &net.TCPAddr{IP: net.IPv4zero}}}, {listener: endpointListener{address: (*net.TCPAddr)(nil)}}} {
		if conn, err := r.DialLocal(context.Background()); conn != nil || err == nil {
			t.Fatalf("invalid receiver conn=%v err=%v", conn, err)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &Receiver{listener: listener}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := r.DialLocal(ctx); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dial conn=%v err=%v", conn, err)
	}
	_ = listener.Close()
	if conn, err := r.DialLocal(context.Background()); conn != nil || err == nil {
		t.Fatalf("closed receiver conn=%v err=%v", conn, err)
	}
}

func TestProbeOnConnLeavesReceiverForActualSend(t *testing.T) {
	var got bytes.Buffer
	r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "token", Magic: "GPUWORKS-IMAGE/1", Receive: func(_ context.Context, in io.Reader) error { _, err := io.Copy(&got, in); return err }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ProbeOnConnWithMagic(ctx, bridgeToReceiver(t, r), "token", r.Fingerprint, "GPUWORKS-IMAGE/1"); err != nil {
		t.Fatal(err)
	}
	if got.Len() != 0 {
		t.Fatal("probe received an archive")
	}
	sent, err := SendOnConnWithMagic(ctx, bridgeToReceiver(t, r), "token", r.Fingerprint, "GPUWORKS-IMAGE/1", func(out io.Writer) error { _, err := io.WriteString(out, "payload"); return err })
	if err != nil {
		t.Fatal(err)
	}
	received, err := r.Wait(ctx)
	if err != nil || sent != received || got.String() != "payload" {
		t.Fatalf("sent=%+v received=%+v payload=%q err=%v", sent, received, got.String(), err)
	}
}

func TestProbeOnConnRejectsInvalidAuthentication(t *testing.T) {
	r, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "token", Receive: func(context.Context, io.Reader) error { t.Error("probe reached receive callback"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	for _, kind := range []string{"token", "fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			token, fp := "token", r.Fingerprint
			if kind == "token" {
				token = "wrong"
			} else {
				fp = strings.Repeat("0", 64)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := ProbeOnConnWithMagic(ctx, bridgeToReceiver(t, r), token, fp, ""); err == nil {
				t.Fatal("bad authentication succeeded")
			}
		})
	}
}

func TestProbeOnConnCancellationClosesRawConnection(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ProbeOnConnWithMagic(ctx, raw, "token", strings.Repeat("0", 64), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancel cause: %v", err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("raw transport remained open: %v", err)
	}
}

func TestProbeOnConnCancellationWhileAwaitingAcknowledgement(t *testing.T) {
	cert, fp, err := ephemeralCertificate()
	if err != nil {
		t.Fatal(err)
	}
	raw, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer peer.Close()
		conn := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		_, err := readLine(bufio.NewReader(conn), 64<<10)
		started <- err
		if err == nil {
			_, _ = conn.Read(make([]byte, 1))
		}
	}()
	done := make(chan error, 1)
	go func() { done <- ProbeOnConnWithMagic(ctx, raw, "token", fp, "") }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe header did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe remained blocked")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("peer did not observe closed transport")
	}
}

func TestProbeOnConnValidationClosesTransport(t *testing.T) {
	for _, kind := range []string{"missing-token", "invalid-fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			raw, peer := net.Pipe()
			defer peer.Close()
			token, fp := "token", strings.Repeat("0", 64)
			if kind == "missing-token" {
				token = ""
			} else {
				fp = "invalid"
			}
			if err := ProbeOnConnWithMagic(context.Background(), raw, token, fp, ""); err == nil {
				t.Fatal("invalid probe succeeded")
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("validation left raw open: %v", err)
			}
		})
	}
	if err := ProbeOnConnWithMagic(context.Background(), nil, "token", strings.Repeat("0", 64), ""); err == nil {
		t.Fatal("nil transport accepted")
	}
}
