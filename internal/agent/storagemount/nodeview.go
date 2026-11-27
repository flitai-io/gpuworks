package storagemount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// An agent in a pod runs mount on its node (Options.Runner enters the node's
// mount namespace, where instance pods find the storage) but creates the
// mount point, reads the mount table and reads and writes the storage in its
// own. The two agree only on a directory the pod mounts from the node with
// propagation in both directions. Anywhere else the node would mount where
// the agent cannot see it: the agent would report its own empty directory as
// the storage and mount again on every pass. With Options.NodeRoot set,
// reconcileNFS therefore mounts only on a directory both namespaces share,
// and takes a mount it made but cannot see off again.

// NodeMount is the command runner and the node root the manager mounts
// with; root is "" for an agent in its node's own namespace.
func (m *Manager) NodeMount() (run Runner, root string) { return m.o.Runner, m.o.NodeRoot }

// sharedWithNode reports whether the storage's mount point, which exists
// for the agent, is the same directory on the node.
func (m *Manager) sharedWithNode(ctx context.Context, sp Spec) (bool, error) {
	var same bool
	err := m.call(ctx, storageKey(sp.ID), func() error {
		own, err := os.Stat(sp.Path)
		if err != nil {
			return err
		}
		node, err := os.Stat(filepath.Join(m.o.NodeRoot, sp.Path))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		same = os.SameFile(own, node)
		return nil
	})
	if err != nil {
		return false, err // same may still be written by a stuck call
	}
	return same, nil
}

// unseenNodeMount answers for a storage whose mount point the node has a
// mount on that the agent's own mount table lacks; stop is false when the
// node has none. Mounting again would stack one more mount on the node, so
// nothing is mounted; the mounts this agent made there are taken off.
func (m *Manager) unseenNodeMount(ctx context.Context, sp Spec, st Status) (failed Status, stop bool) {
	entries, err := readMountsAt(m.o.NodeMountinfoPath)
	if err != nil {
		return failWith(st, ReasonOther, "cannot read the node's mountinfo: "+err.Error()), true
	}
	e, ok := FindMount(entries, sp.Path)
	if !ok {
		return st, false
	}
	if m.mountedByAgent(sp.Path, e.Source) {
		m.takeOffUnseen(ctx, sp, stackedOn(entries, sp.Path, e.Source))
	}
	return failWith(st, ReasonNotSharedWithNode,
		fmt.Sprintf("%s is mounted on the node from %s, but the agent does not see that mount", sp.Path, e.Source)), true
}

// stackedOn counts the mounts of source on path: an agent without this
// check mounted once more on every pass.
func stackedOn(entries []MountEntry, path, source string) int {
	n := 0
	for _, e := range entries {
		if filepath.Clean(e.MountPoint) == filepath.Clean(path) && normalizeSource(e.Source) == normalizeSource(source) {
			n++
		}
	}
	return n
}

// mountedByAgent reports whether this agent recorded mounting source on path.
func (m *Manager) mountedByAgent(path, source string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.mounted[filepath.Clean(path)]
	return ok && normalizeSource(r.Export) == normalizeSource(source)
}

// takeOffUnseen unmounts, on the node, count mounts this agent made on the
// storage's mount point and cannot see. It leaves them when something on
// the node uses the path. The record goes once the node has no mount left
// there; until then the next pass tries again (unseenNodeMount).
func (m *Manager) takeOffUnseen(ctx context.Context, sp Spec, count int) {
	log := m.o.Logger.With("storage", sp.Name, "path", sp.Path)
	if m.o.InUse(sp.ID, sp.Path) {
		log.Warn("storagemount: a mount the agent cannot see is in use on the node, leaving it mounted")
		return
	}
	for done := 0; done < count; done++ {
		out, cmdErr, err := m.runUmount(ctx, storageKey(sp.ID), sp.Path)
		if err != nil {
			log.Warn("storagemount: umount of a mount the agent cannot see did not return", "err", err, "unmounted", done)
			return
		}
		if cmdErr != nil {
			log.Warn("storagemount: umount of a mount the agent cannot see failed", "err", cmdErr, "output", truncate(string(out), 400), "unmounted", done)
			return
		}
	}
	entries, err := readMountsAt(m.o.NodeMountinfoPath)
	if err != nil {
		return
	}
	if _, left := FindMount(entries, sp.Path); !left {
		m.forgetMount(sp.Path)
	}
	log.Warn("storagemount: unmounted mounts the agent could not see", "count", count)
}
