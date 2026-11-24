package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const runtimeMountRootEnv = "GPUWORKS_RUNTIME_MOUNT_ROOT"

var runtimeMountPurposes = map[string]struct{}{
	"assets":    {},
	"pre-start": {},
	"dgpu":      {},
	"overlays":  {},
	// The peers file of a multi-node rank (peers_file.go), written read-only.
	peersMountPurpose: {},
	// PrepareInstanceDir creates this one 0755 owned by the runtime mount owner
	// and the container writes into it, which the other four never do; see
	// proto.MountPurposeKVDisk.
	proto.MountPurposeKVDisk: {},
	// Written by the container too, and unlike the KV disk kept across
	// instances until the server releases it (managed_dirs.go).
	proto.MountPurposeManagedDir: {},
}

// RuntimeMountManager owns the small part of agent state that is exposed as a
// host bind-mount source. It deliberately does not manage models, logs,
// runtime rootfs data, or other private state.
type RuntimeMountManager struct {
	stateDir  string
	root      string
	driver    agentruntime.Driver
	legacy    bool // Docker keeps its historical stateDir/catalog-assets layout.
	configErr error

	createAsOwner func(agentruntime.MountOwner, string, os.FileMode) error
	accessAsOwner func(agentruntime.MountOwner, string, bool) error
}

func newRuntimeMountManager(stateDir string, driver agentruntime.Driver) *RuntimeMountManager {
	manager := &RuntimeMountManager{stateDir: filepath.Clean(stateDir), driver: driver}
	root := strings.TrimSpace(os.Getenv(runtimeMountRootEnv))
	if root == "" {
		if driver != nil && driver.Name() == "docker" {
			manager.legacy = true
			root = stateDir
		} else {
			root = filepath.Join(stateDir, "runtime-mounts")
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		manager.configErr = fmt.Errorf("resolve runtime mount root %q: %w", root, err)
		return manager
	}
	manager.root = filepath.Clean(absolute)
	manager.createAsOwner = createDirectoryAsOwner
	manager.accessAsOwner = checkAccessAsOwner
	return manager
}

// NewRuntimeMountManager constructs the host-mount preparation helper used by
// the engine. The manager is intentionally cheap; ownership is resolved when
// the first mount is prepared, after a runtime has completed provisioning.
func NewRuntimeMountManager(stateDir string, driver agentruntime.Driver) *RuntimeMountManager {
	return newRuntimeMountManager(stateDir, driver)
}

func (m *RuntimeMountManager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

func (m *RuntimeMountManager) mountOwner() (agentruntime.MountOwner, error) {
	if m == nil {
		return agentruntime.MountOwner{}, errors.New("runtime mount manager is nil")
	}
	if m.configErr != nil {
		return agentruntime.MountOwner{}, m.configErr
	}
	owner := agentruntime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
	if provider, ok := m.driver.(agentruntime.MountOwnerProvider); ok {
		var err error
		owner, err = provider.MountOwner()
		if err != nil {
			return agentruntime.MountOwner{}, fmt.Errorf("resolve runtime mount owner: %w", err)
		}
	}
	if err := owner.Validate(); err != nil {
		return agentruntime.MountOwner{}, err
	}
	if owner.UID != os.Geteuid() && strings.TrimSpace(owner.Username) == "" {
		return agentruntime.MountOwner{}, fmt.Errorf("runtime mount owner uid=%d gid=%d has no username for user switch", owner.UID, owner.GID)
	}
	return owner, nil
}

func createDirectoryAsOwner(owner agentruntime.MountOwner, name string, mode os.FileMode) error {
	// The uid match is deliberate where a full identity match is not: the
	// mkdir runs as this process and ensureManagedDirectory chowns the group
	// afterwards, so a gid-only mismatch repairs itself and must not demand
	// a username the configuration does not carry.
	if owner.UID == os.Geteuid() {
		if err := os.Mkdir(name, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("cannot create %s as uid=%d from uid=%d", name, owner.UID, os.Geteuid())
	}
	if strings.TrimSpace(owner.Username) == "" {
		return fmt.Errorf("cannot create %s as uid=%d without username", name, owner.UID)
	}
	command := exec.Command("runuser", "-u", owner.Username, "--", "mkdir", "--", name)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("runuser mkdir %s as %s: %w: %s", name, owner.Username, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func checkAccessAsOwner(owner agentruntime.MountOwner, name string, directory bool) error {
	// The uid alone decides here, on purpose -- a different rule from
	// agentruntime.isCallingIdentity (uid and gid), and the two must not
	// be unified blindly. This check answers "can this process itself
	// reach the path": when the uid matches, this process performs the
	// access and its kernel view -- capabilities included -- is the
	// answer; a gid-only mismatch does not change what THIS process can
	// reach. OwnerCanAccess answers a different question, "can the OWNER
	// identity reach the path", where a gid-only mismatch means the
	// process's group view is not the owner's and the mode bits must
	// speak instead. Flipping this gate to uid+gid would turn a working
	// gid-mismatch-without-username configuration (accepted by mountOwner,
	// repaired by the mkdir-then-chown flow) into a hard error.
	if owner.UID == os.Geteuid() {
		return nil
	}
	if strings.TrimSpace(owner.Username) == "" {
		return fmt.Errorf("cannot preflight %s as uid=%d without username", name, owner.UID)
	}
	checks := []string{"-x"}
	if directory {
		checks = append(checks, "-r")
	} else {
		checks = []string{"-r"}
	}
	for _, check := range checks {
		if output, err := exec.Command("runuser", "-u", owner.Username, "--", "test", check, name).CombinedOutput(); err != nil {
			return fmt.Errorf("runuser access check %s %s: %w: %s", check, name, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (m *RuntimeMountManager) ensureRoot(owner agentruntime.MountOwner) error {
	if m.root == "" {
		return errors.New("runtime mount root is empty")
	}
	parent := filepath.Dir(m.root)
	if err := agentruntime.PrepareAutoDLTraversal(parent, owner); err != nil {
		return fmt.Errorf("prepare AutoDL runtime mount access: %w", err)
	}
	if m.legacy {
		if err := m.preflight(m.root, owner, true); err != nil {
			return err
		}
		return nil
	}
	if samePath(parent, m.stateDir) && owner.UID != os.Geteuid() {
		// The runtime prepares the same state directory when it provisions;
		// repeating it here repairs a directory reset since then.
		if err := agentruntime.PrepareTraversal(parent, owner); err != nil {
			return err
		}
	}
	if err := agentruntime.CheckAncestorTraversal(parent, owner); err != nil {
		return fmt.Errorf("runtime mount root parent: %w", err)
	}
	if err := ensureManagedDirectory(m.root, owner, 0o755, false, m.createAsOwner); err != nil {
		return fmt.Errorf("prepare runtime mount root %s: %w", m.root, err)
	}
	return m.preflight(m.root, owner, true)
}

func ensureManagedDirectory(name string, owner agentruntime.MountOwner, mode os.FileMode, asOwner bool, creator func(agentruntime.MountOwner, string, os.FileMode) error) error {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if asOwner {
			err = creator(owner, name, mode)
		} else {
			err = os.Mkdir(name, mode)
		}
		if err != nil {
			return err
		}
		info, err = os.Lstat(name)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed path %s is not a real directory", name)
	}
	uid, gid, ok := agentruntime.FileOwner(info)
	if !ok {
		return fmt.Errorf("inspect ownership for %s: unsupported platform metadata", name)
	}
	if uid != owner.UID || gid != owner.GID {
		if os.Geteuid() != 0 {
			return fmt.Errorf("managed directory %s owner uid=%d gid=%d, want uid=%d gid=%d", name, uid, gid, owner.UID, owner.GID)
		}
		if err := os.Chown(name, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("set owner on %s: %w", name, err)
		}
	}
	if info.Mode().Perm() != mode.Perm() {
		if err := os.Chmod(name, mode.Perm()); err != nil {
			return fmt.Errorf("set mode on %s: %w", name, err)
		}
	}
	return nil
}

// PrepareInstanceDir prepares a validated purpose/key directory for a future
// host mount type. Adding a purpose requires an explicit allowlist entry.
func (m *RuntimeMountManager) PrepareInstanceDir(purpose, key string) (string, error) {
	if _, ok := runtimeMountPurposes[purpose]; !ok {
		return "", fmt.Errorf("unsupported runtime mount purpose %q", purpose)
	}
	if !safeMountKey(key) {
		return "", fmt.Errorf("unsafe runtime mount key %q", key)
	}
	owner, err := m.mountOwner()
	if err != nil {
		return "", err
	}
	if err := m.ensureRoot(owner); err != nil {
		return "", err
	}
	purposeDir := filepath.Join(m.root, m.purposeName(purpose))
	if err := ensureManagedDirectory(purposeDir, owner, 0o755, true, m.createAsOwner); err != nil {
		return "", fmt.Errorf("prepare runtime mount purpose %s: %w", purpose, err)
	}
	if err := m.preflight(purposeDir, owner, true); err != nil {
		return "", err
	}
	destination := filepath.Join(purposeDir, key)
	if err := ensureManagedDirectory(destination, owner, 0o755, true, m.createAsOwner); err != nil {
		return "", fmt.Errorf("prepare runtime mount directory %s: %w", destination, err)
	}
	if err := m.preflight(destination, owner, true); err != nil {
		return "", err
	}
	return destination, nil
}

func (m *RuntimeMountManager) preparePurpose(purpose string) (agentruntime.MountOwner, string, error) {
	if _, ok := runtimeMountPurposes[purpose]; !ok {
		return agentruntime.MountOwner{}, "", fmt.Errorf("unsupported runtime mount purpose %q", purpose)
	}
	owner, err := m.mountOwner()
	if err != nil {
		return agentruntime.MountOwner{}, "", err
	}
	if err := m.ensureRoot(owner); err != nil {
		return agentruntime.MountOwner{}, "", err
	}
	dir := filepath.Join(m.root, m.purposeName(purpose))
	if err := ensureManagedDirectory(dir, owner, 0o755, true, m.createAsOwner); err != nil {
		return agentruntime.MountOwner{}, "", fmt.Errorf("prepare runtime mount purpose %s: %w", purpose, err)
	}
	if err := m.preflight(dir, owner, true); err != nil {
		return agentruntime.MountOwner{}, "", err
	}
	return owner, dir, nil
}

func (m *RuntimeMountManager) purposeName(purpose string) string {
	if m.legacy {
		if purpose == "assets" {
			return "catalog-assets"
		}
	}
	return purpose
}

func (m *RuntimeMountManager) purposeRoot(purpose string) string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.root, m.purposeName(purpose))
}

func safeMountKey(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 255 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return !strings.Contains(value, "..")
}

func (m *RuntimeMountManager) assetPath(digest, suffix string) (agentruntime.MountOwner, string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return agentruntime.MountOwner{}, "", fmt.Errorf("invalid runtime asset digest %q", digest)
	}
	if suffix != ".dir" && suffix != ".file" {
		return agentruntime.MountOwner{}, "", fmt.Errorf("invalid runtime asset kind %q", suffix)
	}
	owner, root, err := m.preparePurpose("assets")
	if err != nil {
		return agentruntime.MountOwner{}, "", err
	}
	return owner, filepath.Join(root, digest+suffix), nil
}

func (m *RuntimeMountManager) finalizeReadOnly(source string, owner agentruntime.MountOwner) error {
	if err := m.requireWithinRoot(source); err != nil {
		return err
	}
	err := filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed mount path %s is a symlink", name)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(name, owner.UID, owner.GID); err != nil {
				return err
			}
		}
		mode := os.FileMode(0o444)
		if entry.IsDir() {
			mode = 0o555
		}
		return os.Chmod(name, mode)
	})
	if err != nil {
		return fmt.Errorf("finalize read-only runtime mount %s: %w", source, err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	return m.preflight(source, owner, info.IsDir())
}

func (m *RuntimeMountManager) writeReadOnlyFileAtomic(dir, filename string, content []byte, owner agentruntime.MountOwner) (string, error) {
	if !safeMountKey(filename) {
		return "", fmt.Errorf("unsafe runtime mount filename %q", filename)
	}
	if err := m.requireWithinRoot(dir); err != nil {
		return "", err
	}
	destination := filepath.Join(dir, filename)
	if info, err := os.Lstat(destination); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("managed mount target %s is a symlink", destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect managed mount target %s: %w", destination, err)
	} else if err == nil && info.Mode().IsRegular() {
		if existing, readErr := os.ReadFile(destination); readErr == nil && bytes.Equal(existing, content) {
			if os.Geteuid() == 0 {
				if chownErr := os.Chown(destination, owner.UID, owner.GID); chownErr != nil {
					return "", chownErr
				}
			}
			if chmodErr := os.Chmod(destination, 0o444); chmodErr != nil {
				return "", chmodErr
			}
			if preflightErr := m.preflight(destination, owner, false); preflightErr != nil {
				return "", preflightErr
			}
			return destination, nil
		}
	}
	temporary, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(content); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(temporaryName, owner.UID, owner.GID); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(temporaryName, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return "", err
	}
	published = true
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	if err := m.preflight(destination, owner, false); err != nil {
		return "", err
	}
	return destination, nil
}

func (m *RuntimeMountManager) requireWithinRoot(name string) error {
	root := filepath.Clean(m.root)
	name = filepath.Clean(name)
	relative, err := filepath.Rel(root, name)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("runtime mount path %s escapes managed root %s", name, root)
	}
	return nil
}

func (m *RuntimeMountManager) preflight(name string, owner agentruntime.MountOwner, directory bool) error {
	if err := m.requireWithinRoot(name); err != nil {
		return err
	}
	relative, _ := filepath.Rel(m.root, filepath.Clean(name))
	paths := []string{m.root}
	if relative != "." {
		current := m.root
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	for index, current := range paths {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("runtime mount preflight stat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime mount preflight path %s is a symlink", current)
		}
		isTarget := index == len(paths)-1
		if !isTarget || directory {
			if !info.IsDir() {
				return fmt.Errorf("runtime mount preflight path %s is not a directory", current)
			}
			need := os.FileMode(0o1)
			if isTarget {
				need = 0o5
			}
			if ok, probeErr := agentruntime.OwnerCanAccess(current, info, owner, need); !ok {
				return permissionError(current, info, owner, "traverse/read", probeErr)
			}
			if m.accessAsOwner != nil {
				if err := m.accessAsOwner(owner, current, true); err != nil {
					return err
				}
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime mount preflight target %s is not a regular file", current)
		}
		if ok, probeErr := agentruntime.OwnerCanAccess(current, info, owner, 0o4); !ok {
			return permissionError(current, info, owner, "read", probeErr)
		}
		if m.accessAsOwner != nil {
			if err := m.accessAsOwner(owner, current, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// permissionError reports a preflight denial with the facts that decide
// it: the owner and the mode bits. probeErr, when there is one, is the
// error the probe returned after the bits denied, wrapped so a caller
// can match it.
func permissionError(name string, info os.FileInfo, owner agentruntime.MountOwner, operation string, probeErr error) error {
	uid, gid, _ := agentruntime.FileOwner(info)
	denied := fmt.Sprintf("runtime mount %s denied for path=%s run_uid=%d run_gid=%d owner_uid=%d owner_gid=%d mode=%04o", operation, name, owner.UID, owner.GID, uid, gid, info.Mode().Perm())
	if probeErr != nil {
		return fmt.Errorf("%s: %w", denied, probeErr)
	}
	return errors.New(denied)
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}
