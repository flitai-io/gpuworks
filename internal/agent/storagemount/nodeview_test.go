package storagemount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// podEnv is an agent in a pod that mounts on its node: the commands run in
// the node's namespace, whose file tree is under nodeRoot, while the agent
// creates directories and reads its mount table in its own. The directory
// "vol" is one both have (a volume the pod mounts from the node, here a
// link from the node's tree to the agent's); with propagate set the node's
// mounts under it reach the agent.
type podEnv struct {
	*env
	nodeRoot string
	state    string
}

func newPodEnv(t *testing.T, propagate bool) *podEnv {
	t.Helper()
	e := newEnv(t)
	p := &podEnv{env: e, nodeRoot: t.TempDir(), state: filepath.Join(t.TempDir(), "mounts.json")}
	vol := e.path("vol")
	if err := os.MkdirAll(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.onNode(vol)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vol, p.onNode(vol)); err != nil {
		t.Fatal(err)
	}
	e.h.root = p.nodeRoot
	e.h.ownInfoPath = filepath.Join(t.TempDir(), "own-mountinfo")
	if propagate {
		e.h.shared = []string{vol}
	}
	e.h.writeInfo()
	return p
}

// onNode is where the node keeps path, as the test sees it.
func (p *podEnv) onNode(path string) string { return p.nodeRoot + path }

func (p *podEnv) manager(mut ...func(*Options)) *Manager {
	return p.env.manager(append([]func(*Options){func(o *Options) {
		o.MountinfoPath = p.h.ownInfoPath
		o.NodeRoot, o.NodeMountinfoPath = p.nodeRoot, p.h.infoPath
		o.StateFile = p.state
	}}, mut...)...)
}

func (p *podEnv) recorded(t *testing.T) []mountRecord {
	t.Helper()
	b, err := os.ReadFile(p.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatal(err)
	}
	return sf.Mounts
}

func TestPodMountsOnADirectoryItSharesWithItsNode(t *testing.T) {
	p := newPodEnv(t, true)
	p.h.exports["nas:/m"] = fakeExport{}
	m := p.manager()
	sp := p.nfsSpec(1, "vol/m", "nas:/m")
	for pass := 1; pass <= 3; pass++ {
		wantState(t, one(t, m, sp), StateReady, "")
	}
	if got := p.h.callsTo("mount"); len(got) != 1 {
		t.Errorf("the export is mounted once, got %v", got)
	}
	if !p.h.mounted(sp.Path) {
		t.Error("the node has no mount")
	}
	if _, err := os.Stat(p.onNode(sp.Path)); err != nil {
		t.Errorf("the mount point the agent created is not on the node: %v", err)
	}
}

// The agent's own mkdir does not create a directory on the node outside the
// volumes they share; an operator's mkdir on the node does not make the two
// one directory either. Mounting there would be invisible to the agent, and
// repeated on every pass.
func TestPodDoesNotMountWhereItsNodeHasAnotherDirectory(t *testing.T) {
	p := newPodEnv(t, true)
	p.h.exports["nas:/m"] = fakeExport{}
	m := p.manager()
	sp := p.nfsSpec(1, "mnt/m", "nas:/m")
	st := one(t, m, sp)
	wantState(t, st, StateFailed, ReasonNotSharedWithNode)
	if st.Writable || st.TotalBytes != 0 {
		t.Errorf("the agent's own empty directory must not be reported as the storage: %+v", st)
	}
	if err := os.MkdirAll(p.onNode(sp.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 3; pass++ {
		wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	}
	if len(p.h.calls) != 0 {
		t.Errorf("no command may run on the node: %v", p.h.calls)
	}
}

func TestPodTakesOffAMountThatDidNotReachIt(t *testing.T) {
	p := newPodEnv(t, false) // one directory, but the node's mounts stay on the node
	p.h.exports["nas:/m"] = fakeExport{}
	m := p.manager()
	sp := p.nfsSpec(1, "vol/m", "nas:/m")
	st := one(t, m, sp)
	wantState(t, st, StateFailed, ReasonNotSharedWithNode)
	if !strings.Contains(st.Message, "nas:/m") {
		t.Errorf("message must name the export: %q", st.Message)
	}
	if p.h.mounted(sp.Path) {
		t.Error("the mount the agent cannot see is still on the node")
	}
	if got := p.recorded(t); len(got) != 0 {
		t.Errorf("a mount that was taken off is still recorded: %+v", got)
	}
}

func TestPodKeepsTryingToTakeOffAMountItCouldNotUnmount(t *testing.T) {
	p := newPodEnv(t, false)
	p.h.exports["nas:/m"] = fakeExport{}
	m := p.manager()
	sp := p.nfsSpec(1, "vol/m", "nas:/m")
	p.h.busy[sp.Path] = true
	wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	if !p.h.mounted(sp.Path) || len(p.recorded(t)) != 1 {
		t.Fatalf("a mount that could not be unmounted stays recorded: mounted=%v recorded=%+v", p.h.mounted(sp.Path), p.recorded(t))
	}
	// The next passes must not stack another mount on it (the fake fails the test if one does).
	wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	if got := p.h.callsTo("mount"); len(got) != 1 {
		t.Errorf("mount ran again over a mount the agent cannot see: %v", got)
	}
	delete(p.h.busy, sp.Path)
	st := one(t, m, sp)
	wantState(t, st, StateFailed, ReasonNotSharedWithNode)
	if !strings.Contains(st.Message, "does not see") {
		t.Errorf("message must say the node's mount is not visible: %q", st.Message)
	}
	if p.h.mounted(sp.Path) || len(p.recorded(t)) != 0 {
		t.Errorf("the mount is taken off once umount works: mounted=%v recorded=%+v", p.h.mounted(sp.Path), p.recorded(t))
	}
}

// An agent from before this check mounted on every pass once the node had
// the directory. The agent that replaces it takes those mounts off, and only
// while nothing on the node uses the path.
func TestPodUnstacksTheMountsAnEarlierAgentLeftOnTheNode(t *testing.T) {
	p := newPodEnv(t, true)
	sp := p.nfsSpec(1, "mnt/m", "nas:/m")
	if err := os.MkdirAll(p.onNode(sp.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p.h.addForeign(sp.Path, "nas:/m")
	}
	b, _ := json.Marshal(stateFile{Mounts: []mountRecord{{Path: sp.Path, Export: "nas:/m"}}})
	if err := os.WriteFile(p.state, b, 0o644); err != nil {
		t.Fatal(err)
	}
	inUse := true
	m := p.manager(func(o *Options) { o.InUse = func(uint, string) bool { return inUse } })
	wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	if len(p.h.calls) != 0 {
		t.Fatalf("a path in use on the node is left alone: %v", p.h.calls)
	}
	inUse = false
	wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	if got := p.h.mountList(); len(got) != 0 {
		t.Fatalf("mounts left on the node: %+v", got)
	}
	if got := p.recorded(t); len(got) != 0 {
		t.Fatalf("the record outlived the mounts: %+v", got)
	}
	// With the node's mount point clear, the directories are still two.
	wantState(t, one(t, m, sp), StateFailed, ReasonNotSharedWithNode)
	if got := p.h.callsTo("mount"); len(got) != 0 {
		t.Errorf("mount must not run: %v", got)
	}
	if got := p.h.callsTo("umount"); len(got) != 3 {
		t.Errorf("umount runs once per stacked mount, got %v", got)
	}
}

func TestPodLeavesAMountOnItsNodeItDidNotMake(t *testing.T) {
	p := newPodEnv(t, true)
	sp := p.nfsSpec(1, "mnt/m", "nas:/m")
	if err := os.MkdirAll(p.onNode(sp.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	p.h.addForeign(sp.Path, "nas:/m")
	st := one(t, p.manager(), sp)
	wantState(t, st, StateFailed, ReasonNotSharedWithNode)
	if len(p.h.calls) != 0 || !p.h.mounted(sp.Path) {
		t.Errorf("an operator's mount is left alone: calls=%v", p.h.calls)
	}
}

func TestPodDoesNotMountWithoutItsNodesMountTable(t *testing.T) {
	p := newPodEnv(t, true)
	p.h.exports["nas:/m"] = fakeExport{}
	m := p.manager(func(o *Options) { o.NodeMountinfoPath = filepath.Join(p.nodeRoot, "absent") })
	wantState(t, one(t, m, p.nfsSpec(1, "vol/m", "nas:/m")), StateFailed, ReasonOther)
	if len(p.h.calls) != 0 {
		t.Errorf("no command may run: %v", p.h.calls)
	}
}

func TestNodeMountTableDefaultsToPIDOne(t *testing.T) {
	if got := New(Options{NodeRoot: "/proc/1/root"}).o.NodeMountinfoPath; got != "/proc/1/mountinfo" {
		t.Errorf("NodeMountinfoPath = %q", got)
	}
	if got := New(Options{}).o.NodeMountinfoPath; got != "" {
		t.Errorf("an agent in its node's namespace reads no second table, got %q", got)
	}
}
