package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/sqliteutil"
)

// Open connects to SQLite (pure-Go driver) and runs migrations.
// dsn "" or ":memory:" opens an in-memory database (used by tests).
// Files are owner-only unless the caller supplies a verified service-group policy.
func Open(dsn string, b *bus.Bus, policies ...securefile.Policy) (result *Store, err error) {
	defer func() { err = sqliteutil.WrapOpenError(dsn, err) }()
	inMemory := isInMemoryDSN(dsn)
	s := &Store{Bus: b}
	defer func() {
		if result == nil {
			err = errors.Join(err, s.Close())
		}
	}()
	if err := s.prepareDatabase(dsn, policies...); err != nil {
		return nil, err
	}
	if dsn == "" {
		dsn = "file::memory:?cache=shared"
	}
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logger.Silent),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	s.db = gdb
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite: one writer. Keep a small pool and enable WAL for concurrent reads.
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	gdb.Exec("PRAGMA journal_mode=WAL;")
	// WAL + NORMAL fsyncs at checkpoints instead of at every commit. Under WAL
	// the worst case is losing the most recent commits to an OS crash, never a
	// corrupt database - and the control plane's writes are agent reports that
	// the next reconnect replays anyway. FULL made every status report wait on
	// a disk flush, which is what let progress traffic block node heartbeats.
	// The observability stores (tsdb, logstore) already run this way.
	gdb.Exec("PRAGMA synchronous=NORMAL;")
	gdb.Exec("PRAGMA busy_timeout=5000;")

	if !inMemory {
		// Opened with mode=ro (belt) and query_only (braces, see readOnlyDSN)
		// so the read pool can never write even by accident — a bug that
		// routed a write through reader() should fail loudly at the
		// connection level, not silently succeed and let two pools drift.
		//
		// This must come after the writer above has already run its pragmas:
		// those force sqlite to actually create the database file (and,
		// under journal_mode=WAL, the -wal/-shm sidecars) on disk. Opening a
		// mode=ro connection to a path that does not exist yet fails with
		// SQLITE_CANTOPEN — mode=ro can only ever open an existing file, never
		// create one — so the writer's side effects are a hard prerequisite
		// here, not just a matter of establishing WAL first.
		readGDB, err := gorm.Open(sqlite.Open(readOnlyDSN(dsn)), &gorm.Config{
			Logger:  logger.Default.LogMode(logger.Silent),
			NowFunc: func() time.Time { return time.Now().UTC() },
		})
		s.readDB = readGDB
		if err != nil {
			return nil, fmt.Errorf("open sqlite (read pool): %w", sqliteutil.WrapOpenError(readOnlyDSN(dsn), err))
		}
		readSQLDB, err := readGDB.DB()
		if err != nil {
			return nil, err
		}
		// A handful of concurrent readers, never the writer's single
		// connection: WAL lets them run alongside an in-flight write without
		// blocking on it.
		readSQLDB.SetMaxOpenConns(4)
	}
	migrate := s.migrate
	if inMemory {
		migrate = s.migrateInMemory
	}
	if err := migrate(); err != nil {
		return nil, err
	}
	if err := verifyDatabase(dsn, policies...); err != nil {
		return nil, err
	}
	if err := s.EnsureTerminalSettings(); err != nil {
		return nil, err
	}
	if err := s.EnsureDeploymentPolicy(); err != nil {
		return nil, err
	}
	// Terminal sessions live only as long as the process that serves them, so any
	// record still marked open here belongs to a previous run and would otherwise
	// claim to be live forever.
	if _, err := s.CloseOrphanTerminalSessions(); err != nil {
		return nil, err
	}
	return s, nil
}

// DB exposes the raw handle to tests. Production code outside the store goes
// through Store's methods (or CloudLedger); the architecture check holds its
// non-test callers at zero (store_db_accessor_callers).
func (s *Store) DB() *gorm.DB { return s.db }

// Close releases the underlying SQLite connection pool. Applications usually
// keep a Store for their entire process lifetime, while tests may create
// several in-memory stores with the same named shared-cache DSN (for example
// under go test -count). Closing the pool is what allows SQLite to discard
// that shared in-memory database between test runs.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	defer s.releaseDatabase()
	var closeErr error
	for _, db := range []*gorm.DB{s.readDB, s.db} {
		if db == nil {
			continue
		}
		pool, err := db.DB()
		if err != nil {
			closeErr = errors.Join(closeErr, err)
			continue
		}
		closeErr = errors.Join(closeErr, pool.Close())
	}
	return closeErr
}
