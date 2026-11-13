package store

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/storagerules"
	"gpuworks.ai/gpuworks/internal/version"
)

// Storage errors the API maps to product errors.
var (
	ErrStorageExportTaken   = errors.New("storage export is already registered")
	ErrStorageNameTaken     = errors.New("storage name is already used")
	ErrStorageInUse         = errors.New("storage is in use")
	ErrStorageIsDefault     = errors.New("the default storage cannot be deleted until another storage is set as the default")
	ErrStorageDisabled      = errors.New("storage is disabled")
	ErrStorageConfigChanged = errors.New("storage configuration changed")
)

// StorageInUseError lists what keeps a storage from being removed or changed.
type StorageInUseError struct {
	DeploymentIDs []uint
	InstanceUUIDs []string
}

func (e *StorageInUseError) Error() string {
	return fmt.Sprintf("storage is used by deployments %v and instances %v", e.DeploymentIDs, e.InstanceUUIDs)
}

func (e *StorageInUseError) Is(target error) bool { return target == ErrStorageInUse }

// ListStorages returns every storage, the default first.
func (s *Store) ListStorages() ([]Storage, error) {
	var rows []Storage
	err := s.db.Order("is_default desc, name asc").Find(&rows).Error
	return rows, err
}

// StorageByID loads one storage.
func (s *Store) StorageByID(id uint) (*Storage, error) {
	var row Storage
	if err := s.db.First(&row, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return &row, nil
}

// DefaultStorage is the storage every "save to" choice preselects.
func (s *Store) DefaultStorage() (*Storage, error) {
	var row Storage
	err := s.db.Where("is_default = ?", true).Order("id asc").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// DefaultLocalStorageID is the storage a model-file reference without an
// explicit storage means: the default storage when it is local, otherwise the
// local storage that uses the agent's default directory, otherwise the oldest
// local storage. Zero when there is no local storage at all.
func (s *Store) DefaultLocalStorageID() uint {
	var ids []uint
	_ = s.db.Model(&Storage{}).Where("type = ?", proto.StorageLocal).
		Order("is_default DESC, id ASC").
		Limit(1).Pluck("id", &ids).Error
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// Storage directories are always stated on the storage itself, never left to
// the agent: where model files live is the operator's decision, while the
// agent's state directory moves with how the agent was installed (/opt/gpuworks,
// a custom --install-dir, a user's config directory). A node that cannot use
// these directories gets its own through a per-node override.
const (
	// DefaultLocalStoragePath is where a fresh install's local storage keeps
	// model files: the directory a root agent install has always used.
	DefaultLocalStoragePath = "/opt/gpuworks/models"
	// defaultCacheRoot holds an nfs_cache storage's node caches when the
	// storage names no cache directory: one subdirectory per storage.
	defaultCacheRoot = "/opt/gpuworks/cache"
)

// ErrStoragePathRequired is a local storage without a directory.
var ErrStoragePathRequired = errors.New("a local storage needs its directory")

// defaultMountRoot holds a shared storage's mount point when none is given.
const defaultMountRoot = "/mnt/gpuworks"

// DefaultMountPoint is where a shared storage is mounted on each node when its
// definition names no path.
func DefaultMountPoint(name string) string { return path.Join(defaultMountRoot, name) }

// StorageDefaults are the directories a storage gets when its definition
// leaves them out, for a form to show: a local storage's directory, and the
// roots a shared storage's mount point and an nfs_cache storage's node cache
// are made under (<root>/<name>).
type StorageDefaults struct {
	LocalPath string
	MountRoot string
	CacheRoot string
}

// DefaultStorageDirs returns StorageDefaults.
func DefaultStorageDirs() StorageDefaults {
	return StorageDefaults{LocalPath: DefaultLocalStoragePath, MountRoot: defaultMountRoot, CacheRoot: defaultCacheRoot}
}

// Errors CheckNFSSpec returns, wrapping the rule's own text.
var (
	ErrStorageExportInvalid  = errors.New("export must be host:/path")
	ErrStorageOptionsInvalid = errors.New("mount options are not allowed")
)

// CheckNFSSpec applies the rules the agent enforces when it mounts an NFS
// export (internal/storagerules), so a storage that saves never fails to
// mount for its definition alone.
func CheckNFSSpec(export, mountOptions string) error {
	if err := storagerules.ValidateExport(export); err != nil {
		return fmt.Errorf("%w: %v", ErrStorageExportInvalid, err)
	}
	if _, err := storagerules.ValidateMountOptions(mountOptions); err != nil {
		return fmt.Errorf("%w: %v", ErrStorageOptionsInvalid, err)
	}
	return nil
}

// DefaultCachePath is the cache directory an nfs_cache storage gets when none
// is given.
func DefaultCachePath(name string) string { return path.Join(defaultCacheRoot, name) }

// settleStorageDirs fills an nfs_cache storage's cache directory when it is
// empty and refuses a local storage without a directory.
func settleStorageDirs(row *Storage) error {
	row.Path, row.CachePath = strings.TrimSpace(row.Path), strings.TrimSpace(row.CachePath)
	switch row.Type {
	case proto.StorageLocal:
		if row.Path == "" {
			return ErrStoragePathRequired
		}
	case proto.StorageNFSCache:
		if row.CachePath == "" {
			row.CachePath = DefaultCachePath(row.Name)
		}
	}
	return nil
}

// CreateStorage inserts a storage and its node targets. A new default storage
// takes the mark from the previous one.
func (s *Store) CreateStorage(row *Storage, targets []StorageNode) (*Storage, error) {
	row.Name = strings.TrimSpace(row.Name)
	row.Export = strings.TrimSpace(row.Export)
	if err := settleStorageDirs(row); err != nil {
		return nil, err
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := checkStorageNameFree(tx, row.Name, 0); err != nil {
			return err
		}
		if err := checkStorageExportFree(tx, row.Export, 0); err != nil {
			return err
		}
		if row.IsDefault {
			if err := tx.Model(&Storage{}).Where("is_default = ?", true).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		return replaceStorageNodes(tx, row.ID, targets)
	})
	if err != nil {
		return nil, err
	}
	s.publishStorage(row.ID, "created")
	return row, nil
}

// StoragePatch is a partial update; nil fields are left alone.
type StoragePatch struct {
	ExpectedUpdatedAt *time.Time
	Name              *string
	Type              *string
	Enabled           *bool
	IsDefault         *bool
	AllNodes          *bool
	Path              *string
	MountOptions      *string
	CachePath         *string
	CacheMaxBytes     *int64
	Targets           *[]StorageNode
}

// UpdateStorage applies a patch. Moving a storage's directory, changing its
// type is refused while instances read its model files. Target edits must keep
// each live reader in scope with the same effective paths and mount ownership.
func (s *Store) UpdateStorage(id uint, patch StoragePatch) (*Storage, error) {
	var out Storage
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if patch.ExpectedUpdatedAt != nil && !patch.ExpectedUpdatedAt.Equal(out.UpdatedAt) {
			return ErrStorageConfigChanged
		}
		moves := (patch.Path != nil && strings.TrimSpace(*patch.Path) != out.Path) ||
			(patch.Type != nil && *patch.Type != out.Type) ||
			(patch.CachePath != nil && strings.TrimSpace(*patch.CachePath) != out.CachePath)
		if moves {
			if err := checkStorageIdle(tx, id, false); err != nil {
				return err
			}
		}
		if patch.Type != nil && *patch.Type == proto.StorageNFS && out.Type == proto.StorageNFSCache {
			// After checkStorageIdle: a pending deletion or a live reader is
			// reported as such, not as a cache entry.
			if err := settleNodeCachesForSwitch(tx, &out); err != nil {
				return err
			}
		}
		if patch.Enabled != nil && !*patch.Enabled || patch.MountOptions != nil && strings.TrimSpace(*patch.MountOptions) != out.MountOptions {
			if err := checkNoCacheDeletion(tx.Where("storage_id = ?", id)); err != nil {
				return err
			}
		}
		var previousTargets []StorageNode
		checkScope := patch.Targets != nil || (patch.AllNodes != nil && *patch.AllNodes != out.AllNodes)
		if checkScope {
			if err := tx.Where("storage_id = ?", id).Order("id asc").Find(&previousTargets).Error; err != nil {
				return err
			}
		}
		updates := map[string]any{"updated_at": nextStorageRevision(out.UpdatedAt)}
		if patch.Name != nil {
			if err := checkStorageNameFree(tx, *patch.Name, id); err != nil {
				return err
			}
			updates["name"] = strings.TrimSpace(*patch.Name)
		}
		if patch.Type != nil {
			updates["type"] = *patch.Type
		}
		if patch.Enabled != nil {
			updates["enabled"] = *patch.Enabled
		}
		if patch.AllNodes != nil {
			updates["all_nodes"] = *patch.AllNodes
		}
		if patch.Path != nil {
			updates["path"] = strings.TrimSpace(*patch.Path)
		}
		if patch.MountOptions != nil {
			updates["mount_options"] = strings.TrimSpace(*patch.MountOptions)
		}
		if patch.CachePath != nil {
			updates["cache_path"] = strings.TrimSpace(*patch.CachePath)
		}
		if patch.CacheMaxBytes != nil {
			updates["cache_max_bytes"] = *patch.CacheMaxBytes
		}
		// The result must still name its directories: apply the patch to a
		// copy and settle it as a create would.
		final := out
		for key, value := range updates {
			switch key {
			case "name":
				final.Name = value.(string)
			case "type":
				final.Type = value.(string)
			case "path":
				final.Path = value.(string)
			case "cache_path":
				final.CachePath = value.(string)
			}
		}
		if err := settleStorageDirs(&final); err != nil {
			return err
		}
		if final.CachePath != out.CachePath {
			updates["cache_path"] = final.CachePath
		}
		if patch.IsDefault != nil {
			if *patch.IsDefault {
				if err := tx.Model(&Storage{}).Where("is_default = ? AND id <> ?", true, id).Update("is_default", false).Error; err != nil {
					return err
				}
			}
			updates["is_default"] = *patch.IsDefault
		}
		if len(updates) > 0 {
			if err := tx.Model(&Storage{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
		}
		if patch.Targets != nil {
			if err := replaceStorageNodes(tx, id, *patch.Targets); err != nil {
				return err
			}
		}
		if checkScope {
			next := out
			if patch.AllNodes != nil {
				next.AllNodes = *patch.AllNodes
			}
			if err := checkStorageReaderTargets(tx, out, next, previousTargets); err != nil {
				return err
			}
		}
		return tx.First(&out, id).Error
	})
	if err != nil {
		return nil, err
	}
	s.publishStorage(id, "updated")
	return &out, nil
}

// StorageNodes lists a storage's node and label targets. Legacy node identities
// are merged on read so API clients can edit the equivalent configuration
// without resubmitting duplicates that new writes correctly reject.
func (s *Store) StorageNodes(storageID uint) ([]StorageNode, error) {
	var rows []StorageNode
	err := s.db.Where("storage_id = ?", storageID).Order("id asc").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return canonicalStorageNodes(rows), nil
}

func replaceStorageNodes(tx *gorm.DB, storageID uint, targets []StorageNode) error {
	if err := tx.Where("storage_id = ?", storageID).Delete(&StorageNode{}).Error; err != nil {
		return err
	}
	// Preserve the established first-wins behavior for exact input duplicates.
	// Different legacy label identities for one node still need merging.
	unique := make([]StorageNode, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		key := fmt.Sprintf("%d\x00%s\x00%s", target.NodeID, target.LabelKey, target.LabelValue)
		if !seen[key] {
			unique = append(unique, target)
			seen[key] = true
		}
	}
	for _, target := range canonicalStorageNodes(unique) {
		target.ID = 0
		target.StorageID = storageID
		if err := tx.Create(&target).Error; err != nil {
			return err
		}
	}
	return nil
}

// canonicalStorageNodes gives each node one configuration row. Legacy rows
// could carry label fields as well; merge them in effectiveStorage order so
// repairing their identity preserves membership and every effective override.
func canonicalStorageNodes(targets []StorageNode) []StorageNode {
	out := make([]StorageNode, 0, len(targets))
	nodes := map[uint]int{}
	labels := map[string]bool{}
	for _, target := range targets {
		target.Path = strings.TrimSpace(target.Path)
		target.CachePath = strings.TrimSpace(target.CachePath)
		if target.NodeID != 0 {
			target.LabelKey, target.LabelValue = "", ""
			if i, exists := nodes[target.NodeID]; exists {
				previous := &out[i]
				previous.OverrideOnly = previous.OverrideOnly && target.OverrideOnly
				if target.Path != "" {
					previous.Path = target.Path
				}
				if target.CachePath != "" {
					previous.CachePath = target.CachePath
				}
				if target.CacheMaxBytes != nil {
					previous.CacheMaxBytes = target.CacheMaxBytes
				}
				previous.Existing = target.Existing
				continue
			}
			nodes[target.NodeID] = len(out)
		} else {
			key := target.LabelKey + "\x00" + target.LabelValue
			if labels[key] {
				continue
			}
			labels[key] = true
		}
		out = append(out, target)
	}
	return out
}

// checkStorageNameFree refuses a name another storage has.
func checkStorageNameFree(tx *gorm.DB, name string, exceptID uint) error {
	var taken int64
	if err := tx.Model(&Storage{}).Where("name = ? AND id <> ?", strings.TrimSpace(name), exceptID).Count(&taken).Error; err != nil {
		return err
	}
	if taken > 0 {
		return fmt.Errorf("%w: %s", ErrStorageNameTaken, name)
	}
	return nil
}

func checkStorageExportFree(tx *gorm.DB, export string, exceptID uint) error {
	export = normalizeExport(export)
	if export == "" {
		return nil
	}
	var rows []Storage
	if err := tx.Where("export <> ''").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != exceptID && normalizeExport(row.Export) == export {
			return fmt.Errorf("%w: %s is storage %q", ErrStorageExportTaken, export, row.Name)
		}
	}
	return nil
}

// normalizeExport compares host:/path exports without a trailing slash.
func normalizeExport(export string) string {
	export = strings.TrimSpace(export)
	host, path, ok := strings.Cut(export, ":")
	if !ok {
		return export
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	return strings.ToLower(host) + ":" + path
}

// checkStorageIdle refuses a change while live instances read the storage's
// model files and, when bound is set, while deployments read from it or are
// limited to it by a storage node requirement.
func checkStorageIdle(tx *gorm.DB, storageID uint, bound bool) error {
	if err := checkNoCacheDeletion(tx.Where("storage_id = ?", storageID)); err != nil {
		return err
	}
	var inUse StorageInUseError
	deps, uuids, err := liveReadersOfStorage(tx, storageID, 0)
	if err != nil {
		return err
	}
	inUse.InstanceUUIDs = uuids
	if bound {
		if inUse.DeploymentIDs, err = deploymentsUsingStorage(tx, storageID, deps); err != nil {
			return err
		}
	}
	if len(inUse.DeploymentIDs) == 0 && len(inUse.InstanceUUIDs) == 0 {
		return nil
	}
	return &inUse
}

// DeploymentsUsingStorage lists the deployments that read the storage's model
// files or are limited to it by a storage node requirement.
func (s *Store) DeploymentsUsingStorage(storageID uint) ([]uint, error) {
	readers, _, err := liveReadersOfStorage(s.reader(), storageID, 0)
	if err != nil {
		return nil, err
	}
	return deploymentsUsingStorage(s.reader(), storageID, readers)
}

func deploymentsUsingStorage(db *gorm.DB, storageID uint, readers []uint) ([]uint, error) {
	var pinned []uint
	if err := db.Model(&DeploymentNodeRequirement{}).Where("key = ? AND value = ?", nodelabel.KeyStorage, strconv.FormatUint(uint64(storageID), 10)).
		Pluck("deployment_id", &pinned).Error; err != nil {
		return nil, err
	}
	return sortedUnique(append(append([]uint{}, readers...), pinned...)), nil
}

// StorageTasksForNode is the storage part of a node's desired state.
func (s *Store) StorageTasksForNode(node *Node) ([]proto.StorageTask, error) {
	effective, err := s.StoragesForNode(node)
	if err != nil {
		return nil, err
	}
	tasks := make([]proto.StorageTask, 0, len(effective))
	identity := s.ControlPlaneIdentity()
	for _, eff := range effective {
		task := proto.StorageTask{
			ID: eff.Storage.ID, Name: eff.Storage.Name, Type: eff.Storage.Type,
			Path: eff.Path, Export: eff.Storage.Export, MountOptions: eff.Storage.MountOptions,
			Existing: eff.Existing,
		}
		if eff.Storage.Type == proto.StorageNFSCache {
			task.CachePath = eff.CachePath
		}
		if proto.StorageShared(eff.Storage.Type) && eff.Storage.Enabled {
			if writer, _ := s.HeartbeatNode(&eff.Storage); writer == node.ID {
				task.Heartbeat = &proto.StorageHeartbeat{
					ControlPlaneID: identity.ID, ControlPlaneName: identity.Name,
					Version:         version.Version,
					IntervalSeconds: int(StorageHeartbeatInterval.Seconds()),
				}
			}
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// LegacyModelsRoot is the models directory sent in the desired state: the
// default local storage's directory on this node (its override, else the
// storage's own). An agent that predates storages keeps its model files
// there; a newer one also uses it for files outside any storage.
func (s *Store) LegacyModelsRoot(node *Node) string {
	storage, err := s.StorageByID(s.DefaultLocalStorageID())
	if err != nil {
		return ""
	}
	eff, applies, err := s.EffectiveStorageForNode(storage, node)
	if err != nil || !applies {
		return ""
	}
	return eff.Path
}

func (s *Store) publishStorage(id uint, reason string) {
	if s.Bus != nil {
		s.Bus.PublishChange("storage", uintKey(id), reason)
	}
}

// storageRelPathClean rejects storage-relative paths that leave the root.
func storageRelPathClean(rel string) (string, error) {
	rel = strings.TrimSpace(filepath.ToSlash(rel))
	clean := filepath.ToSlash(filepath.Clean(rel))
	if rel == "" || clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid storage-relative path %q", rel)
	}
	return clean, nil
}
