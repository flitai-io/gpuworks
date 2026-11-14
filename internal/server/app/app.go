// Package app is the server assembly root: it wires config, store, bus, hub and
// the HTTP surface, and owns startup/graceful-shutdown.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/offlinepkg"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/version"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/controller"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all" // register drivers
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
	"gpuworks.ai/gpuworks/internal/server/webui"
)

// App holds the assembled server.
type App struct {
	cfg           Config
	log           *slog.Logger
	store         *store.Store
	bus           *bus.Bus
	hub           *hub.Hub
	ctrl          *controller.Controller
	agentUpgrades *agentupgrade.Service
	obs           *observability
	ops           *opsStats // usage analytics' long-term statistics
	recovery      *recovery.Recoverer
	// proxyAcct is proxy request accounting (see accounting.go in the proxy
	// package).
	proxyAcct *proxy.Accountant
	proxy     *proxy.Proxy
	health    *healthcheck.Checker // the deployments' health checks
	cards     *cardHealth          // each card's checks and their failure records
	batches   *batch.Service       // the Batch API's executor and files
	telemetry *telemetryService    // the request trace export
	http      *http.Server
	// ln is created by Run instead of http.Server.ListenAndServe so tests can
	// bind an ephemeral port and discover the actual address through Addr.
	// trackingListener also lets shutdown close hijacked agent websocket
	// connections, which net/http otherwise leaves alive.
	lnMu sync.RWMutex
	lns  []*trackingListener
	// settings serves server.yaml to the API; restart cancels Run so the
	// service manager starts the process again.
	settings       *serverSettings
	restartMu      sync.Mutex
	stopForRestart context.CancelFunc
	// selfLog captures the server's own error-level slog records into the
	// logstore under source=server; nil disables the drain loop.
	selfLog *selflog.Collector
	// usage is the user experience improvement program's daily report;
	// notices the console's notice bell.
	usage   *usageReporter
	notices *noticeService
	// scaleReplicas sets a deployment's replica count as the API does
	// (planning.Scaling, which the handlers share), for the scheduling
	// automation's replica schedules.
	scaleReplicas func(deploymentID uint, role string, replicas int) error
}

// New builds the server from config.
func New(cfg Config, log *slog.Logger) (*App, error) {
	cfg = normalizeRuntimeConfig(cfg)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// Derive the DB path from the (possibly overridden) data dir when unset.
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	// New node enrollment uses per-node tokens stored in the database.

	b := bus.New()
	st, master, err := openCredentialStore(cfg, b, log)
	if err != nil {
		return nil, err
	}
	// The node lists, the dashboard and the monitor share one snapshot of the
	// fleet instead of each reading every node and card per request.
	st.EnableFleetNodeSnapshot(nil)
	embedded, err := startEmbeddedCatalogs(st, log)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	// Bring stored deployments to the current launch-configuration shape before
	// anything reads one. The controller launches instances straight from these
	// columns, so a row still in the old shape would be started from a
	// configuration that is missing values nobody merges in any more.
	if err := normalizeLaunchConfigs(st); err != nil {
		_ = st.Close()
		return nil, err
	}
	catalogPackages, err := packages.New(filepath.Join(cfg.DataDir, "catalog-packages"))
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	// Materialize the exact package embedded in this binary before updating the
	// database projection. A later server version then keeps this directory as
	// long as an older deployment or instance snapshot still references it.
	embeddedCatalog, embeddedDigest, err := seed.Package()
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if _, err := catalogPackages.PutLoaded(embeddedCatalog, embeddedDigest); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := embedded.finish(st); err != nil {
		_ = st.Close()
		return nil, err
	}
	if recovered, err := st.RecoverServerBenchmarkRuns(); err != nil {
		_ = st.Close()
		return nil, err
	} else if recovered > 0 {
		log.Warn("recovered orphaned server benchmarks", "count", recovered)
	}
	// Express the configured model endpoint as site rules, the one place a
	// node's download addresses come from, so node downloads stay hermetic
	// without a second, implicit endpoint setting.
	if strings.TrimSpace(cfg.ModelEndpoint) != "" {
		if err := ensureModelEndpointRules(st, strings.TrimSpace(cfg.ModelEndpoint)); err != nil {
			return nil, err
		}
	}
	// One network service for every component, so they all resolve against
	// the same catalog: every network declared in the store.
	networks := network.New(st, network.StoreCatalog{Store: st})
	readPrivateHosts := privateHostPermissions(cfg, log)
	h, err := newPolicyHub(st, log, cfg.PeerRelay, networks, readPrivateHosts)
	if err != nil {
		return nil, err
	}
	// Besides the listed proxies, the public entry point's connector on this
	// host while `gpuworks expose` has it enabled (ADR 0049).
	proxies, invalidProxies := auth.NewProxyTrust(cfg.TrustedProxies, expose.NewStore(cfg.DataDir).WatchEnabled().Enabled)
	for _, raw := range invalidProxies {
		log.Warn("invalid trusted proxy, ignored", "value", raw)
	}
	h.SetProxyTrust(proxies)
	modelLogoDir := filepath.Join(cfg.DataDir, "model-logos")
	if err := os.MkdirAll(modelLogoDir, 0o755); err != nil {
		return nil, err
	}
	if referenced, referenceErr := st.ReferencedCatalogPackageDigests(); referenceErr != nil {
		log.Warn("list referenced inference catalog packages during startup", "error", referenceErr)
	} else if removed, pruneErr := catalogPackages.Prune(referenced); pruneErr != nil {
		log.Warn("prune inference catalog packages during startup", "error", pruneErr)
	} else if len(removed) > 0 {
		log.Info("pruned unreferenced inference catalog packages during startup", "count", len(removed))
	}
	h.SetRuntimeAssetLoader(func(packageDigest, modelID, file string) ([]byte, error) {
		return catalogPackages.ReadAsset(strings.ToLower(strings.TrimSpace(packageDigest)), modelID, file)
	})
	deploy.SetDefaultImage(deploy.EngineVLLM, cfg.VLLMImage)
	ctrl := controller.New(st, h, log)
	ctrl.SetNetworks(networks)
	h.SetEnginePorts(ctrl.EnginePorts)
	px := proxy.New(st, networks, h, log, cfg.ProxyMaxBodyBytes)
	ctrl.SetDrainProbe(px)

	// Credential encryption for cloud providers (master secret persisted in data dir).
	box, err := auth.NewSecretbox(master)
	if err != nil {
		return nil, err
	}
	st.SetSecretbox(box)
	// The network settings' proxies become proxy entries once the key that
	// seals their passwords is there; until that succeeds they stay put.
	if err := st.CarryOverNetworkProxies(); err != nil {
		log.Warn("carry the network settings' proxies over to the pull settings", "err", err)
	}
	ensurePlatformSSHKey(st, log)
	warnUnmanagedStoragePaths(st, cfg.StorageProbeRoots, log)
	jwtSigner, err := auth.NewTokenSigner(master, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	provSvc := provider.NewService(st, box)
	if err := provider.UseDataDir(cfg.DataDir); err != nil {
		log.Warn("cloud provider state stays in memory", "err", err)
	}

	ctrl.SetInferenceNodeLifecycleHandler(provSvc)
	// Metrics + logs stores, wired to the hub ingest sinks.
	obs, err := openObservability(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	diag := newDiagnostics(cfg, st, h, obs, px, log)
	obs.resolveDeployment = wrapDeploymentLookup(func(instanceUUID string) (uint, error) {
		inst, err := st.InstanceByUUID(instanceUUID)
		if err != nil {
			return 0, err
		}
		return inst.DeploymentID, nil
	}, store.ErrNotFound)
	h.SetSinks(func(instanceUUID string, samples []proto.MetricSample) {
		obs.ingestMetrics(instanceUUID, samples)
		px.ObserveEngineMetrics(samples)
	}, obs.ingestLogs)
	h.SetLogIngestWait(obs.ingestLogsWait)
	h.SetBenchmarkDetailSink(newAgentBenchmarkDetailSink(filepath.Join(cfg.DataDir, "benchmark-artifacts"), st).Handle)

	// Proxy request accounting (plan-9-18-monitor-center.md §4.3): counts
	// completed /v1 requests per deployment+class and flushes to tsdb every
	// 10s through the same ingest path agent-reported metrics use.
	acct := proxy.NewAccountant(proxyMetricsSink(obs), log)
	px.SetAccountant(acct)
	// Every finished /v1 request's record goes to ux.db (ADR 0042), where
	// the request view reads it back with the in-flight table, and to the
	// trace exporter, which samples the traces the gateway starts (ADR 0046).
	telemetry := newTelemetryWithPrivateHosts(st, readPrivateHosts)
	px.SetTraceSampler(telemetry.exporter.Sample)
	ops, err := newOpsStats(cfg.DataDir, cfg.MetricsRetention, st, obs, log)
	if err != nil {
		return nil, err
	}
	px.SetRequestSink(requestSink(obs.ux.Observe, ops.observeRequest, telemetry.exporter))

	ai := newAIService(st, obs, box)
	ai.log = log
	ai.readPrivateHosts = readPrivateHosts
	health := healthcheck.New(healthSource{store: st}, healthSender{proxy: px})
	// The deployment and instance lists, the dashboard, the monitor and the
	// batches' model list share one snapshot of every deployment and instance.
	lists := deployment.NewLists(st, modelPreparation{h, ctrl}, nil)
	batches := batch.New(batchRepo{st}, batchSender{px}, batch.Config{Dir: filepath.Join(cfg.DataDir, "batch-files"), Models: batchModels{st, modelPreparation{h, ctrl}, lists}, Log: log})
	monSvc := newMonitorAPIServiceOn(lists, st, obs.tsdb, nil, newRequestSource(st, px.LiveRequests, obs.ux), health.Health, modelPreparation{h, ctrl})

	a := &App{cfg: cfg, log: log, store: st, bus: b, hub: h, ctrl: ctrl, obs: obs, ops: ops, selfLog: cfg.SelfLog, proxyAcct: acct, proxy: px, health: health, batches: batches, telemetry: telemetry}
	var restart func()
	if cfg.SelfRestart {
		restart = a.requestRestart
	}
	a.settings = newServerSettings(cfg, restart, st.SnapshotTo)
	a.settings.readPrivateHosts = readPrivateHosts
	a.settings.installID = installIDOf(master)
	outside := outsideSources(st, readPrivateHosts)
	a.usage, a.notices = newImprovementProgram(st, obs.ux, a.settings, outside.Channel, log)
	h.SetUsageCounters(a.usage.Counters())
	if a.cards, err = newCardHealth(log, st, h); err != nil {
		return nil, err
	}

	dlDir := filepath.Join(cfg.DataDir, "dl")
	_ = os.MkdirAll(dlDir, 0o755)
	readAgent := func(name string) ([]byte, error) { return agentbin.Read(dlDir, name) }
	provSvc.SetAgentBinaryReader(readAgent)
	h.SetAgentBinaryReader(readAgent)
	a.agentUpgrades, err = newAgentUpgrades(st, h, cfg.DataDir, log)
	if err != nil {
		return nil, err
	}

	sshHostKeys, err := provision.NewFileHostKeyStore(filepath.Join(cfg.DataDir, "ssh-hostkeys.json"))
	if err != nil {
		return nil, err
	}
	nodePrechecker := provision.NewSSHPrechecker()
	nodePrechecker.HostKeys = sshHostKeys
	nodeInstaller := provision.NewSSHInstaller(readAgent)
	nodeInstaller.HostKeys = sshHostKeys
	nodeInstaller.UdockerBundleSource = cfg.UdockerBundleSource
	provision.SetSSHProxy(cfg.SSHProxy)
	provSvc.ConfigureDrivers(nodeInstaller)
	recoverer := recovery.New(st, h, provSvc, nodeInstaller, log, recovery.Config{
		Enabled: cfg.RecoveryEnabled, GracePeriod: cfg.RecoveryGracePeriod,
		InitialInterval: cfg.RecoveryInitialInterval, BackoffMultiplier: cfg.RecoveryBackoffMultiplier,
		MaxInterval: cfg.RecoveryMaxInterval, RestartWait: cfg.RecoveryRestartWait,
	})
	a.recovery = recoverer

	metadataClient := &http.Client{Transport: outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(st) }, readPrivateHosts)}
	apiDeps := api.Deps{
		NodeDoctor: newNodeDoctor(st, h, nodeInstaller),

		Store:         st,
		Bus:           b,
		Log:           log,
		SessionCount:  h.SessionCount,
		AgentConnect:  h.ServeWS,
		InstallSh:     a.installHandler(),
		RecentRejects: func() []dto.AgentReject { return agentRejectDTOs(h.RecentRejects()) },
		AgentUpgrades: api.AgentUpgradeHooks{Service: agentUpgradeAPI{service: a.agentUpgrades}, Errors: h.UpgradeErrors},
		UIHandler:     withDL(dlDir, offlinepkg.NewServer(filepath.Join(cfg.DataDir, "offline"), version.String(), readAgent), webui.Handler(), a.agentUpgrades.DownloadBinary),
		Online:        h.Online,
		KickNode:      h.Kick,
		NodeSync:      h,
		CallNode:      h.CallNode,
		SyncModel:     ctrl.SyncModelToNode,
		PeerSync: api.PeerSyncHooks{
			SyncModelWithOptions: ctrl.SyncModelToNodeWithOptions,
			ProbeModel:           ctrl.ProbeModelSync,
			SyncImageWithRate:    h.SyncImageFromRate,
			ProbeImage:           h.ProbeImagePath,
			ProbeImageResult:     h.ProbeImagePathResult,
			RetryImage:           h.RetryImageSync,
			CopyModel:            ctrl.CopyModelFile,
		},
		CancelModelSync: ctrl.CancelModelSync,
		Preparation:     modelPreparation{h, ctrl},
		Lists:           lists,
		NodeRuntime:     h,
		Outside:         outside,
		ImageFetch: api.ImageFetchHooks{
			Pull:   h.PullImage,
			Cancel: h.CancelImageTransfer,
			Remove: h.RemoveImageFromNode,
		},
		InspectImage:             h.InspectImage,
		ImageExists:              h.ImageExists,
		CatalogPackages:          catalogPackages,
		ModelLogoDir:             modelLogoDir,
		ForgetInitialCredentials: forgetInitialCredentials(cfg),
		FetchModelMeta: func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error) {
			return modelmeta.FetchFrom(ctx, source, repo, revision, subfolder, candidates, metadataClient, planning.ModelLogins(st))
		},
		ServerDialAgent:      h.DialAgentWithSecret,
		NodeStreams:          peerTransferStreams{h},
		OpenTerminal:         h.OpenTerminal,
		NodeCaps:             h.NodeCaps,
		Terminal:             webterm.New(st),
		WakeHeld:             wakeHeldBy(px),
		ProxyV1:              dataPlane(px.ModelsHandler(), batches.V1Handler(batchAuth(px)), px.Handler()),
		Batches:              batchConsole{batches},
		PlaygroundV1:         px.PlaygroundHandler(box),
		NodePrecheck:         nodePrechecker,
		NodeInstaller:        connectionInstaller(nodeInstaller, cfg.DataDir),
		Providers:            provSvc,
		Networks:             networks,
		Server:               a.settings,
		MetricsQuery:         obs.queryMetrics,
		MetricsLatest:        obs.latestMetrics,
		MetricsIngest:        obs.ingestMetrics,
		LogsSearch:           obs.searchLogs,
		LogsContext:          obs.contextLogs,
		LogsHistogram:        obs.histogramLogs,
		Monitor:              monitorService{monSvc},
		Telemetry:            telemetry,
		Analytics:            newAnalyticsAPI(ops, st, obs, cfg.DataDir),
		SchedulingActivity:   ops.sched,
		CardHealth:           a.cards,
		Experience:           experienceLauncher(cfg, log),
		UsageReport:          a.usage,
		Notices:              a.notices,
		AI:                   ai,
		CatalogTemplates:     catalogTemplates{store: st, probe: templateProbe(px)},
		AuthSigner:           jwtSigner,
		AISecretbox:          box,
		BenchmarkArtifactDir: filepath.Join(cfg.DataDir, "benchmark-artifacts"),
		Recovery:             recoverer,
	}
	if diag != nil {
		apiDeps.Diagnostics = diag
	}
	handler := api.Router(apiDeps, auth.SessionPolicy{IdleTimeout: cfg.SessionIdleTimeout, MaxLifetime: cfg.SessionMaxLifetime, Proxies: proxies, Cookie: cfg.sessionCookie()})
	a.scaleReplicas = planning.Scaling{Store: st, Resolve: networks.Resolve, Online: h.Online}.ScaleScheduled
	handler = api.WithSecurityHeaders(proxies, accessLogHandlerFor(log, cfg.DataDir, a.routeByFunction(cfg.experienceGuard(handler))))

	// No read/write timeouts: the agent WS and SSE are long-lived. The WS
	// route bypasses these via hijacking; SSE flushes keepalives.
	a.http = &http.Server{Handler: handler}
	return a, nil
}

// Run starts the HTTP server and background loops, blocking until ctx is done.
func (a *App) Run(ctx context.Context) error {
	tlsConfig, err := a.cfg.listenerTLS()
	if err != nil {
		return err
	}
	// Bind before starting any background loops. This makes a failed bind a
	// synchronous startup error and, for Addr :0, publishes the real port before
	// the first agent is started by an in-process harness.
	lns, err := listenAll(a.cfg.bindings())
	if err != nil {
		return err
	}
	a.lnMu.Lock()
	if a.lns != nil {
		a.lnMu.Unlock()
		closeAll(lns)
		return errors.New("server app already running")
	}
	// net/http marks a Server as permanently shut down after Shutdown. Build a
	// fresh wrapper on every Run so a test harness (and an embeddable caller)
	// can restart the same assembled app on its existing store and handler.
	a.http = &http.Server{Handler: a.http.Handler, ConnContext: connFunctions, TLSConfig: tlsConfig}
	a.lns = lns
	a.lnMu.Unlock()
	ctx, stopForRestart := context.WithCancel(ctx)
	defer stopForRestart()
	a.restartMu.Lock()
	a.stopForRestart = stopForRestart
	a.restartMu.Unlock()
	a.proxy.Start()
	// Deferred first so it runs last: the request store's final flush and
	// the trace exporter's take the records of requests the proxy finished
	// while closing.
	defer a.closeRequestSinks()
	defer a.proxy.Close()
	defer func() {
		closeAll(lns)
		a.lnMu.Lock()
		a.lns = nil
		a.lnMu.Unlock()
	}()

	// bg tracks every loop started below that runs on the shared ctx; see
	// backgroundGroup in shutdown.go (proxyAcct/metricsIngestLoop track their own).
	var bg backgroundGroup
	goBG := bg.start

	// Background: flip stale-online nodes to offline as a safety net.
	goBG(func() { a.staleLoop(ctx) })
	goBG(func() { a.eventRetentionLoop(ctx) }) // Background: bound the deployment event timeline.
	goBG(func() { a.kubeNodeLoop(ctx) })       // Background: remove Kubernetes nodes that left their cluster.
	// Background: reconnect nodes that listen for reverse server_dial sessions.
	goBG(func() { a.hub.RunServerDialReconnect(ctx, a.cfg.ServerDialReconnectInterval) })
	goBG(func() { a.hub.RunNodeQueues(ctx) }) // persist reports queued off the read loops
	// Background: the independent SSH path after ordinary reconnects exhaust.
	goBG(func() { a.recovery.Run(ctx) })
	// The public entry point's connector service (`gpuworks expose`) runs on
	// its own and forwards to what this server really bound.
	if err := expose.RecordServerOrigin(a.cfg.DataDir, exposeOrigin(lns)); err != nil {
		a.log.Warn("cannot record the listening address for `gpuworks expose`", "err", err)
	}
	// Background: reconcile deployments → instances.
	goBG(func() { a.ctrl.Run(ctx) })
	goBG(func() { a.agentUpgrades.Run(ctx) })
	// Background: verify each PD prefill instance's KV handoff once.
	goBG(func() { runPDVerifier(ctx, a.store, a.proxy, a.obs.tsdb) })
	// Background: schedules, waiting wakes, residency.
	waits := a.ctrl.PlacementWaits()
	goBG(func() {
		runAutomation(ctx, a.store, a.proxy, a.proxyAcct, a.obs.hostMemory(), a.scaleReplicas, waits, a.log)
	}) // Background: schedules, waiting wakes, residency.
	goBG(func() { a.health.Run(ctx) }) // Background: the deployments' health checks.
	goBG(func() { a.cards.run(ctx) })  // Background: the cards' failure records.
	// Background: the batches in progress, resumed from their checkpoints.
	if err := a.batches.Start(ctx); err != nil {
		a.log.Error("batch executor not started", "err", err)
	} else {
		goBG(func() { <-ctx.Done(); a.batches.Wait() })
	}
	goBG(func() { a.usage.Run(ctx) })   // Background: the daily usage report.
	goBG(func() { a.notices.Run(ctx) }) // Background: the notices' refresh.
	// Background: prune metrics/logs beyond the retention window
	// (--metrics-retention, default 72h).
	//
	// metricsIngestLoop deliberately runs on its OWN context (metricsCtx),
	// never the shared shutdown ctx: the proxy accountant's final flush (see
	// acctDone below) is only enqueued into the metrics ingest queue partway
	// through the accountant's OWN ctx.Done handling, which takes up to
	// proxy.finalFlushWaitTimeout. If metricsIngestLoop reacted to the same
	// ctx, it would very likely have already drained whatever was queued at
	// that instant and returned - with nothing left running to ever pick up
	// the accountant's later enqueue, silently dropping it. Keeping it on a
	// separate context lets Run stop it explicitly, after stopProxyAccounting
	// confirms the accountant (and therefore its final enqueue) is done - see
	// stopMetricsIngest.
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	// Guarantees metricsCancel is called on every return path (including the
	// unexpected-listen-error path below, which does not itself wait on
	// metricsIngestLoop): the two ordinary shutdown paths already call it
	// explicitly via stopMetricsIngest, and calling a context.CancelFunc more
	// than once is a no-op, so this only matters for the path that doesn't.
	defer metricsCancel()
	metricsDone := make(chan struct{})
	if a.obs != nil {
		goBG(func() { a.ops.run(ctx) })
		goBG(func() { a.ops.backfill(ctx) })
		goBG(func() { a.ops.replayScheduling(ctx) })
		goBG(func() { a.obs.logIngestLoop(ctx) })
		go func() {
			defer close(metricsDone)
			a.obs.metricsIngestLoop(metricsCtx)
		}()
		// Background: the server's own error-level slog records become
		// searchable server-source logs instead of living only in the
		// process's rotated file.
		goBG(func() { a.obs.selfLogLoop(ctx, a.selfLog) })
	} else {
		close(metricsDone)
	}
	// Background: flush proxy request accounting to tsdb every 10s. acctDone
	// closes once Run returns, so shutdown can wait (briefly) for the final
	// flush to be fully enqueued into the metrics ingest queue before this
	// App's caller is free to close the metrics store out from under it; see
	// stopProxyAccounting. That enqueue reaching tsdb itself is a separate
	// guarantee, provided by stopMetricsIngest running only after
	// stopProxyAccounting returns.
	acctDone := make(chan struct{})
	go func() {
		defer close(acctDone)
		a.proxyAcct.Run(ctx)
	}()

	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() {
			a.log.Info("server listening", "addr", ln.Addr().String(), "functions", ln.functions)
			var err error
			if tlsConfig != nil && publicTLSListener(ln) {
				err = a.http.ServeTLS(ln, "", "")
			} else {
				err = a.http.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		a.log.Info("shutting down")
		a.proxy.StopHolding() // held requests: a 503 to retry now, not a drop when the window ends
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.http.Shutdown(shutCtx); err != nil {
			// Long-lived SSE or websocket clients may not close during the
			// graceful window. Force-close those connections so shutdown does not
			// report a false failure to service managers.
			_ = a.http.Close()
			closeConns(lns)
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				a.log.Warn("forced HTTP shutdown", "err", err)
			}
			a.stopProxyAccounting(acctDone)
			a.stopMetricsIngest(metricsCancel, metricsDone)
			// ctx is already Done here, so every loop in bg is already
			// unwinding; wait for all of them before the deferred
			// a.proxy.Close() above runs.
			bg.wait()
			return nil
		}
		// Shutdown does not close hijacked websocket connections. Force-close
		// anything still tracked after the graceful window (and also on a clean
		// shutdown, where a websocket may otherwise keep an agent alive).
		closeConns(lns)
		a.stopProxyAccounting(acctDone)
		a.stopMetricsIngest(metricsCancel, metricsDone)
		bg.wait()
		return nil
	case err := <-errc:
		// A listener's Serve failed on its own: nothing above has cancelled
		// ctx (that otherwise only happens via the caller's ctx or a
		// restart), so without cancelling it here every loop in bg - and the
		// proxy accountant and metrics ingest, which also select on it -
		// would keep running past this return.
		stopForRestart()
		a.stopProxyAccounting(acctDone)
		a.stopMetricsIngest(metricsCancel, metricsDone)
		bg.wait()
		return err
	}
}
