// Command gpuworks-agent runs on each compute node.
//
// Subcommands:
//
//	run       connect to the server and serve
//	configure write the state directory's gpuworks-agent.conf
//	doctor    print an environment self-check
//	grant-device-access  grant a non-root runtime user accelerator device groups
//	runveil   inspect runveil containers: ps, images, exec, logs
//	install-service  register the agent with launchd (macOS)
//	uninstall stop the local service
//	version   print version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/kubeboot"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	recordingruntime "gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	udockerruntime "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	udockernative "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/agent/runveilcli"
	"gpuworks.ai/gpuworks/internal/agent/simulate"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/logging"
	"gpuworks.ai/gpuworks/internal/logthrottle"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/version"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	switch sub {
	case "run":
		// The image of a Kubernetes DaemonSet hands over to the binary on the
		// node's state directory; this returns only when it does not.
		kubeboot.Boot(os.Args[1:])
		if err := run(log, os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("agent exited", "err", err)
			os.Exit(1)
		}
	case "hold":
		// What a kube node's hold pod runs: nothing, for as long as the pod
		// keeps a card borrowed from the kubelet.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		<-ctx.Done()
		stop()
	case "smoke":
		if err := smoke(log, os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("agent smoke failed", "err", err)
			var coded *exitCodeError
			if errors.As(err, &coded) {
				os.Exit(coded.code)
			}
			os.Exit(1)
		}
	case "upgrade":
		if err := upgrade(log, os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Error("agent upgrade failed", "err", err)
			os.Exit(1)
		}
	case "doctor":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := doctor(ctx, log, os.Args[2:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			var coded *exitCodeError
			if errors.As(err, &coded) {
				os.Exit(coded.code)
			}
			os.Exit(1)
		}
	case "grant-device-access":
		if err := grantDeviceAccess(os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "gpuworks-agent grant-device-access: %v\n", err)
			os.Exit(1)
		}
	case "bench":
		if err := bench(os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "gpuworks-agent bench: %v\n", err)
			os.Exit(1)
		}
	case "runveil":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := runveilcli.Main(ctx, os.Args[2:], defaultStateDir())
		stop()
		os.Exit(code)
	case "configure":
		if err := configure(os.Args[2:], os.Stdin, os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "gpuworks-agent configure: %v\n", err)
			os.Exit(1)
		}
	case "install-service":
		if err := installService(context.Background(), os.Args[2:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "gpuworks-agent install-service: %v\n", err)
			os.Exit(1)
		}
	case "uninstall":
		if err := uninstall(os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "gpuworks-agent uninstall: %v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println("gpuworks-agent", version.String())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func grantDeviceAccess(args []string) error {
	fs := flag.NewFlagSet("grant-device-access", flag.ContinueOnError)
	username := fs.String("user", "", "non-root runtime user")
	stateDir := fs.String("state", defaultStateDir(), "agent state directory")
	createUser := fs.Bool("create-user", false, "create the runtime user when it does not exist (requires root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*username) == "" {
		return fmt.Errorf("--user is required")
	}
	if *createUser {
		if os.Geteuid() != 0 {
			return fmt.Errorf("root is required to create runtime user %s", *username)
		}
		if _, err := user.Lookup(*username); err != nil {
			var unknown user.UnknownUserError
			if !errors.As(err, &unknown) {
				return fmt.Errorf("look up runtime user %s: %w", *username, err)
			}
			if out, addErr := exec.Command("useradd", "-m", *username).CombinedOutput(); addErr != nil {
				return fmt.Errorf("create runtime user %s: %w: %s", *username, addErr, strings.TrimSpace(string(out)))
			}
		}
	}
	driver := udockerruntime.New(*stateDir)
	detail, err := agentapp.GrantDeviceAccess(context.Background(), gpu.New(), driver, driver.Name(), *username)
	if err != nil {
		return err
	}
	fmt.Println(detail)
	return nil
}

func run(log *slog.Logger, args []string) error {
	cfg := agentapp.Config{
		DataLane: true,
		DCGM:     envOr("GPUWORKS_DCGM", "off"),
	}
	if fake := os.Getenv("GPUWORKS_FAKE_GPU"); fake != "" {
		p, err := gpu.FromEnv(fake)
		if err != nil {
			return err
		}
		cfg.Prober = p
		log.Warn("fake GPU prober enabled", "spec", fake)
	}
	if vendors := os.Getenv("GPUWORKS_FAKE_GPU_VENDORS"); vendors != "" && cfg.Prober == nil {
		host, err := gpu.FakeHostFromEnv(vendors)
		if err != nil {
			return err
		}
		cfg.AcceleratorHost = host
		log.Warn("fake accelerator host enabled", "vendors", vendors)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	launch := newLaunchFlags(fs)
	fs.BoolVar(&cfg.DataLane, "data-lane", true, "open a dedicated high-throughput data lane (disable for single-connection proxies)")
	fs.StringVar(&cfg.DCGM, "dcgm", cfg.DCGM, "optional DCGM SM-occupancy probe: off (default) | auto (opt in; enables only when dcgmi is on PATH)")
	imagePullMaxConns := fs.Int("image-pull-max-connections", 0, "most simultaneous connections a runveil (udocker) image pull may open to one registry host (0 = automatic, up to "+strconv.Itoa(udockernative.DefaultMaxConnections)+"; 1 = one connection; at most "+strconv.Itoa(udockernative.MaxConnectionsLimit)+")")
	enginePorts := fs.String("engine-ports", os.Getenv("GPUWORKS_ENGINE_PORTS"), "this node's own engine port range START-END (default: the server's)")
	_ = fs.Parse(args)
	if strings.TrimSpace(*enginePorts) != "" {
		r, err := proto.ParsePortRange(*enginePorts)
		if err != nil {
			return fmt.Errorf("--engine-ports: %w", err)
		}
		cfg.EnginePorts = &r
	}
	if *imagePullMaxConns < 0 || *imagePullMaxConns > udockernative.MaxConnectionsLimit {
		return fmt.Errorf("--image-pull-max-connections must be between 0 (automatic) and %d, got %d", udockernative.MaxConnectionsLimit, *imagePullMaxConns)
	}
	if *imagePullMaxConns > 0 {
		// The native image puller reads it where it builds its connection budget.
		_ = os.Setenv(udockernative.MaxConnectionsEnv, strconv.Itoa(*imagePullMaxConns))
	}

	conf, stateDir, _, err := launch.resolve(os.Getenv)
	if err != nil {
		return safeAgentConfigurationError(err)
	}
	if strings.EqualFold(conf.RuntimeDriver, kube.DriverName) {
		stoodBy, err := kubeAcquireTokenOrStandBy(log, &conf, &cfg, stateDir)
		if stoodBy || err != nil {
			return err
		}
	}
	if err := conf.Validate(); err != nil {
		return safeAgentConfigurationError(err)
	}
	cfg.ServerURL, cfg.Token, cfg.StateDir = conf.Server, conf.Token, stateDir
	if strings.EqualFold(conf.RuntimeDriver, kube.DriverName) {
		alignKubeAgent(log, conf.Server, conf.Token)
	}
	cfg.ConnectMode, cfg.ListenAddr, cfg.ForceDriver = conf.Mode(), conf.Listen, conf.RuntimeDriver
	// The runtime reads these two where it uses them; the file is one more
	// place they come from.
	for name, value := range map[string]string{"GPUWORKS_RUNTIME_MOUNT_ROOT": conf.MountRoot, "GPUWORKS_UDOCKER_BUNDLE": conf.UdockerBundle} {
		if value != "" {
			_ = os.Setenv(name, value)
		}
	}
	if record := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_RUNTIME_RECORD")); record != "" {
		driver := recordingruntime.New(record)
		// A process test that has to observe an image arriving needs the node to
		// start out without it; the recording runtime otherwise reports every
		// image as already present.
		for _, ref := range strings.Split(os.Getenv("GPUWORKS_FAKE_RUNTIME_ABSENT_IMAGES"), ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				driver.InjectMissingImage(ref)
			}
		}
		cfg.Driver = driver
		cfg.ForceDriver = "recording"
		log.Warn("recording runtime enabled for process test", "path", record)
	}
	if path := strings.TrimSpace(os.Getenv(simulate.EnvSpec)); path != "" {
		if err := applySimulation(&cfg, path); err != nil {
			return err
		}
		log.Warn("simulated node: cards, runtime and host are simulated", "spec", path)
	}
	fileLog, closeLog, _ := agentLogger(log, cfg.StateDir)
	defer closeLog()
	// The agent's own warn+ records ship to the log center under source=agent
	// while the file handler keeps everything, exactly as before.
	self := selflog.NewCollector(fileLog.Handler(), slog.LevelWarn, 512)
	// Debug sessions capture original records first. Ordinary logs throttle
	// repeating records (a vendor tool failing on every probe) to a few lines
	// and a count.
	log, tee, throttle := debuglog.NewLogger(self, logthrottle.Options{Identity: []string{"code", "reason"}})
	throttleCtx, stopThrottle := context.WithCancel(context.Background())
	defer stopThrottle()
	go throttle.Run(throttleCtx)
	cfg.SelfLog = self
	cfg.DebugLog = tee
	slog.SetDefault(log)

	a := agentapp.New(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("agent starting", "connect_mode", cfg.ConnectMode, "server", cfg.ServerURL, "listen", cfg.ListenAddr, "state", cfg.StateDir)
	return a.Run(ctx)
}

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// errServerPushesUpgrades is why a server_dial agent cannot pull an upgrade:
// it does not know the server, which pushes upgrades to it instead.
var errServerPushesUpgrades = errors.New("the server dials this agent (server_dial) and pushes upgrades to it; upgrade it from the server")

func upgrade(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	launch := newLaunchFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	conf, stateDir, fromFile, err := launch.resolve(os.Getenv)
	if err != nil {
		return err
	}
	if conf.Server == "" && conf.ServerDials() {
		return errServerPushesUpgrades
	}
	if conf.Server == "" || conf.Token == "" {
		return fmt.Errorf("--server and --token are required")
	}
	// The binary may be downloaded through a slow public tunnel; selfUpgrade
	// applies the shared ten-minute operation budget.
	ctx := context.Background()
	return agentconn.UpgradeWithResumeArgs(ctx, conf.Server, conf.Token, resumeArgs(conf, stateDir, fromFile), log)
}

// resumeArgs are the run arguments the upgraded agent restarts with: the
// state directory when the configuration is in its file, else (an agent
// installed before the file) everything as flags.
func resumeArgs(conf agentconf.Config, stateDir string, fromFile bool) []string {
	if fromFile {
		return []string{"--state", stateDir}
	}
	args := []string{"--server", conf.Server, "--token", conf.Token, "--connect-mode", conf.Mode(), "--state", stateDir}
	if conf.ServerDials() {
		args = append(args, "--listen", conf.Listen)
	}
	if conf.RuntimeDriver != "" {
		args = append(args, "--driver", conf.RuntimeDriver)
	}
	return args
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// maxAgentLogBytes caps one agent.log file. The agent keeps a single previous
// generation (agent.log.1); the server keeps many more, both through
// logging.RotatingWriter.
const maxAgentLogBytes int64 = 10 * 1024 * 1024

// agentLogger adds the agent's own log file next to stderr. Losing the file
// (unwritable state dir) degrades to stderr instead of failing startup.
func agentLogger(base *slog.Logger, stateDir string) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		base.Warn("agent log directory unavailable; using stderr", "dir", stateDir, "err", err)
		return base, func() {}, nil
	}
	log, closeLog := logging.NewLogger(base, logging.FileOptions{
		Path:     filepath.Join(stateDir, "agent.log"),
		MaxBytes: maxAgentLogBytes,
		Keep:     1,
		Level:    logLevel(),
		FileOnly: logging.StderrIsAFile(os.Stderr, maxAgentLogBytes),
	})
	return log, closeLog, nil
}

// systemStateDir is the state directory of a root install; tests point it
// elsewhere so the host's own does not decide.
var systemStateDir = "/opt/gpuworks"

func defaultStateDir() string {
	if d := os.Getenv("GPUWORKS_STATE_DIR"); d != "" {
		return d
	}
	// Prefer a system path when writable (root install), else user config.
	if _, err := os.Stat(systemStateDir); err == nil {
		return systemStateDir
	}
	if home, err := os.UserHomeDir(); err == nil && goruntime.GOOS == "darwin" {
		// A Mac install lives in the home. ~/gpuworks counts only with an
		// agent configuration in it: the control plane installs there too.
		if dir := filepath.Join(home, launchd.HomeInstallDir); fileExists(agentconf.Path(dir)) {
			return dir
		}
		if dir := filepath.Join(home, launchd.LegacyHomeInstallDir); dirExists(dir) {
			return dir
		}
	}
	if home, err := os.UserConfigDir(); err == nil {
		return filepath.Join(home, "gpuworks")
	}
	return "./gpuworks-state"
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func usage() {
	fmt.Fprintln(os.Stderr, `gpuworks-agent — compute node agent

usage:
  gpuworks-agent configure --state DIR [--server URL] [--connect-mode agent_dial|server_dial] [--listen ADDR] < token
  gpuworks-agent run [--state DIR]   (flags --server, --token, ... override DIR/gpuworks-agent.conf)
  gpuworks-agent smoke [--state DIR] [--server URL --token TOKEN]
  gpuworks-agent upgrade [--state DIR] [--server URL --token TOKEN]
  gpuworks-agent doctor [--state DIR] [--connectivity [--json]]
  gpuworks-agent grant-device-access --user USER [--create-user] [--state DIR]
  gpuworks-agent runveil [--state DIR] ps|images|exec|logs ...
  gpuworks-agent bench --url http://127.0.0.1:8000 --model MODEL [--concurrency 1] [--isl 128] [--osl 32]
  gpuworks-agent install-service --state DIR [--user USER]   (macOS)
  gpuworks-agent uninstall [--state DIR]
  gpuworks-agent version`)
}

func bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	url := fs.String("url", "", "OpenAI-compatible endpoint base URL")
	apiKey := fs.String("api-key", os.Getenv("OPENAI_API_KEY"), "API key")
	model := fs.String("model", "", "model id")
	concurrency := fs.Int("concurrency", 1, "concurrent workers (used when --concurrency-list is omitted)")
	concurrencyList := fs.String("concurrency-list", "", "grid concurrency list, e.g. 1,2,4")
	isl := fs.Int("isl", 128, "target input length (used when --isl-list is omitted)")
	islList := fs.String("isl-list", "", "grid input length list, e.g. 128,512")
	osl := fs.Int("osl", 32, "target output length (used when --osl-list is omitted)")
	oslList := fs.String("osl-list", "", "grid output length list, e.g. 32,128")
	requests := fs.Int("requests", 0, "closed-loop requests per point (default: concurrency*2)")
	rateList := fs.String("request-rate-list", "", "open-loop arrival rates in req/s, e.g. 1,2,4; replaces the concurrency grid")
	burstiness := fs.Float64("burstiness", benchmeta.DefaultBurstiness, "open-loop gamma shape of inter-arrival gaps (1 = Poisson)")
	maxInFlight := fs.Int("max-in-flight", 0, "open-loop cap on requests in flight (0 = unlimited)")
	pointDuration := fs.Float64("point-duration", benchmeta.DefaultPointDurationSeconds, "open-loop seconds of arrivals per point")
	warmup := fs.Int("warmup-requests", benchmeta.DefaultWarmupRequests, "discarded requests sent before the first point")
	requestTimeout := fs.Int("request-timeout", benchmeta.DefaultRequestTimeoutSeconds, "seconds each request, warmup included, may take before it fails")
	ignoreEOS := fs.Bool("ignore-eos", true, "ask random/agentic requests to generate exactly the target output length")
	kind := fs.String("dataset", "random", "random | multiturn | agentic")
	preset := fs.String("preset", dataset.AgenticDefaultPreset, "agentic input preset")
	turns := fs.Int("turns", dataset.AgenticDefaultTurns, "agentic conversation turns")
	stream := fs.Bool("stream", true, "use streaming responses")
	tokenizerRef := fs.String("tokenizer", "", "tokenizer path, URL, hf:// or ms:// reference")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" || *model == "" {
		return fmt.Errorf("--url and --model are required")
	}
	if *concurrency < 1 {
		return fmt.Errorf("--concurrency must be positive")
	}
	if *requestTimeout < benchmeta.MinRequestTimeoutSeconds || *requestTimeout > benchmeta.MaxRequestTimeoutSeconds {
		return fmt.Errorf("--request-timeout must be between %d and %d seconds", benchmeta.MinRequestTimeoutSeconds, benchmeta.MaxRequestTimeoutSeconds)
	}
	concs, err := parseCSVInts(*concurrencyList, *concurrency)
	if err != nil {
		return fmt.Errorf("concurrency list: %w", err)
	}
	isls, err := parseCSVInts(*islList, *isl)
	if err != nil {
		return fmt.Errorf("isl list: %w", err)
	}
	osls, err := parseCSVInts(*oslList, *osl)
	if err != nil {
		return fmt.Errorf("osl list: %w", err)
	}
	rates, err := parseCSVFloats(*rateList)
	if err != nil {
		return fmt.Errorf("request rate list: %w", err)
	}
	if *kind == "agentic" && strings.TrimSpace(*concurrencyList) == "" {
		concs = []int{1, 2, 4, 8}
	}
	if *kind == "agentic" && *tokenizerRef == "" {
		return fmt.Errorf("--tokenizer is required for agentic workloads")
	}
	rawConcurrency, _ := json.Marshal(concs)
	doc := search.Document{Concurrency: rawConcurrency, ISL: isls, OSL: osls, RequestsPerPointMultiplier: 2, WarmupRequests: *warmup,
		RequestTimeoutSeconds: *requestTimeout, RequestRates: rates, Burstiness: *burstiness, MaxInFlight: *maxInFlight, PointDurationSeconds: *pointDuration}
	if *kind == "agentic" {
		doc.SessionsPerPoint = map[string]int{"1": 4, "2": 8, "4": 8, "8": 16}
	}
	if *requests > 0 {
		doc.SessionsPerPoint = map[string]int{}
		for _, c := range concs {
			doc.SessionsPerPoint[strconv.Itoa(c)] = *requests
		}
	}
	datasetConfig := dataset.Config{Kind: *kind, Seed: time.Now().UnixNano(), IgnoreEOS: ignoreEOS}
	if *kind == "agentic" {
		datasetConfig = dataset.Config{Kind: *kind, Preset: *preset, Turns: *turns, Seed: dataset.AgenticDefaultSeed, IgnoreEOS: ignoreEOS}
		if datasetConfig, err = dataset.ExpandAgenticConfig(datasetConfig); err != nil {
			return err
		}
	}
	rawDataset, _ := json.Marshal(datasetConfig)
	rawSearch, _ := json.Marshal(doc)
	var failure error
	benchrun.Execute(context.Background(), benchrun.Run{
		Model: *model, Mode: "grid", Dataset: rawDataset, Search: rawSearch, TokenizerRef: *tokenizerRef,
		Client: loadgen.Client{BaseURL: *url, APIKey: *apiKey}, NoStream: !*stream,
	}, benchrun.Hooks{
		Warn: func(code string) { fmt.Fprintln(os.Stderr, "warning:", code) },
		Report: func(st proto.BenchmarkStatus) {
			switch {
			case st.State == "failed":
				failure = errors.New(st.Message)
			case len(st.Point) > 0:
				fmt.Println()
				printBenchPoint(st.Point)
			case st.CurrentPoint != nil && st.CurrentPoint.Warmup:
				fmt.Printf("warmup %d requests\n", st.CurrentPoint.RequestTotal)
			case st.CurrentPoint != nil:
				fmt.Printf("\rpoint %d %s ISL%d OSL%d %d/%d", st.CurrentPoint.Seq, benchLoadLabel(st.CurrentPoint.Concurrency, st.CurrentPoint.RequestRate), st.CurrentPoint.TargetISL, st.CurrentPoint.TargetOSL, st.Completed, st.CurrentPoint.RequestTotal)
			}
		},
	})
	return failure
}

func benchLoadLabel(concurrency int, rate float64) string {
	if rate > 0 {
		return fmt.Sprintf("R%g", rate)
	}
	return fmt.Sprintf("C%d", concurrency)
}

func printBenchPoint(raw json.RawMessage) {
	var p proto.BenchmarkPointResult
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	load := fmt.Sprintf("concurrency=%d", p.Concurrency)
	if p.RequestRate > 0 {
		load = fmt.Sprintf("request_rate=%g max_in_flight=%d", p.RequestRate, p.Concurrency)
	}
	fmt.Printf("point=%d %s isl=%d osl=%d requests=%d success=%d errors=%d %s %s\n",
		p.Seq, load, p.TargetISL, p.TargetOSL, p.Success+p.Errors, p.Success, p.Errors, benchPointThroughput(p), benchPointTiming(p))
	printBenchEvidence(p.LongTail)
}

func parseCSVFloats(raw string) ([]float64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []float64
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || !(n > 0) {
			return nil, fmt.Errorf("invalid positive number %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseCSVInts(raw string, fallback int) ([]int, error) {
	if strings.TrimSpace(raw) == "" {
		return []int{fallback}, nil
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid positive integer %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func logLevel() slog.Level {
	switch os.Getenv("GPUWORKS_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
