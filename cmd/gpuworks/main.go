// Command gpuworks is the control-plane server.
//
// Subcommands:
//
//	serve           run the server
//	install         install the server as a system service
//	upgrade         replace the installed server with the published release
//	restart         restart the installed server so server.yaml takes effect
//	reset-password  change an admin password
//	support-bundle  collect diagnostics without the server running
//	migrate         apply database migrations and exit
//	expose          publish this server through a Cloudflare quick tunnel
//	version         print version
//
// Running the binary without a subcommand prints the usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/debugsrv"
	"gpuworks.ai/gpuworks/internal/logging"
	"gpuworks.ai/gpuworks/internal/logthrottle"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	os.Exit(run(log, os.Args[1:], os.Stderr))
}

// run dispatches one subcommand and returns the process exit code. It is
// separate from main so the dispatch itself is testable: a bare invocation
// prints the usage and fails instead of reaching into arguments that do not
// exist.
func run(log *slog.Logger, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		usageTo(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "serve":
		if err := serve(log, rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			if !errors.Is(err, errServeLogged) {
				log.Error("server exited", "err", err)
			}
			return 1
		}
	case "version", "-v", "--version":
		fmt.Println("gpuworks", version.String())
	case selfinstall.ConsoleUpgradeProtocolCommand:
		// Asked by a server about to install this release from the console.
		fmt.Println(selfinstall.ConsoleUpgradeProtocol)
	case "install":
		if err := installCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("install failed", "err", err)
			return 1
		}
	case "upgrade":
		if err := upgradeCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("upgrade failed", "err", err)
			return 1
		}
	case "uninstall":
		if err := uninstallCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("uninstall failed", "err", err)
			return 1
		}
	case "restart":
		if err := restartCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("restart failed", "err", err)
			return 1
		}
	case "stop":
		if err := stopCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("stop failed", "err", err)
			return 1
		}
	case "support-bundle":
		if err := supportBundle(cmdContext(), log, rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("support bundle failed", "err", err)
			return 1
		}
	case "reset-password":
		if err := resetPassword(log, rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("reset password failed", "err", err)
			return 1
		}
	case "migrate":
		if err := migrate(log, rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("migration failed", "err", err)
			return 1
		}
	case "expose":
		if err := exposeCommand(cmdContext(), rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("expose failed", "err", err)
			return 1
		}
	case experiencelaunch.Command:
		if err := experienceCommand(rest); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("experience environment exited", "err", err)
			return 1
		}
	case "help", "-h", "--help":
		usageTo(stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", sub)
		usageTo(stderr)
		return 2
	}
	return 0
}

// errServeLogged marks a serve failure already written through the server's
// own logger (its file and stderr), so run does not print it a second time.
var errServeLogged = errors.New("server exited")

func serve(log *slog.Logger, args []string) (err error) {
	cfg := app.DefaultConfig()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "server.yaml; flags below override its values")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "serve every function on this one address, replacing listen in server.yaml")
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "sqlite database path")
	fs.StringVar(&cfg.TrustedProxies, "trusted-proxies", cfg.TrustedProxies, "comma-separated proxy IPs/CIDRs allowed to supply X-Forwarded-* headers")
	fs.Int64Var(&cfg.ProxyMaxBodyBytes, "proxy-max-body-bytes", cfg.ProxyMaxBodyBytes, "max /v1 request body spooled to disk before routing (413 beyond)")
	fs.BoolVar(&cfg.RecoveryEnabled, "recovery-enabled", cfg.RecoveryEnabled, "enable SSH out-of-band agent recovery")
	fs.DurationVar(&cfg.RecoveryGracePeriod, "recovery-grace-period", cfg.RecoveryGracePeriod, "wait after disconnect before SSH recovery")
	fs.DurationVar(&cfg.RecoveryInitialInterval, "recovery-initial-interval", cfg.RecoveryInitialInterval, "initial SSH recovery retry interval")
	fs.Float64Var(&cfg.RecoveryBackoffMultiplier, "recovery-backoff-multiplier", cfg.RecoveryBackoffMultiplier, "SSH recovery retry multiplier")
	fs.DurationVar(&cfg.RecoveryMaxInterval, "recovery-max-interval", cfg.RecoveryMaxInterval, "maximum SSH recovery retry interval")
	fs.DurationVar(&cfg.RecoveryRestartWait, "recovery-restart-wait", cfg.RecoveryRestartWait, "wait for agent reconnect after restart or reinstall")
	fs.DurationVar(&cfg.StaleCheckInterval, "stale-check-interval", cfg.StaleCheckInterval, "interval for stale online-node checks")
	fs.DurationVar(&cfg.StaleAfter, "stale-after", cfg.StaleAfter, "heartbeat age after which a node is marked offline")
	fs.DurationVar(&cfg.ServerDialReconnectInterval, "server-dial-reconnect-interval", cfg.ServerDialReconnectInterval, "scan interval for server_dial reconnects")
	fs.DurationVar(&cfg.MetricsRetention, "metrics-retention", cfg.MetricsRetention, "how long raw metrics/log samples are kept before pruning")
	logFile := ""
	fs.StringVar(&logFile, "log-file", logFile, "log file path (default <data>/logs/gpuworks-server.log)")
	logMaxSize := "10MB"
	fs.StringVar(&logMaxSize, "log-max-size", logMaxSize, "rotate the log after this size (KB/MB/GB, 1024-based)")
	logKeep := logging.DefaultRotation().Keep
	fs.IntVar(&logKeep, "log-keep", logKeep, "rotated log files to keep")
	level := "info"
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	supervise := fs.Bool(selfinstall.SuperviseFlag, false, "stay in front of the server and start it again whenever it exits, as systemd does for the unit install writes")
	_ = fs.Parse(args)
	if *supervise {
		return superviseServe(log, *configPath, args)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	legacy := readLegacyEnv()
	var file *serverconfig.File
	if *configPath != "" {
		loaded, err := loadServerConfig(*configPath, given["addr"] || legacy.addr != "", serverconfig.SystemHost())
		if err != nil {
			return err
		}
		applyServerConfig(&cfg, loaded, given)
		file = &loaded
		if !given["log-file"] && loaded.Log.File != "" {
			logFile = loaded.Log.File
		}
		if !given["log-max-size"] && loaded.Log.MaxSize != "" {
			logMaxSize = loaded.Log.MaxSize
		}
		if !given["log-keep"] && loaded.Log.Keep > 0 {
			logKeep = loaded.Log.Keep
		}
		if !given["log-level"] && loaded.Log.Level != "" {
			level = loaded.Log.Level
		}
		if abs, err := filepath.Abs(*configPath); err == nil {
			cfg.ConfigPath = abs
		}
	}
	applyLegacyEnv(&cfg, legacy, given, file)
	// A service manager (systemd, launchd) or serve --supervise starts this
	// server again after it exits, so the console may restart it by exiting.
	supervised := selfinstall.TakeSupervised()
	cfg.SelfRestart = selfinstall.ManagedMode() != "" || supervised
	if logFile == "" {
		// Keep the log beside the data dir it belongs to.
		logFile = defaultLogFile(cfg.DataDir)
	}
	log, closeLog := serverLogger(logFile, logMaxSize, logKeep, parseLogLevel(level))
	defer closeLog()
	// From here on the reason the server stops goes to its log file as well:
	// a detached (nohup) server's stderr is /dev/null, and the file is where
	// install and upgrade send an operator whose server did not come up — a
	// port another process holds being the usual case.
	defer func() {
		if err != nil {
			err = explainFullDisk(err, cfg.DataDir)
			log.Error("server exited", "err", err)
			err = fmt.Errorf("%w: %w", errServeLogged, err)
		}
	}()
	// The server's own warn+ records land in the logstore under source=server
	// (warn, not error: a single process emits few warnings and they are the
	// ones worth searching — retries, reconcile lag, handshake trouble; the
	// per-request access log is info-level and stays out).
	self := selflog.NewCollector(log.Handler(), slog.LevelWarn, 512)
	// Debug sessions capture original records first. Ordinary logs throttle
	// repeating records (an unauthenticated client retrying, a path polled
	// without end) to a few lines and a count.
	log, tee, throttle := debuglog.NewLogger(self, logthrottle.Options{Identity: serverLogIdentity})
	throttleCtx, stopThrottle := context.WithCancel(context.Background())
	defer stopThrottle()
	go throttle.Run(throttleCtx)
	cfg.SelfLog = self
	cfg.DebugLog = tee
	cfg.LogFile = logFile
	// Packages that log through slog's default (the ingest pipelines among
	// them) reach the log file too, rather than bare stderr - which under
	// launchd is a file nothing rotates.
	slog.SetDefault(log)

	// Diagnostics on demand: SIGUSR1 opens a loopback pprof listener for a
	// bounded window (debugsrv.DefaultWindow), then it closes itself.
	// Nothing listens before that.
	pprofSignals := debugsrv.Notify()
	pprofStop := make(chan struct{})
	defer close(pprofStop)
	go debugsrv.Start(log, debugsrv.DefaultWindow, pprofSignals, pprofStop)

	pending, installLayout, err := guardConsoleUpgrade(cfg, log)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if pending != nil {
		go confirmConsoleUpgrade(ctx, log, installLayout, *pending, a.LoopbackURL())
	}
	return a.Run(ctx)
}

// cmdContext is cancelled on the signals that should interrupt a long command
// (an install that is waiting for the service to come up, an upgrade that is
// downloading an archive).
func cmdContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}

func migrate(log *slog.Logger, args []string) error {
	cfg := app.DefaultConfig()
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "sqlite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	policy, err := maintenanceDatabasePolicy(cfg.DBPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath, bus.New(), policy)
	if err != nil {
		return fmt.Errorf("open migration database %s: %w; use the installed gpuworks binary or run it with the service owner's privileges (sudo when required)", cfg.DBPath, err)
	}
	defer st.Close()
	applyEmbedded, build, err := app.EmbeddedCatalogsDue(st)
	if err != nil {
		return err
	}
	if applyEmbedded {
		if err := app.SyncInferenceCatalog(st); err != nil {
			return err
		}
		if err := app.SyncImageCatalog(st, log); err != nil {
			return err
		}
		if err := st.RecordEmbeddedCatalogsApplied(build); err != nil {
			return err
		}
	}
	version, err := st.CurrentMigration()
	if err != nil {
		return err
	}
	log.Info("database migrated", "version", version, "db", cfg.DBPath)
	return nil
}

func resetPassword(log *slog.Logger, args []string) error {
	cfg := app.DefaultConfig()
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var username, password, dataDir string
	fs.StringVar(&dataDir, "data", "", "data directory (default: data_dir of server.yaml next to the binary, then of the default installation, then ./data)")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "sqlite database path")
	fs.StringVar(&username, "user", cfg.AdminUser, "username")
	fs.StringVar(&password, "password", "", "new password (default: prompt without echo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if password == "" {
		var err error
		if password, err = promptNewPassword(); err != nil {
			return err
		}
	}
	if err := auth.ValidateAccountPassword(username, password); err != nil {
		return err
	}
	dbPath, err := resetPasswordDB(dataDir, cfg.DBPath)
	if err != nil {
		return err
	}
	policy, err := maintenanceDatabasePolicy(dbPath)
	if err != nil {
		return err
	}
	// Opening the existing database requires its owner's privileges. Policy
	// validation above happens before touching any database or its permissions.
	st, err := store.Open(dbPath, bus.New(), policy)
	if err != nil {
		return fmt.Errorf("open password database %s: %w; use the installed gpuworks binary or run it with the service owner's privileges (sudo when required)", dbPath, err)
	}
	defer st.Close()
	if err := st.SetUserPassword(username, password); err != nil {
		return err
	}
	log.Info("password reset", "user", username, "db", dbPath)
	// As after a change in the console: the install-time record beside the
	// database now holds a password that no longer works.
	if file, err := store.DatabaseFilePath(dbPath); err == nil && file != "" {
		if err := installcred.ForgetInitialCredentials(filepath.Dir(file), username, policy); err != nil {
			log.Warn("initial admin credentials file not removed after the reset; delete it by hand", "path", installcred.CredentialsPath(filepath.Dir(file)), "err", err)
		}
	}
	return nil
}

// resetPasswordDB is the database reset-password changes: --db when given,
// otherwise gpuworks.db in the data dir resolved the way expose resolves it,
// so `/opt/gpuworks/server/gpuworks reset-password` run from any directory
// finds the installation's database. The database must already exist: SQLite
// would otherwise create an empty one, or fail on a missing directory with a
// misleading "out of memory (14)", and neither is what the operator meant.
func resetPasswordDB(dataDir, dbPath string) (string, error) {
	source := "--db"
	if dbPath == "" {
		var dir string
		dir, source = expose.ResolveDataDir(dataDir)
		dbPath = filepath.Join(dir, "gpuworks.db")
	}
	path, err := store.DatabaseFilePath(dbPath)
	if err != nil {
		return "", fmt.Errorf("identify password database: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf("reset-password requires an existing persistent database; pass --data with the server's data directory or --db with its gpuworks.db")
	}
	if path == dbPath {
		if abs, err := filepath.Abs(path); err == nil {
			dbPath, path = abs, abs
		}
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("database %s (from %s) does not exist; pass --data with the server's data directory or --db with its gpuworks.db", path, source)
		}
		return "", err
	}
	return dbPath, nil
}

// defaultLogFile keeps the server log next to the data dir it belongs to, so
// the default layout is <data>/logs/gpuworks-server.log.
func defaultLogFile(dataDir string) string {
	return filepath.Join(dataDir, "logs", "gpuworks-server.log")
}

// serverLogger adds the rotating file handler to the stderr logger. Every
// failure here is non-fatal: the server must start even when it cannot write
// its log file.
// serverLogIdentity are the record attributes that make two records of one
// message different kinds to the log throttle: a product error's code, a
// refusal's reason, and an access log line's method, path and status.
var serverLogIdentity = []string{"code", "reason", "method", "path", "status"}

func serverLogger(path, maxSize string, keep int, level slog.Level) (*slog.Logger, func()) {
	base := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if strings.TrimSpace(path) == "" {
		return base, func() {}
	}
	maxBytes, err := logging.ParseSize(maxSize)
	if err != nil {
		base.Warn("invalid --log-max-size; falling back to the default", "value", maxSize, "err", err)
		maxBytes = logging.DefaultRotation().MaxBytes
	}
	if keep < 0 {
		keep = 0
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			base.Warn("log directory unavailable; using stderr", "dir", dir, "err", err)
			return base, func() {}
		}
	}
	// Under launchd stderr is a file nothing rotates (the plist's
	// StandardErrorPath); the log file alone then takes the records.
	return logging.NewLogger(base, logging.FileOptions{Path: path, MaxBytes: maxBytes, Keep: keep, Level: level,
		FileOnly: logging.StderrIsAFile(os.Stderr, maxBytes)})
}

// flagProvided reports whether name was set on the command line, so defaults
// that depend on other flags can be re-derived after parsing.
func flagProvided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func usageTo(w io.Writer) {
	fmt.Fprintln(w, `gpuworks — GPU cluster control plane

usage:
  gpuworks serve [--config server.yaml] [--addr :8800] [--data ./data]
                 [--trusted-proxies CIDRS] [--log-file PATH] [--log-max-size 10MB] [--log-keep 20]
                 [--log-level info] [--supervise]
  gpuworks reset-password [--user admin] [--password NEW_PASSWORD] [--data DIR] [--db PATH]
  gpuworks migrate [--data ./data] [--db PATH]
  gpuworks support-bundle [--data DIR] [--since 1h | --from T --to T]
                          [--deployments IDS] [--instances UUIDS] [--out PATH]
  gpuworks version

installation:
`+installUsage+`

public address:
`+exposeUsage+`

serve reads server.yaml (written by install) when --config names it; a flag
given on the command line overrides the file. Without --config it serves every
function on --addr. Configure the server through server.yaml; environment
variables are not a configuration source and are not recommended. The only one
read is GPUWORKS_ADMIN_PASSWORD (the initial admin password, used once on first
start). GPUWORKS_ADDR, GPUWORKS_DATA_DIR and GPUWORKS_TRUSTED_PROXIES are still
honored, only when neither a flag nor server.yaml gives the value, so units
written before server.yaml existed keep working. The log goes to
<data>/logs/gpuworks-server.log (rotated, 10MB x 20) and to stderr.`)
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
