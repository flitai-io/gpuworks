package store

import (
	"encoding/json"
	"time"
)

// Deployment is the desired state of a served model (user-facing config).
type Deployment struct {
	ID                       uint   `gorm:"primaryKey"`
	Name                     string `gorm:"size:128;index"`
	ModelID                  uint   `gorm:"index"`
	TemplateID               uint   `gorm:"index"`
	TemplateSpecSource       string `gorm:"size:16;index"`
	TemplateSpecName         string `gorm:"size:160;index"`
	TemplateScenario         string `gorm:"size:32"`
	TemplateDeviated         bool
	CatalogSelectionSnapshot string `gorm:"column:template_snapshot;type:text"` // JSON deploy.CatalogSelection
	NodeID                   uint   `gorm:"index"`                              // user-selected node (system selects GPUs)
	NodeIDs                  string `gorm:"type:text"`                          // JSON []uint for distributed deployments; read through StoredNodeIDs
	ServedName               string `gorm:"size:128;index"`                     // model name for /v1 routing
	// SaveToStorageID is where a node that has to acquire the model saves it,
	// when that storage applies to the node; 0 follows the default order. It
	// does not limit where the model is read from (store.ModelSourceOn).
	SaveToStorageID uint   `gorm:"column:weight_storage_id;index"`
	Engine          string `gorm:"size:32;index"`  // vllm (default) | sglang
	Image           string `gorm:"size:255;index"` // optional engine image override

	// QuickParams is the deployment's complete launch configuration
	// (deploy.LaunchConfig) as JSON. The column keeps its original name.
	QuickParams string `gorm:"type:text"`
	// ExtraArgs is the user's residual argv: the tokens the parameter dictionary
	// does not describe. Anything it does describe lives in QuickParams. It is
	// argument text, not JSON (deploy.FormatArgsText / deploy.ParseArgsText).
	ExtraArgs          string `gorm:"type:text"`
	Env                string `gorm:"type:text"` // JSON map[string]string
	ContainerOverrides string `gorm:"type:text"` // JSON deploy.ContainerOverrides
	Replicas           int
	RoutingPolicy      string `gorm:"column:routing_policy;size:32"`

	VRAMLimitBytes   int64  // 0 = whole-GPU
	PartitionBackend string `gorm:"size:48"`
	SMLimitPct       int
	Sleepable        bool
	DesiredSleep     int    // 0 awake, 1 L1, 2 L2
	Placement        string `gorm:"type:text"` // JSON planning.Placement
	DistGroup        string `gorm:"size:64;index"`
	DistRole         string `gorm:"size:16"` // leader | follower
	NNodes           int
	NodeRank         int
	MasterAddr       string `gorm:"size:128"`
	MasterPort       int
	// PlacementStrategy orders a deployment's candidate nodes when a replica
	// starts (PlacementSpread or PlacementBinpack).
	PlacementStrategy PlacementStrategy `gorm:"size:16;not null;default:spread"`
	// UnitsAuto says the platform picks the nodes of a multi-node or PD
	// deployment's units whenever a unit starts, preferring the nodes it
	// names; false is the operator's own placement, kept as given. A
	// single-node deployment's automatic placement is the absence of a node
	// requirement instead and does not use it.
	UnitsAuto bool `gorm:"not null;default:false"`
	// StartTimeoutSec is this deployment's health budget for a started
	// instance, in seconds. Zero means it states none and every launch takes
	// the cluster default (DeploymentPolicy.StartTimeoutSec) at that time.
	StartTimeoutSec int `gorm:"not null;default:0"`
	// HealthCheck turns on the server's health check of the deployment: one
	// minimal request through the gateway every minute
	// (internal/server/healthcheck). It only reports.
	HealthCheck bool `gorm:"not null;default:false"`
	// HealthCheckThresholdSec is how long a check waits for its reply before
	// it counts as timed out.
	HealthCheckThresholdSec int `gorm:"not null;default:60"`
	// TokenizeDisabled turns off the gateway's /v1/tokenize and
	// /v1/detokenize for the deployment. It is stored inverted so the zero
	// value (and every row from before the column) means they are open.
	TokenizeDisabled bool `gorm:"not null;default:false"`
	// NodeReplaceAfterSec is how long a node that runs one of the
	// deployment's replicas may stay offline before the replica is moved to
	// another node; 0 never moves it.
	NodeReplaceAfterSec int `gorm:"not null;default:600"`
	// The deployment's yield policy and its parameters (package
	// yieldpolicy, docs/explanation/service-levels.md): YieldPolicy is how
	// it shares GPUs with others (never, idle, timeslice, anytime); the
	// others are normalized on write, so a parameter that does not apply to
	// the policy is its zero value.
	YieldPolicy       string `gorm:"size:16;not null;default:'never'"`
	IdleSec           int    `gorm:"not null;default:0"`
	SleepLevelPref    string `gorm:"size:8;not null;default:''"`
	MaxServeSec       int    `gorm:"not null;default:0"`
	Unfinished        string `gorm:"size:16;not null;default:''"`
	UnfinishedWaitSec int    `gorm:"not null;default:0"`
	HoldMaxSec        int    `gorm:"not null;default:0"`

	// State is the internal Go name; the persisted column is desired_state.
	// The column type is unchanged: DeploymentState is a string alias.
	State DeploymentState `gorm:"column:desired_state;size:16;index"`
	// RestartRequestedAt is when a rolling restart was asked for, nil when none
	// is in progress. A replica still running a generation created before it
	// has yet to be replaced; nothing else records which replicas are done.
	RestartRequestedAt *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// StoredNodeIDs decodes the NodeIDs column as written: (nil, nil) when it is
// empty, an error when it does not decode. It is the one decoder of the column;
// callers that only want "the deployment's nodes" use NodeIDList, callers that
// must tell an unreadable list apart (eviction fails closed, routing refuses)
// use this.
func (d *Deployment) StoredNodeIDs() ([]uint, error) {
	if d.NodeIDs == "" {
		return nil, nil
	}
	var ids []uint
	if err := json.Unmarshal([]byte(d.NodeIDs), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// NodeIDList is the deployment's nodes in their stored order: the NodeIDs
// list, or the primary node when the list is empty or does not decode.
func (d *Deployment) NodeIDList() []uint {
	if ids, err := d.StoredNodeIDs(); err == nil && len(ids) > 0 {
		return ids
	}
	return []uint{d.NodeID}
}

// Instance is one running replica (runtime state).
type Instance struct {
	ID           uint   `gorm:"primaryKey"`
	UUID         string `gorm:"uniqueIndex;size:64"`
	DeploymentID uint   `gorm:"index"`
	NodeID       uint   `gorm:"index"`
	State        string `gorm:"size:16;index"`
	GPUIndexes   string `gorm:"size:64"` // JSON []int, the cards' device indexes as of the last probe
	// GPUIdents is the hardware identity (accelerator.CardIdent) of each card
	// in GPUIndexes, in the same order; JSON []string, empty for instances
	// placed before this column existed or on a node whose prober reports no
	// PCI address or UUID. It is what the binding actually means: an index
	// numbers only the cards the driver could initialize, so it shifts when
	// one fails, is passed through to a VM, or is added, and the instance
	// would silently follow the index onto another card.
	GPUIdents   string `gorm:"size:512;column:gpu_idents"`
	Ports       string `gorm:"size:128"` // JSON []int
	APIPort     int
	ContainerID string `gorm:"size:128"`
	Image       string `gorm:"size:255;index"`
	// ImageDigestID records the immutable runtime image identity observed for
	// this instance. Image remains the requested tag/reference.
	ImageDigestID    string `gorm:"size:128"`
	DesiredStateJSON string `gorm:"column:spec;type:text"` // serialized proto.InstanceDesiredState
	// RuntimeObservation is the agent-redacted, instance-scoped runtime/OCI
	// observation. Its own immutable container/image IDs are generation facts;
	// ContainerID keeps its existing lifecycle/status-report semantics.
	// JSON proto.InstanceRuntimeObservation.
	RuntimeObservation           string `gorm:"type:text"`
	RuntimeObservationReceivedAt *time.Time
	Measured                     string `gorm:"type:text"` // JSON enginelog.Measured, parsed from the engine log
	ScheduleExpl                 string `gorm:"type:text"` // JSON planning.ScheduleRecord
	DirectAddr                   string `gorm:"size:128"`
	// TranscriptionServed records whether the engine's published OpenAPI
	// document exposes /v1/audio/transcriptions. Nil means no document has
	// been read; false includes a document with an empty paths map.
	TranscriptionServed *bool
	Message             string `gorm:"size:512"`
	// RuntimeFailure is JSON proto.RuntimeFailure, an opaque snapshot accompanying
	// Message. The host facts cannot be reconstructed after the agent disconnects.
	RuntimeFailure string `gorm:"type:text"`
	Phase          string `gorm:"size:32"`
	Progress       float64
	BytesDone      int64
	BytesTotal     int64
	RateBPS        int64
	SleepLevel     int
	// SleepResidueBytes is the per-GPU memory the agent measured the engine
	// still holding after its last sleep, 0 when it could not measure (the
	// card was shared, or the agent predates the measurement): the ledger
	// then charges an estimate (sleep.ResidueBytes).
	SleepResidueBytes int64
	// L1HostBufferHeld says the instance's engine completed an L1 sleep since
	// its process started, as its agent reports (proto.InstanceStatus): vLLM
	// keeps the pinned host memory it copies the weights into for the life of
	// the process, so a later L1 sleep takes no host memory the node has not
	// already given it (planning.L1HostMemoryRefusal). It cannot be derived:
	// an instance launched under an L2 order and woken since has none, and an
	// older agent does not report it. A new process clears it.
	L1HostBufferHeld bool `gorm:"not null;default:false"`
	// WakeWaitingSince is when a wake that did not fit started waiting for
	// room; nil when no wake waits. The controller wakes the instance once its
	// GPUs can take it back, or gives up after the wait limit.
	WakeWaitingSince *time.Time `gorm:"index"`
	// SleepHeld says the instance's sleep was ordered by an operator or a
	// schedule (a held sleep): a request through the gateway does not wake
	// it. A held sleep marks exactly the instances it orders, a wake clears
	// exactly the ones it wakes or leaves waiting, and a sleep the platform
	// chose (the residency arbiter's) neither sets nor clears it
	// (Deployment.WakesOnRequest).
	SleepHeld   bool   `gorm:"not null;default:false"`
	DistGroup   string `gorm:"size:64;index"`
	DistRole    string `gorm:"size:16"` // leader | follower
	NNodes      int
	NodeRank    int
	Headless    bool
	DistAttempt int
	// Role is the instance's role in a PD disaggregated deployment
	// (pdmode.Role*); empty for every other deployment. It is independent of
	// DistRole, which ranks a multi-node group.
	Role string `gorm:"size:32;index"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// InstanceEvent is an append-only timeline entry.
//
// It is HISTORY, not current state, and it is deliberately NOT deleted together
// with its deployment or instance: deleting a deployment expresses an intent
// about the live configuration, not an instruction to erase what happened.
// The log store already works this way, and keeping events made the two
// inconsistent — the log of a failure survived while the events explaining it
// did not. Rows are bounded by a retention sweep instead (store.RetentionSweep).
//
// Because the instance row may be gone by the time an event is read, the row
// carries its own identity: InstanceUUID is what the API addresses instances by,
// and DeploymentID is what groups an instance's history plus any scheduling
// entries that were recorded before an instance existed at all.
type InstanceEvent struct {
	ID           uint      `gorm:"primaryKey"`
	InstanceID   uint      `gorm:"index"` // 0 when the event predates any instance (e.g. scheduling)
	InstanceUUID string    `gorm:"size:64;index"`
	DeploymentID uint      `gorm:"index"`
	Kind         string    `gorm:"size:32"` // state | schedule | error | injected
	Code         string    `gorm:"size:64"`
	Message      string    `gorm:"size:1024"`
	Data         string    `gorm:"type:text"` // JSON map[string]any of the code's params for new rows; free text in historical rows
	CreatedAt    time.Time `gorm:"index"`
}
