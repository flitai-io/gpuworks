package selfinstall

import (
	"os"
	"strings"
	"time"
)

// A server that nothing restarts cannot upgrade itself from the console: the
// upgrade swaps the binary and exits for a service manager to start the new
// one (ADR 0040). Where there is no systemd (a container, a host installed with
// --no-systemd) `gpuworks serve --supervise` is that manager: a parent process
// that starts `serve` without the flag, starts it again whenever it exits, and
// stops it when the parent is told to stop.
//
// The parent keeps running the binary it was started from across every
// console upgrade, so what it does is kept to that loop and nothing else: it
// reads no configuration and never opens the database, and a release changes
// nothing about the protocol between the two.

const (
	// SupervisedEnv tells the server it was started by the supervisor, which
	// starts it again after it exits; the server may then restart by exiting.
	SupervisedEnv = "GPUWORKS_SUPERVISED"
	// SuperviseFlag is the serve flag that runs the supervisor instead.
	SuperviseFlag = "supervise"
	// superviseRestartDelay matches RestartSec= in the systemd unit, so the
	// start guard counts starts at the same pace under both.
	superviseRestartDelay = 3 * time.Second
)

// SuperviseOptions is one supervisor.
type SuperviseOptions struct {
	// Binary is started on every start. It is the installed path, not the
	// running binary's inode: a console upgrade renames a new file over it.
	Binary string
	// Args are the server's arguments, without the supervise flag.
	Args []string
	// PIDFile, when set, records the supervisor as the installation's
	// process, so `gpuworks upgrade` and `restart` stop this one rather than
	// starting a second server beside it.
	PIDFile string
	// Delay between an exit and the next start; zero means the default.
	Delay time.Duration
	// Logf reports starts and exits; nil discards them.
	Logf func(format string, args ...any)
}

func (o SuperviseOptions) delay() time.Duration {
	if o.Delay > 0 {
		return o.Delay
	}
	return superviseRestartDelay
}

func (o SuperviseOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// WithoutSuperviseFlag returns serve's arguments with the supervise flag
// removed, in any of the spellings the flag package accepts.
func WithoutSuperviseFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			return append(out, args[i:]...)
		}
		name := strings.TrimLeft(arg, "-")
		if strings.HasPrefix(arg, "-") && (name == SuperviseFlag || strings.HasPrefix(name, SuperviseFlag+"=")) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// TakeSupervised reports whether the supervisor started this process, and
// removes the variable so nothing this process starts inherits it.
func TakeSupervised() bool {
	supervised := os.Getenv(SupervisedEnv) == "1"
	_ = os.Unsetenv(SupervisedEnv)
	return supervised
}

// ManagedMode is the service manager that starts this server again after it
// exits, as the install history names it: systemd (which sets INVOCATION_ID
// for a unit's processes) or launchd (whose plist sets LaunchdEnv); "" when
// neither runs it.
func ManagedMode() string {
	switch {
	case os.Getenv("INVOCATION_ID") != "":
		return ModeSystemd
	case os.Getenv(LaunchdEnv) == "1":
		return ModeLaunchd
	}
	return ""
}
