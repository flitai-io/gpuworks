package sshutil

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// proxyEnv names the SOCKS5 proxy L4 jobs set when the runner cannot reach the
// cloud's addresses directly (the same variable the in-process test server
// uses for its own SSH installs): socks5://host:port, no authentication.
const proxyEnv = "GPUWORKS_SSH_PROXY"

// dialTCP connects to addr, through the SOCKS5 proxy in GPUWORKS_SSH_PROXY
// when one is set.
func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	proxy := strings.TrimSpace(os.Getenv(proxyEnv))
	if proxy == "" {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	u, err := url.Parse(proxy)
	if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
		return nil, fmt.Errorf("%s=%q is not a socks5://host:port URL", proxyEnv, proxy)
	}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("dial SOCKS5 proxy %s: %w", u.Host, err)
	}
	if err := socks5Connect(ctx, conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// socks5Connect asks an unauthenticated SOCKS5 proxy (RFC 1928) to open addr.
func socks5Connect(ctx context.Context, conn net.Conn, addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port in %q", addr)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	}
	// The caller owns the connection; resetting its deadline is best effort.
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return fmt.Errorf("SOCKS5 greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("SOCKS5 greeting reply: %w", err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fmt.Errorf("SOCKS5 proxy refused unauthenticated use (reply %v)", reply)
	}
	request := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(append(request, 1), v4...)
		} else {
			request = append(append(request, 4), ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("host name too long for SOCKS5")
		}
		request = append(append(request, 3, byte(len(host))), host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("SOCKS5 connect request: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("SOCKS5 connect reply: %w", err)
	}
	if head[0] != 5 || head[1] != 0 {
		return fmt.Errorf("SOCKS5 proxy could not connect to %s (code %d)", addr, head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fmt.Errorf("SOCKS5 connect reply: %w", err)
		}
		skip = int(length[0]) + 2
	default:
		return fmt.Errorf("SOCKS5 reply has unknown address type %d", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		return fmt.Errorf("SOCKS5 connect reply: %w", err)
	}
	return nil
}
