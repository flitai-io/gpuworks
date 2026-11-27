package app

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ExperienceConsole makes a server the experience environment's console
// (internal/experience) rather than a platform of its own.
type ExperienceConsole struct {
	// Guard wraps every request; it refuses what the environment does not
	// support.
	Guard func(http.Handler) http.Handler
}

// experienceSessionCookie is the experience console's session cookie.
// Cookies are scoped to a host, not a port, so the console beside a server
// names its own and logging into one never logs the other out.
const experienceSessionCookie = "gpuworks-experience-session"

// experiencePortOffset puts the experience console beside the server's own
// console port (8800 -> 8810), where a browser that reached one reaches the
// other.
const experiencePortOffset = 10

func (c Config) sessionCookie() string {
	if c.Experience != nil {
		return experienceSessionCookie
	}
	return ""
}

func (c Config) experienceGuard(next http.Handler) http.Handler {
	if c.Experience == nil || c.Experience.Guard == nil {
		return next
	}
	return c.Experience.Guard(next)
}

// experienceLauncher serves the settings card that turns the experience
// environment on and off, or nil where there is none: inside the
// environment itself, or for an in-process harness.
func experienceLauncher(cfg Config, log *slog.Logger) api.Experience {
	bin, err := os.Executable()
	// The simulated nodes run the agent this build carries for its own
	// platform; a build without one (a development build) has no
	// environment to offer.
	if err != nil || cfg.DataDir == "" || !cfg.agents.Has("gpuworks-agent-"+goruntime.GOOS+"-"+goruntime.GOARCH) {
		return nil
	}
	lc, ok := experienceLaunchConfig(cfg, bin)
	if !ok {
		return nil
	}
	return experiencelaunch.New(lc, log.With("component", "experience"))
}

// experienceLaunchConfig places the environment beside cfg's console: on
// each console address at the port experiencePortOffset above the first
// console port, with the server's certificate. ok is false where there is
// no environment to launch.
func experienceLaunchConfig(cfg Config, bin string) (lc experiencelaunch.Config, ok bool) {
	if cfg.Experience != nil || cfg.DataDir == "" {
		return lc, false
	}
	lc = experiencelaunch.Config{Binary: bin, DataDir: filepath.Join(cfg.DataDir, "experience")}
	for _, b := range cfg.bindings() {
		if !b.Serves(serverconfig.FunctionConsole) {
			continue
		}
		host, port, err := net.SplitHostPort(b.Addr)
		n, convErr := strconv.Atoi(port)
		if err != nil || convErr != nil || n <= 0 || n+experiencePortOffset > 65535 {
			continue
		}
		addr := net.JoinHostPort(host, strconv.Itoa(n+experiencePortOffset))
		if lc.Port == 0 {
			lc.Port = n + experiencePortOffset
		}
		if n+experiencePortOffset == lc.Port {
			lc.Listen = append(lc.Listen, addr)
		}
	}
	if lc.Port == 0 {
		return lc, false
	}
	if cfg.Listen.Empty() {
		// --addr: one address for everything, as the server's own.
		lc.Addr, lc.Listen = lc.Listen[0], nil
	} else {
		lc.TLSCertFile, lc.TLSKeyFile = cfg.TLSCertFile, cfg.TLSKeyFile
	}
	return lc, true
}

// StartStatisticsAt starts a new database's live statistics at since, so
// history seeded with SeedRequestHistory reads as the platform's own record.
// An existing database keeps its state.
func StartStatisticsAt(dbPath string, since time.Time) error {
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	}
	st, err := store.Open(dbPath, bus.New())
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.UsageRollupState(since)
	return err
}

// SeedCloudAccount adds a cloud account and its instances as a sync would
// have listed them, without asking the vendor (provider.Service.Record).
func (a *App) SeedCloudAccount(kind, name string, creds provider.Credentials, instances []provider.Instance) error {
	policy, err := startupCredentialPolicy(a.cfg)
	if err != nil {
		return err
	}
	master, err := masterSecret(a.cfg, policy)
	if err != nil {
		return err
	}
	box, err := auth.NewSecretbox(master)
	if err != nil {
		return err
	}
	_, err = provider.NewService(a.store, box).Record(kind, name, creds, instances)
	return err
}
