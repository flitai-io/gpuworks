package store

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/securefile"
)

type databaseFileUse struct {
	info    os.FileInfo
	closing bool
}

// Permission repair opens and closes ordinary file descriptors, which would
// cancel this process's SQLite POSIX locks. Reserve an inode before opening
// SQLite, and retain its use until even borrowed pool connections are closed.
var databaseFiles = struct {
	sync.Mutex
	uses map[*Store]databaseFileUse
}{uses: make(map[*Store]databaseFileUse)}

func (s *Store) prepareDatabase(dsn string, policies ...securefile.Policy) error {
	policy, err := securefile.Select(policies...)
	if err != nil {
		return err
	}
	path, err := DatabaseFilePath(dsn)
	if err != nil || path == "" {
		return err
	}
	databaseFiles.Lock()
	defer databaseFiles.Unlock()
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect database %s: %w", path, err)
	}
	active := false
	for owner, use := range databaseFiles.uses {
		if use.closing && !hasDatabaseConnections(owner) {
			delete(databaseFiles.uses, owner)
			continue
		}
		if info != nil && os.SameFile(info, use.info) {
			active = true
		}
	}
	if active {
		if err := verifyDatabase(dsn, policy); err != nil {
			return fmt.Errorf("verify database %s already open in this process: %w; close existing database connections before repairing permissions", path, err)
		}
	} else if err := protectDatabase(dsn, true, policy); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect protected database %s: %w", path, err)
	}
	databaseFiles.uses[s] = databaseFileUse{info: info}
	return nil
}

// The caller has finished initializing and closing the pools before publishing
// closing=true. Admission never reads the fields of a Store still initializing.
func (s *Store) releaseDatabase() {
	if s == nil {
		return
	}
	databaseFiles.Lock()
	defer databaseFiles.Unlock()
	use, ok := databaseFiles.uses[s]
	if !ok {
		return
	}
	use.closing = true
	databaseFiles.uses[s] = use
	if !hasDatabaseConnections(s) {
		delete(databaseFiles.uses, s)
	}
}

func hasDatabaseConnections(s *Store) bool {
	for _, db := range []*gorm.DB{s.db, s.readDB} {
		if db == nil {
			continue
		}
		pool, err := db.DB()
		if err == nil && pool != nil && pool.Stats().OpenConnections > 0 {
			return true
		}
	}
	return false
}
