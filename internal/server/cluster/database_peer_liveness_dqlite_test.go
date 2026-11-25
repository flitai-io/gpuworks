//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"io"
	"net"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// The accepting side of this policy runs in the database process and is
// tested with it. These tests cover the connections this process dials.
func TestMemberDialAppliesPeerSocketPolicy(t *testing.T) {
	member := newTestTransport(t).admit(t, "127.0.0.1")
	for name, config := range map[string]*tls.Config{"plain": nil, "tls": member.dial} {
		t.Run(name, func(t *testing.T) {
			var listener net.Listener
			var err error
			if config == nil {
				listener, err = net.Listen("tcp", "127.0.0.1:0")
			} else {
				listener, err = tls.Listen("tcp", "127.0.0.1:0", member.listen)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					accepted, err := listener.Accept()
					if err != nil {
						return
					}
					go func() {
						defer accepted.Close()
						if secured, ok := accepted.(*tls.Conn); ok {
							_ = secured.Handshake()
						}
						_, _ = io.Copy(io.Discard, accepted)
					}()
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, err := dialMember(ctx, config, listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if secured, ok := conn.(*tls.Conn); ok {
				// The handshake verified the certificate against the dialed
				// host: the fixture's dial configuration names no server.
				conn = secured.NetConn()
			}
			raw, err := conn.(*net.TCPConn).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				name                string
				level, option, want int
			}{
				{"user_timeout_ms", unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 3000},
				{"keepalive_enabled", unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
				{"keepalive_idle_seconds", unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 1},
				{"keepalive_interval_seconds", unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 1},
				{"keepalive_count", unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 3},
			} {
				var value int
				var socketErr error
				err := raw.Control(func(fd uintptr) { value, socketErr = unix.GetsockoptInt(int(fd), check.level, check.option) })
				if err != nil || socketErr != nil {
					t.Fatalf("read %s: control=%v socket=%v", check.name, err, socketErr)
				}
				if value != check.want {
					t.Errorf("dialed connection %s=%d want %d", check.name, value, check.want)
				}
			}
		})
	}
}

func TestMemberDialRefusesAnUnnamedHostAndAnotherAuthority(t *testing.T) {
	member := newTestTransport(t).admit(t, "127.0.0.1")
	if _, err := memberDialServerName("9001"); err == nil {
		t.Fatal("address without a host was named")
	}
	stranger := newTestTransport(t).admit(t, "127.0.0.1")
	listener, err := tls.Listen("tcp", "127.0.0.1:0", stranger.listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			accepted, err := listener.Accept()
			if err != nil {
				return
			}
			_ = accepted.(*tls.Conn).Handshake()
			accepted.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if conn, err := dialMember(ctx, member.dial, listener.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("member of another authority was accepted")
	}
}

func TestPeerSocketPolicyRejectsUnsupportedAndClosedSockets(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if err := configureDatabasePeerSocket(left); err == nil {
		t.Fatal("unsupported connection accepted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	if err := configureDatabasePeerSocket(peer); err == nil {
		t.Fatal("closed socket accepted after option failure")
	}
}

func TestNativeDatabasePeerLivenessDoesNotExpireHealthyConnections(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	serverTLS, clientTLS := databaseTestTLS(t)
	cfg := databaseTLSConfig(t, serverTLS, clientTLS)
	db, err := OpenDatabase(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
		t.Fatal(err)
	}
	native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(client.DialFuncWithTLS(client.DefaultDialFunc, clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := native.OpenConnector(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := connector.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := raw.(*dqlitedriver.Conn)
	defer conn.Close()
	check := func() {
		t.Helper()
		rows, err := conn.QueryContext(ctx, "SELECT 1", nil)
		if err != nil {
			t.Fatal("same native wire failed", err)
		}
		values := make([]driver.Value, 1)
		if err := rows.Next(values); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if values[0] != int64(1) {
			t.Fatalf("native SELECT=%v", values)
		}
	}
	check()
	// Same pinned native connection survives both idle keepalive ACKs and busy
	// SQL beyond the policy timeout. No sql.DB retry can mask a reconnect.
	time.Sleep(6 * time.Second)
	check()
	started := time.Now()
	cycles := 0
	for time.Since(started) < 6*time.Second {
		check()
		cycles++
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("same healthy native connection: idle6s busy%s cycles=%d", time.Since(started), cycles)
}

// At a tier that elects later than slow-storage the limit grows with it, so
// that a connection is not closed at the moment the election timeout runs out.
func TestPeerLivenessGrowsWithTheStorageTier(t *testing.T) {
	at200, err := storagetier.SlowStorage.WithLatency(200 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	previous := processStorageTier.Load()
	t.Cleanup(func() { processStorageTier.Store(previous) })
	UseStorageTier(at200)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := configureDatabasePeerSocket(conn); err != nil {
		t.Fatal(err)
	}
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string][2]int{"user_timeout_ms": {unix.TCP_USER_TIMEOUT, 12000}, "keepalive_count": {unix.TCP_KEEPCNT, 12}} {
		var value int
		var socketErr error
		if err := raw.Control(func(fd uintptr) { value, socketErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, check[0]) }); err != nil || socketErr != nil || value != check[1] {
			t.Errorf("%s = %d (%v, %v), want %d", name, value, err, socketErr, check[1])
		}
	}
}
