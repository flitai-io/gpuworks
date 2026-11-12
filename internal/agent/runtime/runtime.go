// Package runtime abstracts container execution on a node. The docker,
// udocker and nerdctl drivers all implement the same interface. The server
// selects a driver per node (auto-detect, overridable).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// ErrDaemonNotReady is what Detect returns for a runtime whose CLI and local
// socket are present but whose daemon did not answer: still starting, or too
// busy to answer in time. It is a state that passes, unlike a missing
// runtime or one the agent may not use (permission denied), which Detect
// reports as plain false.
var ErrDaemonNotReady = errors.New("container daemon is installed but not answering")

type pullProxyContextKey struct{}

// WithPullProxy carries a deployment-scoped proxy to a runtime pull without
// changing the Driver interface used by every runtime and test double. An empty
// proxy explicitly selects direct access; an untouched context inherits policy.
func WithPullProxy(ctx context.Context, proxy string) context.Context {
	return context.WithValue(ctx, pullProxyContextKey{}, strings.TrimSpace(proxy))
}

// PullProxy returns the proxy attached by WithPullProxy.
func PullProxy(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	proxy, _ := ctx.Value(pullProxyContextKey{}).(string)
	return strings.TrimSpace(proxy)
}

type pullNoProxyContextKey struct{}

// WithPullNoProxy carries the hosts a proxied pull reaches without the proxy
// (NO_PROXY syntax), alongside WithPullProxy. Without it an explicit proxy
// carries every request, as the environment's NO_PROXY never applies to an
// explicit choice.
func WithPullNoProxy(ctx context.Context, noProxy string) context.Context {
	return context.WithValue(ctx, pullNoProxyContextKey{}, strings.TrimSpace(noProxy))
}

// PullNoProxy returns the list attached by WithPullNoProxy.
func PullNoProxy(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	noProxy, _ := ctx.Value(pullNoProxyContextKey{}).(string)
	return noProxy
}

// PullRoute is the proxy choice a Go registry client applies to a pull.
func PullRoute(ctx context.Context) httpproxy.Config {
	proxy := PullProxy(ctx)
	return httpproxy.Config{HTTPProxy: proxy, HTTPSProxy: proxy, NoProxy: PullNoProxy(ctx)}
}

// PullDirect reports an explicit direct choice, distinct from no pull policy.
func PullDirect(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	proxy, explicit := ctx.Value(pullProxyContextKey{}).(string)
	return explicit && proxy == ""
}

// ValidatePullProxy rejects invalid scoped proxy configuration before a CLI can
// echo it into logs. Unspecified policy remains the runtime's responsibility.
func ValidatePullProxy(ctx context.Context) error {
	proxy := PullProxy(ctx)
	if proxy == "" {
		return nil
	}
	req, _ := http.NewRequest(http.MethodGet, "https://registry.example/", nil)
	_, err := httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: proxy, HTTPSProxy: proxy}, false)
	if err != nil {
		return fmt.Errorf("invalid image pull proxy: %w", err)
	}
	return nil
}

type launchIntentContextKey struct{}

// WithLaunchIntent marks a pull whose image a container on this node is about
// to be created from. A runtime may use the hint to prepare that container's
// root filesystem while the image is still downloading; a pull that only
// distributes an image must not pay for that, so the hint is opt-in.
func WithLaunchIntent(ctx context.Context) context.Context {
	return context.WithValue(ctx, launchIntentContextKey{}, true)
}

// LaunchIntent reports whether ctx carries WithLaunchIntent.
func LaunchIntent(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	intent, _ := ctx.Value(launchIntentContextKey{}).(bool)
	return intent
}

// PullProxyEnv applies the scoped choice to CLI registry clients. Direct clears
// inherited proxies; an explicit proxy replaces ambient bypass rules with its
// own (WithPullNoProxy). An unspecified policy preserves the caller's normal
// environment.
func PullProxyEnv(ctx context.Context, base []string) []string {
	proxy := PullProxy(ctx)
	if proxy == "" && !PullDirect(ctx) {
		return base
	}
	out := make([]string, 0, len(base)+4)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "HTTP_PROXY") || strings.EqualFold(key, "HTTPS_PROXY") || strings.EqualFold(key, "ALL_PROXY") || strings.EqualFold(key, "NO_PROXY") {
			continue
		}
		out = append(out, entry)
	}
	if proxy == "" {
		return out
	}
	out = append(out,
		"HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy,
		"http_proxy="+proxy, "https_proxy="+proxy,
	)
	if noProxy := PullNoProxy(ctx); noProxy != "" {
		out = append(out, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	}
	return out
}

// Mount is a host→container bind mount.
type Mount struct {
	Source   string // host path
	Target   string // container path
	ReadOnly bool
}

// WorkloadSpec is a driver-agnostic container spec (plan §3).
type WorkloadSpec struct {
	Name        string
	Image       string
	Cmd         []string // args (appended to the image entrypoint)
	Entrypoint  []string // override entrypoint (empty = image default)
	Env         map[string]string
	Mounts      []Mount
	GPUIndexes  []int // physical GPU indexes to expose
	DockerArgs  []string
	HostNetwork bool
	ShmBytes    int64
	Ports       []int // published ports (ignored under host network)
	// PreparationNotes are factual host/runtime changes or checks performed
	// while constructing the workload. The engine reports them before launch so
	// persistent mutations such as Ascend device-share remain in the instance
	// timeline instead of being visible only in agent logs.
	PreparationNotes []string
	// dGPU injection rides on these fields; it needs no interface change.
}

// Status is a container's observed state.
type Status struct {
	Exists    bool
	Running   bool
	ExitCode  int
	Health    string // "", "starting", "healthy", "unhealthy"
	OOMKilled bool
}

// ImageInspection is the portable subset of image metadata: the node's
// immutable image id after pull/load, and the OCI config runtime observation
// and on-demand inspection report.
type ImageInspection struct {
	// RuntimeSizeBytes is the runtime-reported per-image size, including shared layers.
	// It is neither transfer archive bytes nor uniquely allocated disk space.
	RuntimeSizeBytes *int64
	ID               string
	Entrypoint       []string
	Cmd              []string
	Env              map[string]string
	Volumes          []string
}

// ImageInspector is optional so older/custom runtime drivers remain source
// compatible; docker and udocker implement it where their CLI supports it.
type ImageInspector interface {
	InspectImage(context.Context, string) (ImageInspection, error)
}

// WorkloadInspection is the portable subset of container inspect output used
// for instance-scoped launch observation. ContainerID and ImageID must be the
// immutable runtime identities, never the caller's lookup name or image tag.
// ImageID may be unknown until the driver resolves an immutable ImageLookupRef.
type WorkloadInspection struct {
	// ImageLookupRef is an immutable reference for querying image metadata when
	// ImageID is not yet known or cannot be used for lookup. Empty means use
	// ImageID. InspectImage must resolve this exact reference; its returned ID
	// fills an unknown ImageID, or must match an already known ImageID.
	ImageLookupRef string

	ContainerID string
	ImageID     string
	Entrypoint  []string
	Cmd         []string
	Path        string
	Args        []string
	Env         map[string]string
	Mounts      []InspectedMount
	Devices     []string
	RuntimeName string
	NetworkMode string
}

type InspectedMount struct {
	Type        string
	Source      string
	Destination string
	ReadOnly    bool
}

// WorkloadInspector is optional so custom and older runtime drivers continue
// to work. Callers represent its absence as unknown/unsupported.
type WorkloadInspector interface {
	InspectWorkload(context.Context, string) (WorkloadInspection, error)
}

// NamedWorkloads is optional. A driver implementing it returns from Run a
// workload name (a Kubernetes pod's) rather than the container's runtime ID,
// so an inspection's ContainerID cannot be compared with what Run returned.
type NamedWorkloads interface {
	WorkloadHandleIsName() bool
}

// MountOwner identifies the host user that resolves bind-mount source paths.
// Username is required only when a root agent switches to a dedicated runtime
// user; direct/non-root runtimes normally leave it empty.
type MountOwner struct {
	UID      int
	GID      int
	Username string
}

func (o MountOwner) Validate() error {
	if o.UID < 0 || o.GID < 0 {
		return fmt.Errorf("invalid mount owner uid=%d gid=%d", o.UID, o.GID)
	}
	return nil
}

// MountOwnerProvider is an optional runtime extension. A runtime that executes
// as a user other than the agent process exposes that resolved identity so host
// mount paths can be prepared and checked before container creation.
type MountOwnerProvider interface {
	MountOwner() (MountOwner, error)
}

// DeviceAccessDiagnoser is an optional runtime extension used by node Doctor.
// It checks the OS identity that will execute a workload against the concrete
// accelerator devices already present in WorkloadSpec.
type DeviceAccessDiagnoser interface {
	DeviceAccessDiagnostics(WorkloadSpec) string
}

// DeviceAccessGranter is an optional administrative extension used during a
// root-assisted, non-root runtime install. Implementations derive permissions
// from the actual device nodes rather than from vendor or model names.
type DeviceAccessGranter interface {
	GrantDeviceAccess(context.Context, WorkloadSpec, string) (string, error)
}

// Driver is the container runtime contract.
type Driver interface {
	// Name returns the driver identifier ("docker", "udocker").
	Name() string
	// Detect reports whether this driver is usable on the host.
	Detect(ctx context.Context) (bool, error)
	// ImageExists reports whether the image is already present locally.
	ImageExists(ctx context.Context, image string) (bool, error)
	// PullImage pulls an image, streaming human-readable progress lines.
	PullImage(ctx context.Context, image string, onLine func(string)) error
	// Prepare does any pre-run setup (image checks, rootfs prep). May be a no-op.
	Prepare(ctx context.Context, spec WorkloadSpec) error
	// ListContainers returns the names of every container this runtime knows
	// about, stopped ones included. The names are the same identifiers Run,
	// Status, Stop and Remove accept (docker/nerdctl container names, udocker
	// container directory names), which is what orphan reclamation compares
	// against the desired set. It must not be hardcoded to one CLI: an udocker
	// node has no docker binary, and an error there used to be silently
	// discarded by the orphan sweeper.
	ListContainers(ctx context.Context) ([]string, error)
	// RunningContainers returns the names of the containers that are running
	// now, in one runtime call however many there are; the liveness monitor
	// asks once per round instead of inspecting every instance. Only a name
	// in the result says anything: a container missing from it may have
	// exited, be gone, or just not be reported, and Status tells those apart.
	RunningContainers(ctx context.Context) ([]string, error)
	// Run starts the container detached and returns its id.
	Run(ctx context.Context, spec WorkloadSpec) (containerID string, err error)
	// Stop stops a running container (graceful, then kill).
	Stop(ctx context.Context, containerID string) error
	// Remove deletes a container.
	Remove(ctx context.Context, containerID string) error
	// Status inspects a container.
	Status(ctx context.Context, containerID string) (Status, error)
	// Logs returns the container's logs; follow streams until the container ends
	// or the reader is closed.
	Logs(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error)
	// Exec runs a command in the container and returns combined output.
	Exec(ctx context.Context, containerID string, cmd []string) ([]byte, error)
}

// PTYExecer is an optional runtime extension for opening a real terminal for
// a container. The agent allocates the PTY and passes its slave to the
// returned command; runtimes only provide the argv and own the runtime-specific
// setup. Docker uses `exec` (a true attach). udocker uses a controlled shell
// invocation rooted at the tracked workload rootfs with the workload's env,
// mounts and GPU visibility; it is intentionally not presented as an attach to
// the model process.
type PTYExecer interface {
	ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error)
}

// PTYSessionExecer additionally owns remote processes created outside the local
// CLI tree. The close function must use a fresh bounded context and confirm
// cleanup; callers must not silently fall back after preparation fails.
type PTYSessionExecer interface {
	ExecPTYSession(context.Context, string, []string) ([]string, func(context.Context) error, error)
}

// RunProgressDriver is an optional extension for runtimes whose container
// preparation can take long enough to warrant user-visible phase updates.
// Drivers that do not implement it continue through Driver.Run unchanged.
type RunProgressDriver interface {
	RunWithProgress(ctx context.Context, spec WorkloadSpec, onProgress func(phase, message string)) (containerID string, err error)
}

// HostPortBinder is an optional extension for runtimes whose workloads do not
// bind host ports the way a host-network container does. The engine checks
// that an instance's allocated ports are free before creating it by binding
// each once; a runtime implementing this answers that bind in its own terms
// (a hermetic runtime standing many nodes up on one host keeps each node's
// ports to itself). A port already taken fails with syscall.EADDRINUSE.
type HostPortBinder interface {
	BindHostPort(port int) (io.Closer, error)
}

// ImageLoader accepts a Docker- or OCI-compatible image archive under the
// requested canonical reference. Probe execution only needs this narrower
// capability; ImageArchiver adds the save side that image sync needs.
type ImageLoader interface {
	LoadImage(ctx context.Context, image string, r io.Reader) error
}

// ImageArchiver is implemented by runtimes that can both save and load a
// Docker-compatible image archive. It is used for node-to-node image transfer.
type ImageArchiver interface {
	ImageLoader
	SaveImage(ctx context.Context, image string, w io.Writer) error
}

// LocalImageSource is a runtime on this node that another runtime can copy a
// local image from, so switching runtimes does not download what the node
// already holds. HoldsImage must answer without setting the runtime up or
// fetching anything: it is asked of runtimes the node is not using.
type LocalImageSource interface {
	Name() string
	HoldsImage(ctx context.Context, image string) (bool, error)
	SaveImage(ctx context.Context, image string, w io.Writer) error
}

// ImageFileLoader loads an image archive that is already a complete file on
// the node's disk. It exists alongside ImageLoader because a node that
// downloaded an archive itself must not pay for a second full-size copy just
// to hand the runtime a reader: udocker, in particular, stages any io.Reader
// to a temporary file before invoking its CLI.
type ImageFileLoader interface {
	LoadImageFile(ctx context.Context, image, path string) error
}

// LoadedRefReporter is ImageFileLoader for runtimes whose load output names
// the reference the archive actually carried ("Loaded image: <ref>" for
// docker, "unpacking <ref> (sha256:…)" for nerdctl). The returned reference is
// empty when the output did not name one. A caller uses it to tag an archive
// whose baked-in tag differs from the catalog reference, and to report the real
// tag when it cannot.
type LoadedRefReporter interface {
	ImageFileLoader
	LoadImageFileRef(ctx context.Context, image, path string) (string, error)
}

// StreamedRefReporter is ImageLoader for runtimes whose load names the
// reference the archive carried even when the archive arrived as a stream
// rather than as a file. A server-relayed archive is never a file on the node,
// so this is what lets that path run the same tag fallback as a downloaded
// one. The returned reference is empty when the output named none.
type StreamedRefReporter interface {
	ImageLoader
	LoadImageRef(ctx context.Context, image string, r io.Reader) (string, error)
}

// ImageTagger records an image already present under one reference under a
// second one. Every runtime whose CLI has a tag verb implements it; udocker
// does not need it because its load stores the archive under the repository
// name it is given.
type ImageTagger interface {
	TagImage(ctx context.Context, source, target string) error
}

// ImageRemover is implemented by runtimes that can delete a local image.
//
// It is a capability of its own rather than part of Driver because running
// containers does not imply being able to remove images, and because a runtime
// refuses while a container still uses the image — that refusal is the answer
// the operator needs, so it is returned rather than worked around.
type ImageRemover interface {
	RemoveImage(ctx context.Context, image string) error
}

// ParseLoadedImageRef extracts the image reference a runtime's archive load
// reported on stdout. It understands docker's "Loaded image: <ref>" (and the
// digest-only "Loaded image ID: <id>" form, which names no reference and is
// therefore ignored) and nerdctl's "unpacking <ref> (sha256:…)". The last
// reference wins, because a multi-image archive ends with the entry the
// runtime finished importing. An empty string means the output named none.
func ParseLoadedImageRef(output string) string {
	return ParseLoadedImageRefFor(output, "")
}

// ParseLoadedImageRefFor prefers the requested image when a multi-image load
// reported it. Otherwise it retains the last reported reference for the caller's
// retag fallback. Equivalent Docker Hub spellings refer to the requested image.
func ParseLoadedImageRefFor(output, image string) string {
	wanted := loadedImageRefName(image)
	found := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		switch {
		case strings.HasPrefix(line, "Loaded image ID:"):
			// A reference-less archive: docker printed a digest, not a tag.
			continue
		case strings.HasPrefix(line, "Loaded image:"):
			if ref := strings.TrimSpace(strings.TrimPrefix(line, "Loaded image:")); ref != "" {
				found = ref
			}
		case strings.HasPrefix(line, "unpacking "):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "unpacking "))
			if open := strings.LastIndex(rest, " ("); open > 0 {
				rest = rest[:open]
			}
			if rest = strings.TrimSpace(rest); rest != "" {
				found = rest
			}
		}
		if wanted != "" && loadedImageRefName(found) == wanted {
			return image
		}
	}
	return found
}

// loadedImageRefName applies Docker name defaults: the legacy index.docker.io
// name, library/ and the latest tag. Registry endpoint hosts remain distinct.
func loadedImageRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "@") && strings.LastIndex(ref, ":") <= strings.LastIndex(ref, "/") {
		ref += ":latest"
	}
	host, path, slash := strings.Cut(ref, "/")
	if !slash || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, path = "docker.io", ref
	}
	switch host {
	case "docker.io", "index.docker.io":
		host = "docker.io"
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
	}
	return host + "/" + path
}

// ImagePresenceReporter reads presence, identity and size in one runtime
// operation. A nil size means this runtime cannot measure it. An empty
// identity means it was not observed. Errors are not absence.
type ImagePresenceReporter interface {
	ImagePresence(context.Context, string) (bool, string, *int64, error)
}
