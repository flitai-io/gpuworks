package runtime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ownDir returns a fresh directory the caller owns with exactly mode,
// whatever the umask: its class is the owner's, and its parent stays
// the test's private temp directory.
func ownDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// missingCapability ends a test whose premise this process lacks: the
// filesystem or namespace refuses uid 1000 as an owner, or the kernel
// does not behave toward this process the way the case needs. Strict
// mode turns the lack into a failure.
func missingCapability(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("GPUWORKS_TEST_STRICT=1: %s: %v", what, err)
	}
	t.Skipf("%s: %v", what, err)
}

// rootOverridesBits ends the test, as a capability lack, unless the
// kernel lets this root process search and read a directory its mode
// bits close to it: the capability root's pass relies on.
func rootOverridesBits(t *testing.T) {
	t.Helper()
	closed := ownDir(t, 0o200)
	if _, err := os.Lstat(closed + "/."); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			missingCapability(t, "root cannot search a directory the mode bits close to it", err)
		}
		t.Fatal(err)
	}
	f, err := os.Open(closed)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			missingCapability(t, "root cannot read a directory the mode bits close to it", err)
		}
		t.Fatal(err)
	}
	f.Close()
}

// OwnerCanAccess asks the kernel for the calling identity, so it must
// answer through the same identity the kernel sees for this process:
// reachable when the bits allow it, unreachable for a missing path,
// and where the bits deny it, the kernel's answer -- granted to root
// through a capability, refused to anyone else with the kernel's error
// attached.
func TestOwnerCanAccessFollowsTheCallingIdentity(t *testing.T) {
	type access struct {
		name string
		path string
		info os.FileInfo
		need os.FileMode
	}
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	self := MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
	if ok, probeErr := OwnerCanAccess(dir, info, self, 0o5); !ok || probeErr != nil {
		t.Fatalf("the calling identity cannot read a temp dir it owns: ok=%v err=%v", ok, probeErr)
	}
	// A nil info is stat'd from the path: a missing path answers false
	// with that error, an existing one answers as with its own info.
	missing := filepath.Join(dir, "does-not-exist")
	if ok, statErr := OwnerCanAccess(missing, nil, self, 0o4); ok || !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("missing path: ok=%v err=%v, want false with the stat error", ok, statErr)
	}
	if ok, probeErr := OwnerCanAccess(dir, nil, self, 0o5); !ok || probeErr != nil {
		t.Fatalf("nil info on an existing directory: ok=%v err=%v, want the answer its own info gives", ok, probeErr)
	}
	// A symlink stands for the directory it leads to, with a nil info
	// too. Its target is made absolute: a relative temp dir would be
	// resolved from the link's own directory.
	target, err := filepath.Abs(ownDir(t, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Write on a directory has no honest probe: the bits answer through
	// the link, and no probe runs.
	readonly, err := filepath.Abs(ownDir(t, 0o500))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(readonly, filepath.Join(dir, "readonly")); err != nil {
		t.Fatal(err)
	}
	if ok, probeErr := OwnerCanAccess(filepath.Join(dir, "readonly"), nil, self, 0o2); ok || probeErr != nil {
		t.Fatalf("write through a symlink to a 0500 directory: ok=%v err=%v, want the mode bits' refusal with no probe", ok, probeErr)
	}
	xonly := ownDir(t, 0o111)
	xinfo, err := os.Stat(xonly)
	if err != nil {
		t.Fatal(err)
	}
	ronly := ownDir(t, 0o400)
	rinfo, err := os.Stat(ronly)
	if err != nil {
		t.Fatal(err)
	}
	deniedFile := filepath.Join(dir, "denied")
	if err := os.WriteFile(deniedFile, []byte("x"), 0o200); err != nil {
		t.Fatal(err)
	}
	deniedInfo, err := os.Stat(deniedFile)
	if err != nil {
		t.Fatal(err)
	}
	// What the bits allow is granted with no probe, for every process.
	for _, c := range []access{
		{"search on an 0111 directory", xonly, xinfo, 0o1},
		{"read on a 0400 directory", ronly, rinfo, 0o4},
		{"write on a 0200 file", deniedFile, deniedInfo, 0o2},
	} {
		if ok, probeErr := OwnerCanAccess(c.path, c.info, self, c.need); !ok || probeErr != nil {
			t.Errorf("%s: ok=%v err=%v, want granted by the bits with no probe", c.name, ok, probeErr)
		}
	}
	// What the bits deny is the kernel's answer.
	asked := []access{
		{"search through a symlink to a 0600 directory", link, nil, 0o1},
		// The "."-entry probe is what denies search on a readable
		// directory: open plus readdir succeeds on a 0400 directory the
		// process owns, so without the Lstat(path+"/.") step this case
		// would wrongly grant 0o1.
		{"search on a 0400 directory", ronly, rinfo, 0o1},
		{"read on an 0111 directory", xonly, xinfo, 0o5},
		{"read on a 0200 file", deniedFile, deniedInfo, 0o4},
		{"read+write on a 0200 file", deniedFile, deniedInfo, 0o6},
	}
	if os.Geteuid() == 0 {
		rootOverridesBits(t)
		for _, c := range asked {
			if ok, probeErr := OwnerCanAccess(c.path, c.info, self, c.need); !ok || probeErr != nil {
				t.Errorf("%s: ok=%v err=%v, want root granted by the kernel", c.name, ok, probeErr)
			}
		}
		return
	}
	// Without capabilities the kernel refuses too, and the verdict
	// carries its error.
	for _, c := range asked {
		if ok, probeErr := OwnerCanAccess(c.path, c.info, self, c.need); ok || !errors.Is(probeErr, fs.ErrPermission) {
			t.Errorf("%s: ok=%v err=%v, want denied with the kernel's error", c.name, ok, probeErr)
		}
	}
}

// For an owner that is someone else the mode bits are the whole
// verdict, whatever this process can reach itself: no probe runs, so
// nothing of its own reach -- root's capabilities here -- masks the
// denial the runtime identity faces.
func TestOwnerCanAccessKeepsTheModeBitVerdictForAnotherOwner(t *testing.T) {
	self := MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
	other := runtimeIn(false)
	for _, mode := range []os.FileMode{0o700, 0o711} {
		dir := ownDir(t, mode)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, need := range []os.FileMode{0o1, 0o5} {
			// The caller owns the directory and its class holds every
			// need: the bits grant, and that answer carries no error.
			if ok, probeErr := OwnerCanAccess(dir, info, self, need); !ok || probeErr != nil {
				t.Errorf("mode %#o, calling identity, need %#o: ok=%v err=%v, want granted by the owner's bits", mode, need, ok, probeErr)
			}
			// Another identity gets what the other class holds and no
			// probe, with the caller's info or with none.
			for _, given := range []os.FileInfo{info, nil} {
				if ok, probeErr := OwnerCanAccess(dir, given, other, need); ok != (mode&need == need) || probeErr != nil {
					t.Errorf("mode %#o, another owner, need %#o, info given=%v: ok=%v err=%v, want the other class's bits (%v) and no probe",
						mode, need, given != nil, ok, probeErr, mode&need == need)
				}
			}
		}
	}
	if os.Geteuid() != 0 {
		return
	}
	// The incident's shape, seen by the identity that hit it: a 0711
	// state directory another user owns under a root agent. 0711 gives
	// group and others the search alone and the owner everything. Root
	// is in the group or the other class, never the owner's, so the
	// bits grant it the search alone and only the kernel's answer
	// grants the read; the owner's bits grant, and a third identity is
	// refused the read.
	dir := ownDir(t, 0o711)
	if err := os.Chown(dir, 1000, 1000); err != nil {
		// A user namespace without uid 1000 mapped, or a root without
		// CAP_CHOWN, cannot build the shape; anything else is a fault.
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
			missingCapability(t, "cannot give a directory to uid 1000", err)
		}
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner := MountOwner{UID: 1000, GID: 1000}
	for _, need := range []os.FileMode{0o1, 0o5} {
		if ok, probeErr := OwnerCanAccess(dir, info, owner, need); !ok || probeErr != nil {
			t.Errorf("0711 owned by another user, owner, need %#o: ok=%v err=%v, want granted by the owner's bits", need, ok, probeErr)
		}
	}
	third := MountOwner{UID: 4242, GID: 4242}
	if ok, probeErr := OwnerCanAccess(dir, info, third, 0o1); !ok || probeErr != nil {
		t.Errorf("third identity, search: ok=%v err=%v, want granted by the other class's bits", ok, probeErr)
	}
	if ok, probeErr := OwnerCanAccess(dir, info, third, 0o5); ok || probeErr != nil {
		t.Errorf("third identity, read: ok=%v err=%v, want refused by the other class's bits with no probe", ok, probeErr)
	}
	rootOverridesBits(t)
	if ok, probeErr := OwnerCanAccess(dir, info, self, 0o5); !ok || probeErr != nil {
		t.Fatalf("root must read a 0711 directory it does not own: ok=%v err=%v", ok, probeErr)
	}
}

// A directory whose bits close it to the calling identity is where the
// ancestor walk meets the probe: root's answer carries the walk through
// it; an identity the kernel refuses stops there with the kernel's
// refusal in the verdict.
func TestCheckAncestorTraversalReportsTheKernelRefusal(t *testing.T) {
	closed := ownDir(t, 0o200)
	owner := MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
	err := CheckAncestorTraversal(closed, owner)
	if os.Geteuid() == 0 {
		rootOverridesBits(t)
		if err != nil {
			t.Fatalf("a root agent's walk must pass a directory whose bits deny it the search: %v", err)
		}
		return
	}
	var blocked *TraversalError
	if !errors.As(err, &blocked) || blocked.Dir != closed || blocked.ProbeErr == nil {
		t.Fatalf("err = %v, want *TraversalError for %s that carries the kernel's refusal", err, closed)
	}
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), closed+" is not traversable") {
		t.Fatalf("the verdict must name the directory and the kernel's refusal: %v", err)
	}
}
