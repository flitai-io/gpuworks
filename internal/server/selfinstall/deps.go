package selfinstall

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Deps bundles everything an install or upgrade reaches outside the process
// with: subprocesses, the network, the clock and the terminal. Production code
// uses NewDeps; tests replace fields and assert on what was asked for.
type Deps struct {
	// Run executes commands (with optional privilege escalation).
	Run Runner
	// Client fetches a release site's manifest and archive (--base-url).
	Client *http.Client
	// Channel builds the distribution client for an upgrade from the channel;
	// sources is empty for the built-in ones. Nil is the production client
	// (built-in keys, no overall timeout, stalls cut off).
	Channel func(sources []string) (*distribution.Client, error)
	// Now timestamps backups and audit entries.
	Now func() time.Time
	// Sleep waits between health probes and process-exit polls.
	Sleep func(ctx context.Context, d time.Duration)
	// StartDetached starts a process that must outlive the installer and
	// returns its pid. Production detaches it from the terminal.
	StartDetached func(ctx context.Context, cmd Command) (int, error)
	// Euid reports the effective uid.
	Euid func() int
	// LookupUser reports whether a local account exists.
	LookupUser func(name string) bool
	// LookupAccount resolves a local account's uid and groups; a missing
	// account is user.UnknownUserError.
	LookupAccount func(name string) (Account, error)
	// LookupGroup resolves the explicit credential group's numeric identity.
	LookupGroup func(name string) (*user.Group, error)
	// HasBinary reports whether a command resolves on PATH.
	HasBinary func(name string) bool
	// SystemdRunning reports whether systemd is the init system. A host that
	// merely ships the systemctl binary (a container) must not be mistaken for
	// one that runs it.
	SystemdRunning func() bool
	// LaunchdRunning reports whether launchd is the init system (macOS).
	LaunchdRunning func() bool
	// FreeBytes reports the space available under a path.
	FreeBytes func(path string) (uint64, error)
	// Health probes the started server and returns the version it reports;
	// the default GETs /healthz.
	Health func(ctx context.Context, url string) (string, error)
	// HealthTimeout bounds readiness probing after a start or restart.
	HealthTimeout time.Duration
	// StopTimeout bounds how long a stopped process may take to exit before it
	// is killed.
	StopTimeout time.Duration
	// Host is the machine's addresses, for picking the address a fresh
	// install binds.
	Host serverconfig.Host
}

// NewDeps returns the production dependency set.
func NewDeps(stderr io.Writer) Deps {
	return Deps{
		Run:            ExecRunner{Euid: os.Geteuid, Stderr: stderr},
		Client:         &http.Client{Timeout: 10 * time.Minute},
		Now:            time.Now,
		Sleep:          sleepCtx,
		StartDetached:  startDetached,
		Euid:           os.Geteuid,
		LookupUser:     userExists,
		LookupAccount:  lookupAccount,
		LookupGroup:    user.LookupGroup,
		HasBinary:      hasBinary,
		SystemdRunning: systemdAvailable,
		LaunchdRunning: launchdAvailable,
		FreeBytes:      freeBytes,
		Health:         probeHealth,
		HealthTimeout:  60 * time.Second,
		StopTimeout:    20 * time.Second,
		Host:           serverconfig.SystemHost(),
	}
}

func (d Deps) detectIP() (net.IP, error) {
	return serverconfig.DetectIP(d.Host)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) sleep(ctx context.Context, dur time.Duration) {
	if dur <= 0 {
		return
	}
	if d.Sleep != nil {
		d.Sleep(ctx, dur)
		return
	}
	sleepCtx(ctx, dur)
}

func (d Deps) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

func (d Deps) channelClient(sources []string) (*distribution.Client, error) {
	if d.Channel != nil {
		return d.Channel(sources)
	}
	if len(sources) > 0 {
		normalized, err := distribution.NormalizeSources(sources)
		if err != nil {
			return nil, err
		}
		sources = normalized
	}
	return &distribution.Client{Sources: sources}, nil
}

func (d Deps) healthTimeout() time.Duration {
	if d.HealthTimeout > 0 {
		return d.HealthTimeout
	}
	return 60 * time.Second
}

func (d Deps) hasBinary(name string) bool {
	if d.HasBinary != nil {
		return d.HasBinary(name)
	}
	return hasBinary(name)
}

func (d Deps) systemdRunning() bool {
	if d.SystemdRunning != nil {
		return d.SystemdRunning()
	}
	return systemdAvailable()
}

func (d Deps) launchdRunning() bool {
	if d.LaunchdRunning != nil {
		return d.LaunchdRunning()
	}
	return launchdAvailable()
}

func (d Deps) stopTimeout() time.Duration {
	if d.StopTimeout > 0 {
		return d.StopTimeout
	}
	return 20 * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
