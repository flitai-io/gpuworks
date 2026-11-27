// Package storagemount makes storages usable on one agent node and reports their state. It mounts
// NFS exports through an injectable mount command, checks health with time-limited filesystem
// calls (a hard NFS mount can block a syscall forever), and reads and writes the heartbeat files
// that let several control planes see each other on a shared export.
//
// It is a leaf package: standard library (and x/sys) only.
package storagemount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Storage types.
const (
	TypeLocal     = "local"
	TypeNFS       = "nfs"
	TypeNFSCache  = "nfs_cache"
	TypeSharedDir = "shared_dir"
)

// Storage states.
const (
	StateReady        = "ready"
	StateMounting     = "mounting"
	StateFailed       = "failed"
	StateUnresponsive = "unresponsive"
	StateUnsupported  = "unsupported"
)

// Reason codes for failed states and probe results.
const (
	ReasonMissingClient  = "missing_client"
	ReasonPermission     = "permission_denied"
	ReasonUnreachable    = "unreachable"
	ReasonSourceMismatch = "source_mismatch"
	ReasonNotRoot        = "not_root"
	ReasonPathMissing    = "path_missing"
	ReasonNotMounted     = "not_mounted"
	// The agent mounts on its node from a pod, and the mount point is not a
	// directory the two share (nodeview.go).
	ReasonNotSharedWithNode = "not_shared_with_node"
	ReasonOther             = "other"
)

// Reason codes of an unresponsive storage: what the agent will do about a
// filesystem that stopped answering (see stuck.go).
const (
	// It answers too slowly, or has only just stopped answering: the agent
	// keeps checking and unmounts nothing.
	ReasonUnresponsiveRetrying = "unresponsive_retrying"
	// A call is blocked on a mount the agent created: it will force the
	// mount off and mount it again.
	ReasonStuckWillRemount = "stuck_will_remount"
	// Something on the node reads or writes it, so the agent leaves the
	// mount alone.
	ReasonStuckInUse = "stuck_in_use"
	// The agent did not mount it and never unmounts it.
	ReasonStuckNotManaged = "stuck_not_managed"
	// Another mount sits below its mount point and would be detached too.
	ReasonStuckNestedMount = "stuck_nested_mount"
	// Its server backs another NFS mount on this node that still answers or
	// is in use; a forced unmount could abort that one's requests.
	ReasonStuckSharedServer = "stuck_shared_server"
	// Too many calls the agent gave up are still blocked; it stops forcing
	// the mount off until the agent is restarted.
	ReasonStuckRestartNeeded = "stuck_restart_needed"
)

// Names inside a storage, below its root. They repeat the shared layout in internal/proto
// (StorageMetaDir and its subdirectories); this package cannot import proto, and a test in
// internal/agent/app keeps the two in step.
const (
	MetaDirName          = ".gpuworks"
	TrashDirName         = "trash"
	ControlPlanesDirName = "control-planes"
)

// ErrUnsupported is returned by the default Statfs on platforms without a real implementation.
var ErrUnsupported = errors.New("storagemount: not supported on this platform")

// Spec is one storage the server wants usable on this node, already merged with node overrides.
type Spec struct {
	ID           uint
	Name, Type   string
	Path         string // local: directory; nfs/nfs_cache: mount point; shared_dir: existing directory
	Export       string // nfs/nfs_cache: "host:/path"
	MountOptions string // comma separated, validated by ValidateOptions
	Existing     bool   // use a mount that already exists; never mount or unmount it
	CachePath    string // nfs_cache: node cache directory
}

// Status is what the agent reports for one storage.
type Status struct {
	ID                         uint
	State, Reason, Message     string
	Path                       string
	Writable                   bool
	TotalBytes, AvailableBytes int64 // filesystem holding Path
	CachePath                  string
	CacheTotalBytes            int64
	CacheAvailableBytes        int64
	// SharedFSType and SharedFSSource name the shared filesystem a local
	// storage's directory sits on (a network or FUSE one, such as an E-HPC
	// node's /opt on NAS): every node that sees it writes the same files,
	// while the platform takes a local storage as each node's own. Empty
	// when the directory is on a local disk, and for other storage types.
	SharedFSType, SharedFSSource string
	// fsMagic is the statfs magic number of the filesystem holding Path.
	fsMagic uint64
}

// Runner runs an external command without a shell and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (combinedOutput []byte, err error)

// Usage is a statfs result.
type Usage struct {
	TotalBytes, AvailableBytes int64
	FSType                     uint64 // filesystem magic number, see IsNetworkFS
}

// Options configures a Manager. Zero values select the defaults noted per field.
type Options struct {
	Runner        Runner                           // default: exec.CommandContext(...).CombinedOutput()
	MountinfoPath string                           // default "/proc/self/mountinfo"
	Statfs        func(path string) (Usage, error) // default: real statfs on linux and macOS, ErrUnsupported elsewhere
	IsRoot        func() bool                      // default: os.Geteuid() == 0
	CallTimeout   time.Duration                    // stat, statfs, readdir and write probes; default 2s
	MountTimeout  time.Duration                    // mount and umount commands; default 30s (the agent passes 20s)
	StateFile     string                           // JSON file of mount points this agent mounted; "" keeps them in memory only
	ProbeDir      string                           // parent of temporary probe mount points; default os.TempDir()
	Busy          func(path string) bool           // true when a platform container uses something under path; default never
	// InUse reports that something on this node reads or writes under the
	// storage's path (instances in any state, downloads, copies, transfers,
	// scans), so its mount must not be forced off; default Busy(path).
	InUse func(id uint, path string) bool
	// StuckRemountAfter is how long one call may stay blocked on an NFS mount
	// this agent created before the mount is forced off and mounted again;
	// default 3m.
	StuckRemountAfter time.Duration
	// StuckMaxAbandoned is how many calls given up on one storage may still
	// be blocked before the agent stops forcing its mount off; default 8.
	StuckMaxAbandoned int
	Now               func() time.Time // default time.Now
	// NodeRoot is the node's root directory as this process sees it, set
	// when Runner runs mount and umount in the node's mount namespace and
	// the agent itself lives in another (an agent pod: "/proc/1/root").
	// Empty when the agent runs in the node's own namespace.
	NodeRoot string
	// NodeMountinfoPath is the node's mount table when NodeRoot is set;
	// default "/proc/1/mountinfo".
	NodeMountinfoPath string
	Logger            *slog.Logger
}

// Manager reconciles storages on this node. It is safe for concurrent use.
type Manager struct {
	o Options
	g *guard

	recMu   sync.Mutex // serializes Reconcile
	forceMu sync.Mutex // serializes forced unmounts with the remount after them

	mu      sync.Mutex
	mounted map[string]mountRecord // mount points this agent mounted, by path
	stuck   map[uint]stuckSince    // storages that are unresponsive, by id
	// wasEligible is, by mount point, what the previous pass found forceable
	// on its own (snapshotEligible).
	wasEligible map[string]bool
	// mounting counts, by mount point, the mount commands that have not
	// returned.
	mounting map[string]int
}

// New returns a Manager and loads the persisted mount set from Options.StateFile, if any.
func New(o Options) *Manager {
	if o.Runner == nil {
		o.Runner = defaultRunner
	}
	if o.MountinfoPath == "" {
		o.MountinfoPath = "/proc/self/mountinfo"
	}
	if o.NodeRoot != "" && o.NodeMountinfoPath == "" {
		o.NodeMountinfoPath = "/proc/1/mountinfo"
	}
	if o.Statfs == nil {
		o.Statfs = defaultStatfs
	}
	if o.IsRoot == nil {
		o.IsRoot = func() bool { return os.Geteuid() == 0 }
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 2 * time.Second
	}
	if o.MountTimeout <= 0 {
		o.MountTimeout = 30 * time.Second
	}
	if o.ProbeDir == "" {
		o.ProbeDir = os.TempDir()
	}
	if o.Busy == nil {
		o.Busy = func(string) bool { return false }
	}
	if o.InUse == nil {
		busy := o.Busy
		o.InUse = func(_ uint, path string) bool { return busy(path) }
	}
	if o.StuckRemountAfter <= 0 {
		o.StuckRemountAfter = 3 * time.Minute
	}
	if o.StuckMaxAbandoned <= 0 {
		o.StuckMaxAbandoned = 8
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Manager{o: o, g: newGuard(o.Now), mounted: map[string]mountRecord{}, stuck: map[uint]stuckSince{}, mounting: map[string]int{}}
	m.loadState()
	return m
}

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Do runs fn, a filesystem access under a storage's path, with a time limit and at most one call
// in flight per (storage, op). If fn does not return within timeout, or an earlier call of the
// same op on the same storage is still stuck, it returns ErrUnresponsive at once and starts
// nothing: a hung NFS mount costs one blocked goroutine per op instead of one per caller. fn may
// still be running after Do returned, so it must not hand results back through variables the
// caller reads when Do fails. Different ops on one storage do not block each other.
func (m *Manager) Do(id uint, op string, timeout time.Duration, fn func() error) error {
	return m.g.run(context.Background(), opKey(id, op), timeout, fn)
}

// DoAny is Do with up to slots calls of the same op running side by side on one storage: for
// short operations several clients may issue at once (listing a directory, renaming a file), where
// refusing the second caller while the first is merely busy would be wrong, yet a hung mount must
// still cost a bounded number of goroutines.
func (m *Manager) DoAny(id uint, op string, slots int, timeout time.Duration, fn func() error) error {
	if slots < 1 {
		slots = 1
	}
	keys := make([]string, slots)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s#%d", opKey(id, op), i)
	}
	return m.g.runAny(context.Background(), keys, timeout, fn)
}

// OpInFlight is when the call of op now running on a storage through Do
// started, if one is running.
func (m *Manager) OpInFlight(id uint, op string) (time.Time, bool) {
	return m.g.blockedSince(opKey(id, op))
}

// Statfs reports the capacity of the filesystem holding path through the manager's statfs (the
// real one unless a test injected another). It is not time limited; callers bound it.
func (m *Manager) Statfs(path string) (Usage, error) { return m.o.Statfs(path) }

func opKey(id uint, op string) string { return fmt.Sprintf("s%d/op/%s", id, op) }

func storageKey(id uint) string { return fmt.Sprintf("s%d", id) }
func cacheKey(id uint) string   { return fmt.Sprintf("s%d/cache", id) }
