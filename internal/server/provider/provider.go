// Package provider is the cloud-provider plugin framework. v1 "takes over"
// existing instances: record credentials → list instances → install the agent
// over the vendor's ssh/exec channel → sync power state. Each vendor implements
// Driver; drivers register into a global registry.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// PowerState of a cloud instance.
type PowerState string

const (
	PowerRunning PowerState = "running"
	PowerStopped PowerState = "stopped"
	PowerUnknown PowerState = "unknown"
)

// ErrUnsupported marks a provider operation that the driver does not implement.
var ErrUnsupported = errors.New("provider operation unsupported")

// Instance is a vendor instance as seen through a provider's API.
type Instance struct {
	ExternalID     string          `json:"external_id"`
	Name           string          `json:"name"`
	GPUModel       string          `json:"gpu_model"`
	GPUCount       int             `json:"gpu_count"`
	Power          PowerState      `json:"power"`
	PricePerHr     float64         `json:"price_per_hr"`
	Region         string          `json:"region"`
	Zone           string          `json:"zone,omitempty"`
	PublicIP       string          `json:"public_ip,omitempty"`
	PrivateIP      string          `json:"private_ip,omitempty"`
	SSHPort        int             `json:"ssh_port,omitempty"`
	SSHUser        string          `json:"ssh_user,omitempty"`
	SSHConnections []SSHConnection `json:"ssh_connections,omitempty"`
	// Runtime hint: "container" (no root/daemon → udocker) or "vm" (docker).
	Kind string `json:"kind,omitempty"`
	// Cluster is the managed Kubernetes cluster (Cluster.ExternalID) the
	// instance is a node of; such an instance joins through the cluster.
	Cluster string `json:"cluster,omitempty"`
}

// Zone describes a provider-supported region/availability zone pair.
type Zone struct {
	Region string `json:"region"`
	Zone   string `json:"zone"`
	IsPod  bool   `json:"is_pod"`
}

// SSHConnection is one provider-supplied path for accessing an instance.
type SSHConnection struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"` // direct | proxy
	Label            localization.Text `json:"label"`
	Host             string            `json:"host"`
	Port             int               `json:"port"`
	User             string            `json:"user"`
	InstallSupported bool              `json:"install_supported"`
}

// Capabilities describes what a provider supports.
type Capabilities struct {
	CanListInstances  bool `json:"can_list_instances"`
	CanPowerControl   bool `json:"can_power_control"`
	CanInstallAgent   bool `json:"can_install_agent"`    // via vendor ssh/exec
	CanCreate         bool `json:"can_create"`           // rent new instance
	CanDelete         bool `json:"can_delete"`           // delete/rent termination
	CanInjectUserData bool `json:"can_inject_user_data"` // provider-native bootstrap data
	HasPricing        bool `json:"has_pricing"`          // instance prices are populated
}

// CredentialField describes one input needed to configure a provider kind.
type CredentialField struct {
	Name         string            `json:"name"`
	Label        localization.Text `json:"label"`
	Secret       bool              `json:"secret"`
	Required     bool              `json:"required"`
	Placeholder  localization.Text `json:"placeholder,omitempty"`
	DefaultValue string            `json:"default_value,omitempty"`
	// DefaultTemplate is an editable creation hint, not a valid credential.
	// It takes precedence over DefaultValue and must be changed before submission.
	DefaultTemplate string `json:"default_template,omitempty"`
	Extra           bool   `json:"extra,omitempty"`
}

// KindInfo is the public schema for one registered provider kind.
type KindInfo struct {
	Kind                    string            `json:"kind"`
	Label                   localization.Text `json:"label"`
	Homepage                string            `json:"homepage,omitempty"` // vendor's public site
	Capabilities            Capabilities      `json:"capabilities"`
	CredentialFields        []CredentialField `json:"credential_fields"`
	DefaultServerDialListen string            `json:"default_server_dial_listen,omitempty"`
	ServerDialSupported     bool              `json:"server_dial_supported"`
	// ServerDialAutoPort: the driver chooses a server-dialed agent's port and
	// address from the provider's port mappings (ServerDialPortMapper), so
	// the operator may leave both empty.
	ServerDialAutoPort bool `json:"server_dial_auto_port,omitempty"`
	// CanListClusters: the driver lists managed Kubernetes clusters
	// (ClusterProvider).
	CanListClusters bool `json:"can_list_clusters"`
}

// CreateInstanceRequest carries generic create/rent parameters for future
// provider implementations. Drivers may use Extra for provider-native fields.
type CreateInstanceRequest struct {
	Name     string            `json:"name,omitempty"`
	Region   string            `json:"region,omitempty"`
	GPUModel string            `json:"gpu_model,omitempty"`
	GPUCount int               `json:"gpu_count,omitempty"`
	Image    string            `json:"image,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// ResizeInstanceRequest carries generic resize parameters for future drivers.
type ResizeInstanceRequest struct {
	GPUModel string            `json:"gpu_model,omitempty"`
	GPUCount int               `json:"gpu_count,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// AgentBinaryReader loads the cross-compiled agent artifact requested by an
// SSH installer after it detects the target architecture.
type AgentBinaryReader func(name string) ([]byte, error)

// InstallRequest carries what the agent installer needs.
type InstallRequest struct {
	Instance          Instance
	InstanceKind      string // container or vm runtime hint
	ServerURL         string // gpuworks server base URL the agent should dial
	Token             string // per-node connection token
	SSHKeyPEM         string // optional private key the operator gave
	Password          string // optional ssh password the operator gave
	PlatformKey       *PlatformKey
	InstancePassword  string // root password the provider's API issued, if any
	ConnectMode       string // agent_dial | server_dial
	ServerDialListen  string // agent listen address when ConnectMode=server_dial
	ServerDialAddress string // control-plane address used to reach server_dial agent
	// AllowInstanceRestart: the operator agreed that opening a port for a
	// server-dialed agent may restart the instance (ErrInstanceRestartRequired).
	AllowInstanceRestart bool
	// InstallDir is the agent's install directory on the instance, "" for
	// the default (/opt/gpuworks).
	InstallDir string
	// NoSystemd runs the agent under nohup instead of the gpuworks-agent
	// systemd unit.
	NoSystemd       bool
	ReadAgentBinary AgentBinaryReader
	// Connection is the path chosen for this attempt: an SSH endpoint (also
	// applied to Instance) or a provider command channel such as
	// ConnectionCloudAssistant.
	Connection SSHConnection
}

// ScriptOptions are the install.sh arguments for the operator's choices
// about where and how the agent runs on the instance.
func (r InstallRequest) ScriptOptions() provision.ScriptOptions {
	return provision.ScriptOptions{InstallDir: r.InstallDir, NoSystemd: r.NoSystemd}
}

// InstallOptions are management-plane choices for provider agent installation.
type InstallOptions struct {
	SSHKeyPEM        string
	Password         string
	SSHConnectionID  string
	ConnectMode      string
	ServerDialListen string
	// HostAddress is where the server reaches the instance for server_dial;
	// default its private, else public, IP.
	HostAddress       string
	ServerDialAddress string
	// AllowInstanceRestart: see InstallRequest.AllowInstanceRestart.
	AllowInstanceRestart bool
	// InstallDir and NoSystemd: see InstallRequest.
	InstallDir string
	NoSystemd  bool
}

// InstallResult returns the generated bootstrap material and connection hints.
type InstallResult struct {
	Token             string
	NodeUUID          string
	Instance          Instance
	ConnectMode       string
	ServerDialListen  string
	ServerDialAddress string
	InstallChecks     []InstallCheck
	SSHConnection     *SSHConnection
	SSHAttempts       []SSHAttempt
}

// SSHAttempt is a sanitized account of an endpoint considered by auto selection.
type SSHAttempt struct {
	ConnectionID string `json:"connection_id"`
	Target       string `json:"target"`
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
}

// SSHConnectionError identifies failures that occur before a remote install
// command starts and are therefore safe for automatic endpoint fallback.
type SSHConnectionError struct {
	Stage string
	Err   error
}

func (e *SSHConnectionError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *SSHConnectionError) Unwrap() error { return e.Err }

func RetryableSSHConnectionError(stage string, err error) error {
	return &SSHConnectionError{Stage: stage, Err: err}
}

func IsRetryableSSHConnectionError(err error) bool {
	var target *SSHConnectionError
	return errors.As(err, &target)
}

// InstallCheck is a provider-install observation surfaced by the API. It is
// intentionally provider-package-local so drivers can report SSH installer
// details without coupling the driver interface to the provision package.
type InstallCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ReportingDriver is an optional extension for drivers that can return
// detailed installation checks. The base Driver interface remains unchanged
// for provider-native transports and test drivers.
type ReportingDriver interface {
	InstallAgentWithReport(ctx context.Context, creds Credentials, req InstallRequest) (InstallReport, error)
}

// InstanceGetter is an optional provider capability for refreshing one
// instance from a detail endpoint. Some cloud APIs expose runtime connection
// data in detail responses before their list endpoint catches up.
// CommandPrechecker is an optional driver capability: run the install
// precheck through a provider command channel (ConnectionCloudAssistant)
// instead of SSH. The checks use the provision check names.
type CommandPrechecker interface {
	PrecheckCommand(ctx context.Context, creds Credentials, inst Instance, connection SSHConnection, req CommandPrecheckRequest) ([]InstallCheck, error)
}

// CommandPrecheckRequest is what a command-channel precheck needs to know.
type CommandPrecheckRequest struct {
	ServerURL   string
	ConnectMode string
}

type InstanceGetter interface {
	GetInstance(ctx context.Context, creds Credentials, externalID string) (Instance, error)
}

type InstallReport struct {
	Checks []InstallCheck `json:"checks"`
	// AuthUsed is the provision.Auth* credential the instance accepted, ""
	// when the driver does not report it.
	AuthUsed string `json:"auth_used,omitempty"`
	// PlatformKeyAuthorized: the instance now accepts the platform key.
	PlatformKeyAuthorized bool `json:"platform_key_authorized,omitempty"`
	// PlatformKeyVolatile: the platform key was authorized only where the
	// instance loses it when it restarts (a RunPod pod re-reads its keys from
	// the PUBLIC_KEY it was created with and starts from a clean container
	// disk), so the credential that logged in stays on the node: recovery
	// has nothing else to log in with after such a restart.
	PlatformKeyVolatile bool `json:"-"`
	// InstallDir is where the install put the agent, "" when the driver
	// used the install script's default.
	InstallDir string `json:"-"`
	// ServerDialListen and ServerDialAddress are where a ServerDialPortMapper
	// put a server-dialed agent: the container port it listens on and the
	// public address the provider maps to it.
	ServerDialListen  string `json:"-"`
	ServerDialAddress string `json:"-"`
	// SSHHost and SSHPort are the SSH endpoint the install logged in to when
	// the driver changed it (a RunPod pod edit renumbers SSH): the node's
	// credential must point there, not at the endpoint the install began at.
	SSHHost string `json:"-"`
	SSHPort int    `json:"-"`
}

// Driver is one cloud vendor's integration.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	// ValidateCredentials checks that creds work (cheap API call).
	ValidateCredentials(ctx context.Context, creds Credentials) error
	// ListInstances returns the account's instances.
	ListInstances(ctx context.Context, creds Credentials) ([]Instance, error)
	// PowerState returns the current power state of one instance.
	PowerState(ctx context.Context, creds Credentials, externalID string) (PowerState, error)
	PowerOn(ctx context.Context, creds Credentials, externalID string) error
	PowerOff(ctx context.Context, creds Credentials, externalID string) error
	CreateInstance(ctx context.Context, creds Credentials, req CreateInstanceRequest) (Instance, error)
	DeleteInstance(ctx context.Context, creds Credentials, externalID string) error
	ResizeInstance(ctx context.Context, creds Credentials, externalID string, req ResizeInstanceRequest) error
	// InstallAgent injects and starts the agent on an instance (v1: via ssh/exec).
	InstallAgent(ctx context.Context, creds Credentials, req InstallRequest) error
}

// Credentials is a vendor-agnostic credential bag (fields used per driver).
type Credentials struct {
	Endpoint   string            `json:"endpoint,omitempty"`
	APIKey     string            `json:"api_key,omitempty"`
	PublicKey  string            `json:"public_key,omitempty"`  // Public key or access key ID.
	PrivateKey string            `json:"private_key,omitempty"` // Private key or access key secret.
	Extra      map[string]string `json:"extra,omitempty"`
}

// EndpointOr returns the configured endpoint override, or the driver's default.
func (c Credentials) EndpointOr(defaultEndpoint string) string {
	if endpoint := strings.TrimRight(strings.TrimSpace(c.Endpoint), "/"); endpoint != "" {
		return endpoint
	}
	return strings.TrimRight(defaultEndpoint, "/")
}

// registry of drivers.
var (
	regMu    sync.RWMutex
	registry = map[string]Driver{}
)

// Register adds a driver (called from driver package init()).
func Register(d Driver) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[d.Name()] = d
}

// DataDirUser is a driver that keeps state, such as trusted SSH host keys, in
// the server's data dir.
type DataDirUser interface {
	UseDataDir(dir string) error
}

// UseDataDir hands the data dir to every registered driver that keeps state.
func UseDataDir(dir string) error {
	regMu.RLock()
	defer regMu.RUnlock()
	for name, d := range registry {
		if user, ok := d.(DataDirUser); ok {
			if err := user.UseDataDir(dir); err != nil {
				return fmt.Errorf("provider %s: %w", name, err)
			}
		}
	}
	return nil
}

// Get returns a registered driver by name.
func Get(name string) (Driver, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// Names lists registered driver names, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// CredentialSchemaProvider lets a driver own its credential input contract.
type CredentialSchemaProvider interface {
	CredentialFields() []CredentialField
}

func driverCredentialFields(driver Driver) []CredentialField {
	if schema, ok := driver.(CredentialSchemaProvider); ok {
		return schema.CredentialFields()
	}
	return credentialFieldsFor(driver.Name())
}

// KindInfos lists registered drivers with UI/API credential schema.
func KindInfos() []KindInfo {
	regMu.RLock()
	defer regMu.RUnlock()
	infos := make([]KindInfo, 0, len(registry))
	for kind, drv := range registry {
		infos = append(infos, KindInfo{
			Kind:                    kind,
			Label:                   labelFor(kind),
			Homepage:                kindHomepages[kind],
			Capabilities:            drv.Capabilities(),
			CredentialFields:        driverCredentialFields(drv),
			DefaultServerDialListen: defaultServerDialListenFor(kind),
			ServerDialSupported:     ServerDialSupported(kind),
			ServerDialAutoPort:      isPortMapper(drv),
			CanListClusters:         isClusterProvider(drv),
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Kind < infos[j].Kind })
	return infos
}

// Category groups provider kinds by the platform they connect.
type Category string

const (
	CategoryPublicCloud  Category = "public_cloud"
	CategoryPrivateCloud Category = "private_cloud"
)

// CategoryOf reports the platform a kind connects. Each known kind declares
// its category (kindCategories); a kind that declares none has no category.
func CategoryOf(kind string) Category { return kindCategories[kind] }

func isPortMapper(drv Driver) bool {
	_, ok := drv.(ServerDialPortMapper)
	return ok
}

// ServerDialSupported reports whether the server can dial agents on this
// provider's instances. AutoDL Pro instances open HTTP ports only, and a
// server-dialed agent needs a TCP one.
func ServerDialSupported(kind string) bool { return kind != "autodl" }

func defaultServerDialListenFor(kind string) string {
	if kind == "compshare" {
		return ":3389"
	}
	return ""
}

func credentialFieldsFor(kind string) []CredentialField {
	apiKey := CredentialField{
		Name: "api_key", Secret: true, Required: true,
	}
	localizeCredentialField(&apiKey, "api_key")
	endpoint := CredentialField{
		Name: "endpoint", Required: false, Placeholder: localization.SameText(endpointPlaceholder(kind), requiredProviderLocales...),
		DefaultValue: defaultEndpoint(kind),
	}
	localizeCredentialField(&endpoint, "endpoint")
	switch kind {
	case "compshare":
		return []CredentialField{
			credentialField("public_key", CredentialField{Name: "public_key", Required: true}),
			credentialField("private_key", CredentialField{Name: "private_key", Secret: true, Required: true}),
			endpoint,
		}
	case "aliyun":
		// An AccessKey alone signs every call; with a RAM role ARN the server
		// assumes that role and signs with its temporary credentials.
		return []CredentialField{
			credentialField("access_key_id", CredentialField{Name: "public_key", Required: true}),
			credentialField("access_key_secret", CredentialField{Name: "private_key", Secret: true, Required: true}),
			credentialField("role_arn", CredentialField{Name: "role_arn", Required: false, Extra: true}),
			credentialField("regions", CredentialField{Name: "regions", Required: false, Extra: true}),
		}
	case "autodl":
		return []CredentialField{
			apiKey,
			endpoint,
			credentialField("ssh_password", CredentialField{Name: "ssh_password", Secret: true, Required: false, Extra: true}),
		}
	default:
		return []CredentialField{apiKey, endpoint}
	}
}

// API endpoints are shared with the drivers so form defaults match runtime defaults.
const (
	AutoDLEndpoint       = "https://api.autodl.com"
	CompShareEndpoint    = "https://api.compshare.cn"
	DigitalOceanEndpoint = "https://api.digitalocean.com"
	RunPodEndpoint       = "https://api.runpod.io/v2"
	VastEndpoint         = "https://console.vast.ai/api/v0"
)

func defaultEndpoint(kind string) string {
	switch kind {
	case "autodl":
		return AutoDLEndpoint
	case "compshare":
		return CompShareEndpoint
	case "digitalocean":
		return DigitalOceanEndpoint
	case "runpod":
		return RunPodEndpoint
	case "vast":
		return VastEndpoint
	default:
		return ""
	}
}

func endpointPlaceholder(kind string) string {
	if endpoint := defaultEndpoint(kind); endpoint != "" {
		return endpoint
	}
	return "https://api.example.com"
}
