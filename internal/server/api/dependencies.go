package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
)

// Deps are the collaborators the API needs.
// ImageFetchHooks are the two node-side image operations the API needs: start
// fetching an image onto a node, and stop a transfer that is already running.
type ImageFetchHooks struct {
	Pull   func(ctx context.Context, imageID uint, nodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error)
	Cancel func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
	Remove func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
}

// OutsideSources are what the API reads from outside the installation: the
// community registries the image page lists tags from, and the distribution
// channel the update checks read (ADR 0040). A nil Community uses a shared
// default browser; a nil Channel reads the built-in sources with the built-in
// keys. Which channel is read is the server's setting (ServerSettings).
type OutsideSources struct {
	AllowPrivateHosts []string
	Community         *community.Browser
	Channel           *distribution.Client
}

var (
	defaultCommunityBrowser = community.NewBrowser(nil)
	defaultChannelClient    = &distribution.Client{}
)

func (s OutsideSources) community() *community.Browser {
	if s.Community != nil {
		return s.Community
	}
	return defaultCommunityBrowser
}

func (s OutsideSources) channel() *distribution.Client {
	if s.Channel != nil {
		return s.Channel
	}
	return defaultChannelClient
}

// NodeStreams are the hub's on-demand byte streams from a node's agent. The api
// layer stays unaware of hub and yamux: each method returns the stream (a
// net.Conn) positioned after its header, and the caller closes it.
//
// They share one Deps field rather than one field each — the same grouping
// MonitorService uses — so adding a stream kind costs an interface method
// instead of another entry in the god struct; see scripts/arch-constraints.md's
// api_deps_fields entry.
type NodeStreams interface {
	// OpenLogtail follows a container's logs (ref = instance uuid). ctx bounds
	// a detached data lane's wait for reattachment.
	OpenLogtail(ctx context.Context, nodeUUID, ref string) (net.Conn, error)
	// OpenLargeFileRead reads one bounded file (absolute path on the node). It
	// needs an attached data lane and fails rather than falling back to ctrl
	// or waiting for one to reattach.
	OpenLargeFileRead(ctx context.Context, nodeUUID, path string) (net.Conn, error)
	// UpgradeAgent pushes the agent binary for arch over the node's own
	// connection; pushed=false means the agent cannot take a push and the
	// agent.upgrade RPC (agent pulls /dl/) is used instead.
	UpgradeAgent(nodeUUID, goos, arch string) (pushed bool, err error)
	// DataLaneAttached reports whether a node's data lane is attached now.
	// The storage-files streams need it and never wait for one.
	DataLaneAttached(nodeUUID string) bool
	// UploadStorageFile sends body on a storage.upload stream (hdr names the
	// storage, target, staging, size, extraction) and returns the agent's
	// verdict. The stream is half-closed only when body ends cleanly; a body
	// error or ctx ending resets it. An error is a transport failure.
	UploadStorageFile(ctx context.Context, nodeUUID string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error)
	// OpenStorageDownload opens a storage.download stream and reads the
	// agent's head. A refusal is a head with Code set and a nil body;
	// otherwise the caller reads the bytes and closes the body.
	OpenStorageDownload(ctx context.Context, nodeUUID string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error)
}

type NodeDoctor interface {
	Diagnose(context.Context, string, bool) (diagnostics.DoctorReport, error)
}

type Deps struct {
	NodeDoctor NodeDoctor

	Store         *store.Store
	Bus           *bus.Bus
	Log           *slog.Logger
	SessionCount  func() int       // from hub
	AgentConnect  http.HandlerFunc // hub.ServeWS
	InstallSh     http.HandlerFunc // install script
	RecentRejects func() []dto.AgentReject
	AgentUpgrades AgentUpgradeHooks
	UIHandler     http.Handler // embedded SPA (may be nil in dev)

	// Hub integration.
	Online func(uuid string) bool
	// WakeHeld counts the requests the gateway holds for each deployment's
	// wake right now (the scheduling overview's contention); nil reads none.
	WakeHeld        func() map[uint]int
	KickNode        func(uuid string)
	NodeSync        NodeSync
	CallNode        func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
	SyncModel       func(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error)
	PeerSync        PeerSyncHooks
	CancelModelSync func(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error)
	// Preparation is model preparation as the hub and the controller hold it.
	Preparation deployment.ModelPreparation
	// NodeRuntime reads and restarts a node's container runtime daemon.
	NodeRuntime     NodeRuntimeHooks
	ImageFetch      ImageFetchHooks
	Outside         OutsideSources
	InspectImage    func(ctx context.Context, imageID uint, nodeUUID string) (proto.ImageInspectResult, error)
	ImageExists     func(ctx context.Context, ref, nodeUUID string) (bool, error)
	CatalogPackages *packages.Store
	ModelLogoDir    string
	// ForgetInitialCredentials removes the install-time admin credentials
	// file once the account it records has changed its password.
	ForgetInitialCredentials func(username string) error
	// FetchModelMeta reads a model's metadata from the first of candidates
	// that serves it (modelmeta.FetchFrom); fetchModelMeta gives the
	// candidates.
	FetchModelMeta  func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error)
	ServerDialAgent func(ctx context.Context, addr, secret string) error
	NodeStreams     NodeStreams

	// Web terminal. OpenTerminal mirrors NodeStreams.OpenLogtail: the api layer
	// stays unaware of hub and yamux. Terminal owns admission and audit;
	// NodeCaps lets the handler refuse agents that predate webterm with a clear
	// message.
	OpenTerminal func(nodeUUID, target, ref string, rows, cols uint16, graceSec int) (net.Conn, error)
	NodeCaps     func(nodeUUID string) []string
	Terminal     *webterm.Manager
	ProxyV1      http.Handler // /v1/* data plane: models, the OpenAI-compatible files and batches, and the model requests
	PlaygroundV1 http.Handler // authenticated internal proxy, no platform API key
	// Batches is the Batch API's console side (batches.go); nil answers 404.
	Batches       BatchService
	NodePrecheck  provision.Prechecker
	NodeInstaller provision.Installer
	Recovery      interface {
		Trigger(context.Context, string) (recovery.Outcome, error)
	}

	// Cloud providers.
	Providers *provider.Service
	// Networks resolves node networks and applies network changes; nil reads
	// every declared network from Store.
	Networks *network.Service
	// Server is the machine-level configuration (server.yaml): listen
	// addresses and restart.
	Server ServerSettings

	// Metrics + logs. Decoupled from the tsdb/logstore packages via callbacks.
	MetricsQuery func(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error)
	// MetricsLatest accepts an optional inclusive upper bound, applied before
	// selecting the latest sample for each series.
	MetricsLatest func(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]MetricLatest, error)
	MetricsIngest func(node string, samples []proto.MetricSample)
	LogsSearch    func(filter LogsFilter) (LogsPage, error)
	LogsContext   func(instance, node string, ts, id int64, before, after int) (LogsWindow, error)
	LogsHistogram func(filter LogsFilter, bucketMS int64) ([]LogsBucket, error)
	// Monitor serves /monitor/overview, /monitor/deployments/{id} and
	// /monitor/series. One field of a small interface (rather than three
	// separate callbacks) keeps api_deps_fields at 51 instead of 53; see
	// scripts/arch-constraints.md's api_deps_fields entry for why 51 replaces
	// this struct's previous limit of 50.
	Monitor MonitorService
	// Telemetry is the export of the gateway's request traces to an
	// OpenTelemetry collector (telemetry_export.go); nil answers 404.
	Telemetry TelemetryService

	// UsageReport is the user experience improvement program's daily report
	// (usage_report.go); Notices the console's notice bell. Nil serves an
	// installation that reports nothing and has no notices.
	UsageReport UsageReport
	// Diagnostics runs debug sessions and diagnostics collections; nil
	// answers DIAGNOSTICS_UNAVAILABLE.
	Diagnostics Diagnostics
	Notices     Notices

	// Analytics is usage analytics: the data retention every store is
	// kept under (analytics.go). Nil answers 404.
	Analytics Analytics
	// SchedulingActivity is the scheduling activity's rollups
	// (scheduling_activity.go): the activity route, the sharing stats and
	// the node timelines' counts read it. Nil answers those 404.
	SchedulingActivity SchedulingActivity
	// CardHealth is each card's checks and their failure records
	// (card_health.go). Nil answers those routes 404.
	CardHealth CardHealth
	// Experience is the experience environment beside the server
	// (experience.go). Nil answers 404: the environment's own console, or a
	// harness.
	Experience Experience

	// AI serves /ai/available, /ai/ask and the LLM-free findings and fit
	// reads. Nil means no AI service is wired: available reports false.
	AI AIService
	// CatalogTemplates saves a running deployment as an inference template
	// (deployments_catalog_template.go). Nil answers 404.
	CatalogTemplates CatalogTemplates

	// Management-plane authentication. Data-plane /v1 uses API keys separately.
	AuthSigner           *auth.TokenSigner
	AISecretbox          *auth.Secretbox
	BenchmarkArtifactDir string
}

// PeerSyncHooks groups explicitly sourced model/image operations
// so the API dependency surface remains stable as the peer protocol grows.
type PeerSyncHooks struct {
	RetryImage           func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
	SyncModelWithOptions func(ctx context.Context, modelID uint, nodeUUID, sourceNodeUUID string, rateLimitKiB int) (*store.ModelFile, error)
	ProbeModel           func(ctx context.Context, modelID uint, targetNodeUUID, sourceNodeUUID string) error
	SyncImageWithRate    func(ctx context.Context, imageID uint, nodeUUID, sourceNodeUUID string, rateLimitKiB int64) (*store.ImageNode, error)
	ProbeImage           func(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string) error
	ProbeImageResult     func(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string) (proto.ImageSyncCopyResult, error)
	// CopyModel copies a model's files between two locations (storage, and
	// node for a local storage); see controller.CopyModelFile.
	CopyModel func(ctx context.Context, modelID uint, src, dst store.FileRef, rateLimitKiB int) (*store.ModelFile, error)
}

// NodeRuntimeHooks is the hub's view of a node's container runtime daemon:
// the proxy it uses as the agent last reported it, and a restart so a changed
// proxy takes effect.
type NodeRuntimeHooks interface {
	RuntimeProxy(uuid string) *proto.RuntimeProxyStatus
	RestartRuntime(ctx context.Context, uuid string) (*proto.RuntimeProxyStatus, error)
}
type apiHandlers struct {
	sessionPolicy       auth.SessionPolicy
	loginThrottle       loginThrottle
	d                   Deps
	gpuCacheMu          sync.Mutex
	gpuCacheAt          time.Time
	gpuCache            *gpuInventoryCache
	gpuCacheGeneration  uint64
	overview            overviewCache // the scheduling overview (scheduling_overview.go)
	benchmarkMu         sync.Mutex
	benchmarkExecutions map[uint]*benchmarkExecution
	updates             distribution.CheckCache
	templateUpdate      templateUpdateCheck
	// mux is the router the handlers are mounted on, for the assistant's
	// in-process reads (ai_reader.go).
	mux *chi.Mux
	routesState
	// aiAsks and mcpCalls count each user's questions and MCP tool calls
	// in flight (ai.go, mcp.go).
	aiAsks, mcpCalls inFlightLimiter
}
