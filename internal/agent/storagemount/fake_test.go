package storagemount

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitErr mimics *exec.ExitError for a command that ran and failed.
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// fakeExport describes how one NFS export behaves when mounted.
type fakeExport struct {
	failOutput string // non-empty: mount fails with exit 32 and this stderr
	readOnly   bool   // the mounted tree rejects writes
	vers       string // negotiated version, default 4.2
}

// fakeHost is a faithful stand-in for the mount and umount commands. It validates the argument
// shape the real mount(8) would accept, and keeps a mountinfo file in sync with what is mounted.
type fakeHost struct {
	t        *testing.T
	mu       sync.Mutex
	exports  map[string]fakeExport
	busy     map[string]bool // mount points umount refuses with "target is busy"
	mounts   []fakeMount
	calls    [][]string
	infoPath string
	nextID   int

	mountHook func(ctx context.Context) // runs before mount decides; used to block

	// root, when set, is the node's root directory: the commands run in the
	// node's mount namespace and resolve paths under it, as `nsenter -t 1 -m`
	// does for an agent in a pod. ownInfoPath is then the agent's own mount
	// table, which gets the node's mounts under the directories in shared
	// only (the volumes a pod mounts from its node with propagation).
	root        string
	ownInfoPath string
	shared      []string

	// forceFail makes "umount -f -l <path>" fail with this output; forced
	// runs after a forced unmount took the mount off, as the kernel failing
	// the calls blocked on it would.
	forceFail map[string]string
	forced    func(path string)
	// forceDetachFail makes it detach the mount and then fail with this
	// output, and forceHang makes it detach and then not return until it is
	// killed: umount.nfs talks to the server after the mount point is gone.
	forceDetachFail map[string]string
	forceHang       map[string]bool
}

// fstype is what mountinfo shows; "" is nfs4.
type fakeMount struct{ path, export, opts, fstype string }

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, exports: map[string]fakeExport{}, busy: map[string]bool{}, nextID: 40}
	h.infoPath = filepath.Join(t.TempDir(), "mountinfo")
	h.writeInfo()
	return h
}

func (h *fakeHost) escape(s string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(s)
}

func (h *fakeHost) writeInfo() {
	var b, own strings.Builder
	b.WriteString("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n")
	b.WriteString("23 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n")
	own.WriteString("900 800 0:99 / / rw,relatime - overlay overlay rw\n")
	if h.ownInfoPath != "" {
		defer func() { h.replaceInfo(h.ownInfoPath, own.String()) }()
	}
	for i, m := range h.mounts {
		exp := h.exports[m.export]
		vers := exp.vers
		if vers == "" {
			vers = "4.2"
		}
		mode := "rw"
		if exp.readOnly {
			mode = "ro"
		}
		fstype := m.fstype
		if fstype == "" {
			fstype = "nfs4"
		}
		line := fmt.Sprintf("%d 22 0:%d / %s %s,noatime shared:%d - %s %s %s,vers=%s,rsize=1048576,proto=tcp\n",
			100+i, 50+i, h.escape(m.path), mode, 200+i, fstype, m.export, mode, vers)
		b.WriteString(line)
		if h.reaches(m.path) {
			own.WriteString(line)
		}
	}
	h.replaceInfo(h.infoPath, b.String())
}

// replaceInfo replaces a mount table in one step: the kernel never shows a
// reader half a table, so a read that races a mount sees the table before
// or after it.
func (h *fakeHost) replaceInfo(path, table string) {
	tmp := path + ".next"
	if err := os.WriteFile(tmp, []byte(table), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		h.t.Fatal(err)
	}
}

// reaches reports whether a mount the node makes on path shows up in the
// agent's namespace.
func (h *fakeHost) reaches(path string) bool {
	for _, dir := range h.shared {
		if strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// addForeign registers a mount that this agent did not create.
func (h *fakeHost) addForeign(path, export string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mounts = append(h.mounts, fakeMount{path: path, export: export})
	if _, ok := h.exports[export]; !ok {
		h.exports[export] = fakeExport{}
	}
	h.writeInfo()
}

// addForeignFS is addForeign for a filesystem of another type.
func (h *fakeHost) addForeignFS(path, source, fstype string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mounts = append(h.mounts, fakeMount{path: path, export: source, fstype: fstype})
	h.writeInfo()
}

// detachOutside takes the mount on path away without the manager, as an
// operator's own "umount -f -l" does.
func (h *fakeHost) detachOutside(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, m := range h.mounts {
		if m.path == path {
			h.mounts = append(h.mounts[:i], h.mounts[i+1:]...)
			break
		}
	}
	h.writeInfo()
}

// remountedAs replaces what is mounted on path, as someone unmounting it
// and mounting something else there does.
func (h *fakeHost) remountedAs(path, source, fstype string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.mounts {
		if h.mounts[i].path == path {
			h.mounts[i].export, h.mounts[i].fstype = source, fstype
		}
	}
	h.writeInfo()
}

func (h *fakeHost) mounted(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.mounts {
		if m.path == path {
			return true
		}
	}
	return false
}

func (h *fakeHost) callsTo(name string) [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]string
	for _, c := range h.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, append([]string{name}, args...))
	hook := h.mountHook
	h.mu.Unlock()
	switch name {
	case "mount":
		if hook != nil {
			hook(ctx)
		}
		return h.mount(args)
	case "umount":
		return h.umount(ctx, args)
	}
	return []byte(name + ": command not found\n"), exitErr{127}
}

func (h *fakeHost) mount(args []string) ([]byte, error) {
	// mount -t nfs -o <opts> <export> <path>, exactly as the code under test must call it.
	if len(args) != 6 || args[0] != "-t" || args[1] != "nfs" || args[2] != "-o" {
		return []byte("Usage: mount [-lhV] ...\n"), exitErr{1}
	}
	opts, export, path := args[3], args[4], args[5]
	if strings.ContainsAny(opts, " ;$`") {
		return []byte("mount.nfs: an incorrect mount option was specified\n"), exitErr{32}
	}
	if st, err := os.Stat(h.root + path); err != nil || !st.IsDir() {
		return []byte(fmt.Sprintf("mount.nfs: mount point %s does not exist\n", path)), exitErr{32}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.exports[export]
	if !ok {
		return []byte(fmt.Sprintf("mount.nfs: mounting %s failed, reason given by server: No such file or directory\n", export)), exitErr{32}
	}
	if exp.failOutput != "" {
		return []byte(exp.failOutput + "\n"), exitErr{32}
	}
	for _, m := range h.mounts {
		if m.path == path {
			// The kernel would stack the mount; the code under test must never get here.
			h.t.Errorf("fake mount: %s already has a mount stacked on it", path)
		}
	}
	if exp.readOnly {
		// Writes to a read-only tree fail; model that by occupying the probe directory name with a file.
		if err := os.WriteFile(filepath.Join(h.root+path, MetaDirName), nil, 0o444); err != nil {
			h.t.Fatal(err)
		}
	}
	h.mounts = append(h.mounts, fakeMount{path: path, export: export, opts: opts})
	h.writeInfo()
	return nil, nil
}

func (h *fakeHost) umount(ctx context.Context, args []string) ([]byte, error) {
	// A forced lazy unmount is exactly `umount -f -l <path>`: it detaches a
	// busy mount point too, and fails only for its own reasons.
	force := len(args) == 3 && args[0] == "-f" && args[1] == "-l"
	if force {
		args = args[2:]
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		h.t.Errorf("umount must be called as `umount <path>` or `umount -f -l <path>`, got %v", args)
		return []byte("umount: bad usage\n"), exitErr{1}
	}
	path := args[0]
	h.mu.Lock()
	defer h.mu.Unlock()
	if out := h.forceFail[path]; force && out != "" {
		return []byte(out + "\n"), exitErr{32}
	}
	if force && h.forced != nil {
		defer h.forced(path)
	}
	for i, m := range h.mounts {
		if m.path != path {
			continue
		}
		if h.busy[path] && !force {
			return []byte(fmt.Sprintf("umount: %s: target is busy.\n", path)), exitErr{32}
		}
		h.mounts = append(h.mounts[:i], h.mounts[i+1:]...)
		h.writeInfo()
		// The fake has no real filesystem behind the mount: what was written "through" it lands in the
		// mount point directory. A real unmount uncovers the empty directory, so clear it.
		ents, _ := os.ReadDir(h.root + path)
		for _, x := range ents {
			os.RemoveAll(filepath.Join(h.root+path, x.Name()))
		}
		if out := h.forceDetachFail[path]; force && out != "" {
			return []byte(out + "\n"), exitErr{32}
		}
		if force && h.forceHang[path] {
			h.mu.Unlock()
			<-ctx.Done() // exec kills the command when its context ends
			h.mu.Lock()
			return nil, fmt.Errorf("signal: killed")
		}
		return nil, nil
	}
	return []byte(fmt.Sprintf("umount: %s: not mounted.\n", path)), exitErr{32}
}

// fakeStatfs returns canned capacity and can block chosen paths until released.
type fakeStatfs struct {
	mu      sync.Mutex
	calls   map[string]int
	block   map[string]chan struct{}
	failFor map[string]error
	fsType  map[string]uint64
}

func newFakeStatfs() *fakeStatfs {
	return &fakeStatfs{calls: map[string]int{}, block: map[string]chan struct{}{}, failFor: map[string]error{}, fsType: map[string]uint64{}}
}

func (f *fakeStatfs) blockPath(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.block[p] = ch
	return ch
}

// letThrough stops blocking new calls on p; the ones already blocked stay
// blocked until the channel blockPath returned is closed.
func (f *fakeStatfs) letThrough(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.block, p)
}

func (f *fakeStatfs) count(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[p]
}

func (f *fakeStatfs) fn(path string) (Usage, error) {
	f.mu.Lock()
	f.calls[path]++
	ch := f.block[path]
	err := f.failFor[path]
	f.mu.Unlock()
	if ch != nil {
		<-ch
	}
	if err != nil {
		return Usage{}, err
	}
	f.mu.Lock()
	magic, ok := f.fsType[path]
	f.mu.Unlock()
	if !ok {
		magic = 0x6969 // NFS_SUPER_MAGIC, unless a test says the path is on the local disk
	}
	return Usage{TotalBytes: 100 << 30, AvailableBytes: 40 << 30, FSType: magic}, nil
}

// onLocalDisk makes statfs report an ext4 filesystem for path.
func (f *fakeStatfs) onLocalDisk(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fsType[path] = 0xEF53
}

type env struct {
	h    *fakeHost
	sf   *fakeStatfs
	root bool
	dir  string
	busy func(string) bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{h: newFakeHost(t), sf: newFakeStatfs(), root: true, dir: t.TempDir()}
}

func (e *env) manager(mut ...func(*Options)) *Manager {
	o := Options{
		Runner:        e.h.run,
		MountinfoPath: e.h.infoPath,
		Statfs:        e.sf.fn,
		IsRoot:        func() bool { return e.root },
		CallTimeout:   300 * time.Millisecond,
		MountTimeout:  time.Second,
		ProbeDir:      filepath.Join(e.dir, "probe"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if e.busy != nil {
		o.Busy = e.busy
	}
	for _, f := range mut {
		f(&o)
	}
	return New(o)
}

func (e *env) path(name string) string { return filepath.Join(e.dir, name) }

func (e *env) nfsSpec(id uint, name, export string) Spec {
	return Spec{ID: id, Name: name, Type: TypeNFS, Path: e.path(name), Export: export}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *fakeHost) mountList() []fakeMount {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeMount(nil), h.mounts...)
}
