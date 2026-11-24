package storagemount

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A mount that answers every call, only slower than the time limit, is not
// stuck: each check times out, its call returns before the next one, and
// after ten minutes of that nothing has been unmounted (a loaded NFS server).
func TestSlowButAnsweringMountIsNeverForcedOff(t *testing.T) {
	e := newEnv(t)
	clock := newStuckClock()
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager(func(o *Options) { o.Now = clock.Now; o.CallTimeout = 30 * time.Millisecond })
	sp := e.nfsSpec(1, "m", "nas:/m")
	wantState(t, one(t, m, sp), StateReady, "")
	for range 20 { // 20 checks, 30 seconds apart
		late := e.sf.blockPath(sp.Path)
		st := one(t, m, sp)
		wantState(t, st, StateUnresponsive, ReasonUnresponsiveRetrying)
		close(late) // the answer arrives after the check gave up on it
		eventually(t, "the slow call to return", func() bool {
			_, held := m.g.blockedSince(storageKey(sp.ID))
			return !held
		})
		clock.advance(30 * time.Second)
	}
	if n := len(e.h.callsTo("umount")); n != 0 || !e.h.mounted(sp.Path) {
		t.Fatalf("umount ran %d times on a mount that kept answering, mounted=%v", n, e.h.mounted(sp.Path))
	}
	e.sf.letThrough(sp.Path)
	eventually(t, "the storage to answer in time", func() bool { return one(t, m, sp).State == StateReady })
}

// The mount is already gone while a call stays blocked on it (an operator's
// own "umount -f -l"): nothing can be taken away, so after the threshold the
// blocked calls are given up and the export is mounted again.
func TestStuckMountThatIsAlreadyGoneIsMountedAgain(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	one(t, m, sp)
	e.h.detachOutside(sp.Path)
	e.sf.letThrough(sp.Path)
	clock.advance(time.Minute)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckWillRemount)
	clock.advance(2 * time.Minute)
	wantState(t, one(t, m, sp), StateReady, "")
	if n := len(e.h.callsTo("umount")); n != 0 {
		t.Fatalf("umount ran %d times with nothing mounted", n)
	}
	if n := len(e.h.callsTo("mount")); n != 2 || !e.h.mounted(sp.Path) {
		t.Fatalf("mount ran %d times, mounted=%v", n, e.h.mounted(sp.Path))
	}
}

// forcedUmountLimit is how long the hanging forced umount of the test below
// runs before it is killed.
const forcedUmountLimit = 40 * time.Millisecond

// umount can detach the mount point and still exit non-zero, or be killed at
// its time limit while it waits for the server. Either way the mount is gone,
// so the storage is mounted again in the same pass.
func TestForcedUnmountThatDetachesButFailsStillRecovers(t *testing.T) {
	for _, mode := range []string{"exits non-zero", "is killed at its time limit"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			// Only the forced umount is meant to meet a short time limit, and
			// only in the mode that hangs it. MountTimeout bounds the mount
			// too, and a 40ms one fails the remount on a loaded host with
			// "mount did not finish", which is not what this test is about.
			m, clock, sp, _ := stuckNFS(t, e, func(o *Options) {
				inner := o.Runner
				o.Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if name == "umount" && len(args) == 3 && args[0] == "-f" && args[1] == "-l" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, forcedUmountLimit)
						defer cancel()
					}
					return inner(ctx, name, args...)
				}
			})
			e.h.forced = func(path string) { e.sf.letThrough(path) }
			if mode == "exits non-zero" {
				e.h.forceDetachFail = map[string]string{sp.Path: "umount.nfs: nas: RPC: Timed out"}
			} else {
				e.h.forceHang = map[string]bool{sp.Path: true}
			}
			one(t, m, sp)
			clock.advance(3 * time.Minute)
			wantState(t, one(t, m, sp), StateReady, "")
			if n := forcedUmounts(e.h); n != 1 || !e.h.mounted(sp.Path) {
				t.Fatalf("forced=%d mounted=%v", n, e.h.mounted(sp.Path))
			}
		})
	}
}

// The path holds something other than the agent's own NFS mount of the
// recorded export: another export, another filesystem, or a second mount
// stacked on it. None of them is the agent's to force off.
func TestStuckPathHoldingSomethingElseIsNeverForcedOff(t *testing.T) {
	for name, change := range map[string]func(e *env, path string){
		"another export": func(e *env, path string) { e.h.remountedAs(path, "nas:/other", "") },
		"not nfs":        func(e *env, path string) { e.h.remountedAs(path, "nas:/m", "fuse.sshfs") },
		"stacked mount":  func(e *env, path string) { e.h.addForeign(path, "nas:/m") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			m, clock, sp, _ := stuckNFS(t, e)
			one(t, m, sp)
			change(e, sp.Path)
			for range 3 {
				clock.advance(time.Hour)
				wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckNotManaged)
			}
			if n := len(e.h.callsTo("umount")); n != 0 {
				t.Fatalf("umount ran %d times", n)
			}
		})
	}
}

// A lazy unmount detaches everything below the mount point, so a mount with
// another mount inside it is reported and left alone.
func TestStuckMountWithAMountBelowItIsNotForcedOff(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	one(t, m, sp)
	e.h.addForeignFS(sp.Path+"/scratch", "/dev/sdb1", "ext4")
	e.h.addForeignFS(sp.Path+"-sibling", "/dev/sdc1", "ext4") // not below it
	clock.advance(time.Hour)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckNestedMount)
	clock.advance(time.Hour)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckNestedMount)
	if n := len(e.h.callsTo("umount")); n != 0 {
		t.Fatalf("umount ran %d times", n)
	}
}

// A forced unmount aborts requests per NFS superblock, which mounts of one
// server can share. Another mount of the same server that is not the agent's
// own, stuck and unused keeps the stuck one from being forced; a mount of
// another server does not.
func TestStuckMountSharingItsServerWithALiveMountIsNotForcedOff(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	one(t, m, sp)
	e.h.addForeign(e.path("operator"), "NAS:/projects") // same server, any case
	e.h.addForeign(e.path("elsewhere"), "other-nas:/m")
	for range 3 {
		clock.advance(time.Hour)
		wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckSharedServer)
	}
	if n := len(e.h.callsTo("umount")); n != 0 {
		t.Fatalf("umount ran %d times", n)
	}
	e.h.detachOutside(e.path("operator"))
	clock.advance(time.Minute)
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times once only another server's mount was left", n)
	}
}

// Two storages of the agent on one server: while one still answers the stuck
// one is left alone; once both are stuck and unused they lose nothing, and
// both are forced off and mounted again.
func TestStoragesOfOneServerAreForcedOffOnlyWhenAllAreStuck(t *testing.T) {
	e := newEnv(t)
	clock := newStuckClock()
	e.h.exports["nas:/a"], e.h.exports["nas:/b"] = fakeExport{}, fakeExport{}
	// A generous time limit: the storage that answers must never be taken
	// for one that does not because the test host is busy.
	m := e.manager(func(o *Options) { o.Now = clock.Now; o.CallTimeout = 2 * time.Second })
	a, b := e.nfsSpec(1, "a", "nas:/a"), e.nfsSpec(2, "b", "nas:/b")
	both := []Spec{a, b}
	check := func() []Status { return m.Reconcile(t.Context(), both) }
	for _, st := range check() {
		wantState(t, st, StateReady, "")
	}
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	releaseA := e.sf.blockPath(a.Path)
	t.Cleanup(func() { close(releaseA) })
	check()
	clock.advance(time.Hour)
	got := check()
	wantState(t, got[0], StateUnresponsive, ReasonStuckSharedServer)
	wantState(t, got[1], StateReady, "")

	releaseB := e.sf.blockPath(b.Path)
	t.Cleanup(func() { close(releaseB) })
	check()
	clock.advance(time.Hour)
	// Each sees the other as it was at the check before: b was not yet
	// known to be stuck then.
	got = check()
	wantState(t, got[0], StateUnresponsive, ReasonStuckSharedServer)
	if forcedUmounts(e.h) != 0 {
		t.Fatalf("forced unmount ran while the other storage was not known to be stuck")
	}
	clock.advance(30 * time.Second)
	got = check()
	wantState(t, got[0], StateReady, "")
	wantState(t, got[1], StateReady, "")
	if n := forcedUmounts(e.h); n != 2 {
		t.Fatalf("forced unmount ran %d times", n)
	}
}

// While one storage of a server is forced off and mounted again, its mount
// is for a moment in the mount table without the agent's record of it. A
// second storage of that server deciding at that moment must still be forced
// off: it judges its peer by the check before, not by the record. (Found as a
// one-in-twenty failure of the test above: the second storage then waited for
// good behind a peer that had already recovered.)
func TestStorageIsForcedOffWhileItsPeerIsBetweenUnmountAndRecord(t *testing.T) {
	e := newEnv(t)
	clock := newStuckClock()
	e.h.exports["nas:/a"], e.h.exports["nas:/b"] = fakeExport{}, fakeExport{}
	m := e.manager(func(o *Options) { o.Now = clock.Now; o.CallTimeout = 2 * time.Second })
	a, b := e.nfsSpec(1, "a", "nas:/a"), e.nfsSpec(2, "b", "nas:/b")
	for _, st := range m.Reconcile(t.Context(), []Spec{a, b}) {
		wantState(t, st, StateReady, "")
	}
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	for _, sp := range []Spec{a, b} {
		release := e.sf.blockPath(sp.Path)
		t.Cleanup(func() { close(release) })
	}
	m.Reconcile(t.Context(), []Spec{a, b})
	clock.advance(time.Hour)
	// Both are found stuck, unused and forceable on their own; neither is
	// forced yet, since each still sees the other as it was a check ago.
	for _, st := range m.Reconcile(t.Context(), []Spec{a, b}) {
		wantState(t, st, StateUnresponsive, ReasonStuckSharedServer)
	}
	if n := forcedUmounts(e.h); n != 0 {
		t.Fatalf("forced unmount ran %d times before both were known to be stuck", n)
	}
	// b's mount is in the table and its record is not there yet.
	m.forgetMount(b.Path)
	clock.advance(30 * time.Second)
	wantState(t, one(t, m, a), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times", n)
	}
}

// InUse is asked about the storage itself, and whatever it reports in use is
// left alone.
func TestStuckMountAsksWhetherTheStorageIsInUse(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var asked []string
	m, clock, sp, _ := stuckNFS(t, e, func(o *Options) {
		o.InUse = func(id uint, path string) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, path)
			return id == 1
		}
	})
	one(t, m, sp)
	clock.advance(time.Hour)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckInUse)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != sp.Path || len(e.h.callsTo("umount")) != 0 {
		t.Fatalf("asked %v, umount calls %d", asked, len(e.h.callsTo("umount")))
	}
}

// Every call given up on a mount that never lets go keeps a goroutine. Once
// StuckMaxAbandoned of them are still blocked the agent stops forcing the
// mount off and says a restart is needed; when they return it resumes.
func TestStuckMountStopsBeingForcedOffOnceTooManyCallsAreGivenUp(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, release := stuckNFS(t, e, func(o *Options) { o.StuckMaxAbandoned = 2 })
	// The new mount hangs as the old one did: nothing is let through.
	one(t, m, sp)
	for forced := 1; forced <= 2; forced++ {
		clock.advance(3 * time.Minute)
		wantState(t, one(t, m, sp), StateUnresponsive, ReasonUnresponsiveRetrying)
		if n := forcedUmounts(e.h); n != forced {
			t.Fatalf("forced unmount ran %d times, want %d", n, forced)
		}
	}
	for range 3 {
		clock.advance(time.Hour)
		st := one(t, m, sp)
		wantState(t, st, StateUnresponsive, ReasonStuckRestartNeeded)
		if !strings.Contains(st.Message, sinceText) {
			t.Fatalf("message = %q", st.Message)
		}
	}
	if n := forcedUmounts(e.h); n != 2 {
		t.Fatalf("forced unmount ran %d times past the bound", n)
	}
	e.sf.letThrough(sp.Path)
	release()
	eventually(t, "the blocked calls to return", func() bool { return one(t, m, sp).State == StateReady })
	if _, err := os.Stat(sp.Path); err != nil {
		t.Fatal(err)
	}
}

// The three minutes are counted for one call, not for the storage: a call
// that was blocked and then returned, followed by another that blocks, has
// the mount forced off only once that second call has been blocked for the
// whole threshold.
func TestStuckThresholdIsCountedFromTheCallThatIsStillBlocked(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, release := stuckNFS(t, e)
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	one(t, m, sp)
	clock.advance(2*time.Minute + 30*time.Second)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckWillRemount)
	// The first call returns; the storage does not get to a ready check
	// before the next call blocks.
	again := e.sf.blockPath(sp.Path)
	t.Cleanup(func() { close(again) })
	release()
	eventually(t, "the first blocked call to return", func() bool {
		_, held := m.g.blockedSince(storageKey(sp.ID))
		return !held
	})
	one(t, m, sp) // the second call starts and blocks
	clock.advance(time.Minute)
	st := one(t, m, sp) // 3m30s unresponsive, the call in flight blocked for 1m
	wantState(t, st, StateUnresponsive, ReasonStuckWillRemount)
	if n := forcedUmounts(e.h); n != 0 {
		t.Fatalf("forced unmount ran %d times with no call blocked for the threshold", n)
	}
	clock.advance(2 * time.Minute)
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times", n)
	}
}
