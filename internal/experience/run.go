package experience

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Config is one experience environment.
type Config struct {
	// Listen are the console's addresses; Addr replaces them with one address
	// for everything. TLSCertFile and TLSKeyFile are the server's own
	// certificate, used on the public listeners.
	Listen                  []string
	Addr                    string
	TLSCertFile, TLSKeyFile string
	// DataDir holds everything the environment writes.
	DataDir string
	// PasswordFile holds the console password; changing the file changes
	// the running environment's password.
	PasswordFile string
}

// user is the console's administrator, whom the server's settings card
// names.
const user = experiencelaunch.User

// Run starts the environment and blocks until ctx ends.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.DataDir == "" || cfg.PasswordFile == "" {
		return errors.New("the data directory and the password file are required")
	}
	password, err := readPassword(cfg.PasswordFile)
	if err != nil {
		return err
	}
	shapes, err := catalogShapes()
	if err != nil {
		return fmt.Errorf("read the inference catalog: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	// Storage paths must be canonical (no symbolic links, as /tmp is on macOS).
	if dir, err := filepath.EvalSymlinks(cfg.DataDir); err == nil {
		cfg.DataDir = dir
	}
	if dir, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = dir
	}
	serverDir := filepath.Join(cfg.DataDir, "server")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		return err
	}
	agents, err := agentbin.PrepareForServer(serverDir, log)
	if err != nil {
		return err
	}
	agent, err := extractAgent(cfg.DataDir, agents)
	if err != nil {
		return err
	}
	assets, err := placeholderPartitionAssets(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := serverapp.StartStatisticsAt(filepath.Join(serverDir, "gpuworks.db"), time.Now().Add(-31*24*time.Hour)); err != nil {
		return err
	}
	hubURL, stopHub, err := startHub(shapes)
	if err != nil {
		return err
	}
	defer stopHub()

	secret := randomHex(24)
	g := newGuard(secret)
	app, err := serverapp.New(serverapp.Config{
		Addr: cfg.Addr, Listen: serverconfig.Listen{Console: cfg.Listen},
		TLSCertFile: cfg.TLSCertFile, TLSKeyFile: cfg.TLSKeyFile,
		DataDir: serverDir, AdminUser: user, AdminPassword: password,
		// Nothing leaves the environment: no update checks, no usage report,
		// and model metadata comes from the offline hub.
		ManualUpdateChecks: true, UsageReportOff: true,
		// A day of raw metrics is what the monitoring center shows; the
		// environment keeps no more.
		MetricsRetention: 26 * time.Hour,
		ModelEndpoint:    hubURL, OutboundAllowPrivateHosts: []string{"127.0.0.1"},
		Experience: &serverapp.ExperienceConsole{Guard: g.wrap},
	}, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := app.Close(); err != nil {
			log.Warn("experience server resources close failed", "err", err)
		}
	}()
	// Engines take ports from a block nothing else on this host holds, below
	// the ephemeral range, so no starting instance meets a port another
	// program took.
	// Each node takes an equal slice of the block (simulate.Spec.PortSlot).
	size := len(scenario) * enginePortsPerNode
	start, ok := freePortBlock(size)
	if !ok {
		return fmt.Errorf("no free block of %d ports between 20000 and 30000 for the simulated engines", size)
	}
	app.SetControllerEnginePorts(start, start+size-1)
	ctx, cancel := context.WithCancel(ctx)
	// Everything the environment starts ends before Run returns, so
	// whoever started it may remove its data at once.
	var bg sync.WaitGroup
	spawn := func(f func()) {
		bg.Add(1)
		go func() {
			defer bg.Done()
			f()
		}()
	}
	defer func() {
		cancel()
		bg.Wait()
	}()
	served := make(chan error, 1)
	spawn(func() { served <- app.Run(ctx) })
	base, err := waitBound(ctx, app, served)
	if err != nil {
		return err
	}

	enginesLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	enginesToken := randomHex(24)
	engineHost := newEngineHost(enginesToken, log.With("component", "experience-engines"))
	spawn(func() { engineHost.serve(ctx, enginesLn) })

	members, err := enroll(app, filepath.Join(cfg.DataDir, "nodes.json"))
	if err != nil {
		return err
	}
	f := &fleet{agent: agent, assets: assets, server: base, engines: "http://" + enginesLn.Addr().String(),
		enginesToken: enginesToken, dataDir: cfg.DataDir, log: log.With("component", "experience-node")}
	defer func() {
		cancel()
		f.wait()
	}()
	for slot, n := range scenario {
		if err := f.start(ctx, slot, n, members[n.Hostname]); err != nil {
			return err
		}
	}

	var client *apiClient
	if err := retry(ctx, 30, func() (err error) {
		client, err = login(ctx, base, user, password, secret)
		return err
	}); err != nil {
		return err
	}
	spawn(func() { followPassword(ctx, app, client, cfg.PasswordFile, password, log) })
	spawn(func() { populate(ctx, app, client, g, cfg.DataDir, members, shapes, spawn, log) })
	log.Info("experience environment ready", "console", base, "nodes", len(scenario))
	select {
	case <-ctx.Done():
		return <-served
	case err := <-served:
		cancel()
		return err
	}
}

// populate fills a new environment: the preloaded models, the starter
// deployments, the cloud accounts, the usage history and a finished
// evaluation; then it keeps live traffic going.
func populate(ctx context.Context, app *serverapp.App, c *apiClient, g *guard, dataDir string, members map[string]member, shapes map[string]modelShape, spawn func(func()), log *slog.Logger) {
	ids, err := seedModels(ctx, c, dataDir, members, shapes)
	if err != nil {
		log.Warn("models were not all preloaded", "err", err)
	}
	preloaded, names := map[uint]bool{}, []string{}
	for repo, id := range ids {
		preloaded[id] = true
		names = append(names, repo)
	}
	sort.Strings(names)
	g.allowModels(preloaded, names)
	marker := filepath.Join(dataDir, "seeded")
	if _, err := os.Stat(marker); err != nil {
		if err := createStarters(ctx, c, members); err != nil {
			log.Warn("starter deployments were not all created", "err", err)
		} else {
			_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
		}
	}
	if err := seedClouds(ctx, app, dataDir); err != nil {
		log.Warn("cloud accounts were not added", "err", err)
	}
	keys, err := callerKeys(ctx, c, filepath.Join(dataDir, "caller-keys.json"))
	if err != nil {
		log.Warn("callers' API keys were not created; no traffic or history", "err", err)
		return
	}
	spawn(func() { traffic(ctx, app.LoopbackURL(), c, keys) })
	spawn(func() { seedBenchmark(ctx, c, filepath.Join(dataDir, "seeded-benchmark"), log) })
	spawn(func() { backfillMetrics(ctx, app, filepath.Join(dataDir, "seeded-metrics"), log) })
	seedHistory(ctx, app, c, members, keys, filepath.Join(dataDir, "seeded-history"), log)
}

// seedModels registers each preloaded model under its HuggingFace identity
// (the offline hub answers for its metadata), lays a sparse copy on every
// node the scenario puts it on, and registers that copy, as an operator
// registers a model already on a node. It returns the models' ids.
func seedModels(ctx context.Context, c *apiClient, dataDir string, members map[string]member, shapes map[string]modelShape) (map[string]uint, error) {
	var errs []error
	ids := map[string]uint{}
	var existing struct {
		Items []struct {
			ID       uint   `json:"id"`
			SourceID string `json:"source_id"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/models", &existing); err == nil {
		for _, m := range existing.Items {
			ids[m.SourceID] = m.ID
		}
	}
	for _, n := range scenario {
		for _, repo := range n.Models {
			shape, ok := shapes[strings.ToLower(repo)]
			if !ok {
				errs = append(errs, fmt.Errorf("%s is not in the inference catalog", repo))
				continue
			}
			dir := filepath.Join(dataDir, "nodes", n.Hostname, "models", "local", strings.ReplaceAll(repo, "/", "--"))
			if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
				continue // a restarted environment has its models
			}
			if _, ok := ids[repo]; !ok {
				var out struct {
					ID uint `json:"id"`
				}
				if err := c.post(ctx, "/api/v1/models", map[string]string{"source": "huggingface", "source_id": repo, "name": repo}, &out); err != nil {
					errs = append(errs, fmt.Errorf("register %s: %w", repo, err))
					continue
				}
				ids[repo] = out.ID
			}
			if err := writeCheckpoint(dir, shape); err != nil {
				errs = append(errs, fmt.Errorf("write %s: %w", repo, err))
				continue
			}
			body := map[string]any{"node_uuid": members[n.Hostname].UUID, "path": dir}
			if err := retry(ctx, 60, func() error {
				return c.post(ctx, fmt.Sprintf("/api/v1/models/%d/register-path", ids[repo]), body, nil)
			}); err != nil {
				errs = append(errs, fmt.Errorf("register %s on %s: %w", repo, n.Hostname, err))
			}
		}
	}
	preloaded := map[string]uint{}
	for _, n := range scenario {
		for _, repo := range n.Models {
			if id, ok := ids[repo]; ok {
				preloaded[repo] = id
			}
		}
	}
	return preloaded, errors.Join(errs...)
}

func waitBound(ctx context.Context, app *serverapp.App, served <-chan error) (string, error) {
	base := app.LoopbackURL()
	if base == "" {
		return "", errors.New("the experience console has no loopback address")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			return base, nil
		}
		select {
		case err := <-served:
			return "", fmt.Errorf("the experience console stopped: %w", err)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", errors.New("the experience console did not start listening")
}

// enroll creates the node identities once and keeps them beside the data,
// so a restarted environment reconnects the same nodes.
func enroll(app *serverapp.App, path string) (map[string]member, error) {
	members := map[string]member{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	changed := false
	for _, n := range scenario {
		if _, ok := members[n.Hostname]; ok {
			continue
		}
		node, token, err := app.Store().CreateNodePending(n.Hostname)
		if err != nil {
			return nil, fmt.Errorf("enroll %s: %w", n.Hostname, err)
		}
		members[n.Hostname] = member{UUID: node.UUID, Token: token}
		changed = true
	}
	if changed {
		raw, _ := json.Marshal(members)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// followPassword applies the console password the launcher writes: whenever
// the file changes, the administrator's password becomes its content, which
// also ends the sessions of the old one.
func followPassword(ctx context.Context, app *serverapp.App, c *apiClient, path, current string, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		next, err := readPassword(path)
		if err != nil || next == current {
			continue
		}
		if err := app.Store().SetUserPassword(user, next); err != nil {
			log.Warn("console password not changed", "err", err)
			continue
		}
		current = next
		c.setPassword(next)
		log.Info("console password changed")
	}
}

func readPassword(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(raw))
	if len(password) < 8 {
		return "", fmt.Errorf("%s holds no usable password", path)
	}
	return password, nil
}

// enginePortsPerNode is each simulated node's slice of the engine ports: more
// than its instances ever hold.
const enginePortsPerNode = 100

// freePortBlock finds size consecutive ports no program listens on, below
// the ephemeral ranges (Linux from 32768, macOS from 49152) and above the
// registered services most hosts run. A port counts as free only when both
// the loopback address and every address can be bound: on macOS a loopback
// bind succeeds beside another program's wildcard listener.
func freePortBlock(size int) (int, bool) {
	for start := 20000; start+size <= 30000; start += size {
		free := true
		for port := start; port < start+size && free; port++ {
			for _, host := range []string{"127.0.0.1", ""} {
				ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
				if err != nil {
					free = false
					break
				}
				_ = ln.Close()
			}
		}
		if free {
			return start, true
		}
	}
	return 0, false
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
