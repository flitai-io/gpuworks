package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// A stopped server's listeners close as it exits, so the wait for them is
// short; it only absorbs a supervisor that reports the stop a moment early.
const (
	addrReleaseAttempts = 10
	addrReleaseInterval = 200 * time.Millisecond
)

// checkAddrsReleased fails when an address the installation binds is still
// held after its service was stopped, naming the holder when the host lets it
// be found. command is the subcommand that stopped it ("upgrade", "restart"),
// for the advice on what to run instead.
func (i *Installer) checkAddrsReleased(ctx context.Context, cfg serverconfig.File, command string) error {
	for _, addr := range installedAddrs(cfg) {
		if err := i.waitAddrReleased(ctx, addr, command); err != nil {
			return err
		}
	}
	return nil
}

// installedAddrs is what the installed server binds. A server.yaml whose
// listen section the first start has not filled in yet is served on the
// default port, which loopback reaches whatever the start picks.
func installedAddrs(cfg serverconfig.File) []string {
	if cfg.Listen.Empty() {
		return []string{installedProbeAddr(cfg)}
	}
	var addrs []string
	for _, b := range serverconfig.Bindings(cfg.Listen) {
		addrs = append(addrs, b.Addr)
	}
	return addrs
}

func (i *Installer) waitAddrReleased(ctx context.Context, addr, command string) error {
	for attempt := 0; attempt < addrReleaseAttempts; attempt++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return nil
		}
		// Only "in use" is a holder; anything else (an address this host no
		// longer has) is the server's own startup error to report.
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		i.deps.sleep(ctx, addrReleaseInterval)
	}
	return i.addrHeldError(ctx, addr, command)
}

// addrHeldError explains a held address: which process holds it and, when
// that process is another installation's server, how to reach that one.
func (i *Installer) addrHeldError(ctx context.Context, addr, command string) error {
	msg := fmt.Sprintf("%s is still in use after stopping the service of the installation at %s", addr, i.layout.Dir)
	_, port, _ := serverconfig.SplitAddr(addr)
	if pid := listenerPID(port); pid > 0 {
		argv := cmdline(ctx, i.deps.Run, pid)
		msg += fmt.Sprintf(": pid %d (%s) holds it", pid, strings.Join(argv, " "))
		switch dir, ok := serverDirOf(argv); {
		case ok && dir == i.layout.Dir:
			msg += fmt.Sprintf("; it runs this installation's binary but its service does not supervise it — stop it, then %s again", command)
		case ok:
			msg += fmt.Sprintf("; that is the installation at %s — pass --dir %s to %s it instead", dir, dir, command)
		}
	} else if probe, err := healthURL(addr); err == nil {
		if got, err := i.deps.Health(ctx, probe); err == nil && got != "" {
			msg += fmt.Sprintf(": a gpuworks server reporting version %s answers there", got)
		}
	}
	return errors.New(msg)
}

// serverDirOf reports the installation a gpuworks server process belongs to:
// the directory of the server.yaml it serves, else the installation its binary
// is installed in.
func serverDirOf(argv []string) (string, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != binaryName {
		return "", false
	}
	for idx, arg := range argv[1:] {
		switch {
		case arg == "--config" || arg == "-config":
			if idx+2 < len(argv) {
				return filepath.Dir(absolutePath(argv[idx+2])), true
			}
		case strings.HasPrefix(arg, "--config="), strings.HasPrefix(arg, "-config="):
			_, value, _ := strings.Cut(arg, "=")
			return filepath.Dir(absolutePath(value)), true
		}
	}
	return installationDirOf(argv[0])
}
