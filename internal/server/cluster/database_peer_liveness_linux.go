//go:build linux && dqlite

package cluster

import (
	"errors"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// Clear blackholed member sessions with headroom inside the initial
// five-second membership grant. Acknowledged healthy connections have no
// absolute age limit. This process applies it to the connections it dials;
// the database process applies the same policy to the ones it accepts.
const databasePeerLivenessTimeout = 3 * time.Second

func configureDatabasePeerSocket(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok || tcp == nil {
		return errors.New("database transport requires a TCP connection")
	}
	// At a tier whose election timeout reaches the limit, a connection
	// would be closed at the very moment the leader steps down for want of
	// answers; the limit grows with the tier so that the election decides
	// first.
	limit := ProcessStorageTier().Stretch(databasePeerLivenessTimeout)
	if err := tcp.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: time.Second, Interval: time.Second, Count: int(limit / time.Second)}); err != nil {
		return err
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	controlErr := raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(limit/time.Millisecond))
	})
	return errors.Join(controlErr, socketErr)
}
