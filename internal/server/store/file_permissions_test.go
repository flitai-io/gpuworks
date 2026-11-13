package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestSQLiteJournalsInheritProtectedDatabaseBeforePostOpenHardening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	f, err := securefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	for _, journal := range []string{"WAL", "DELETE", "WAL"} {
		if err := db.Exec("PRAGMA journal_mode=" + journal).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE IF NOT EXISTS private_fixture (id integer)").Error; err != nil {
			t.Fatal(err)
		}
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if err := tx.Exec("INSERT INTO private_fixture(id) VALUES(1)").Error; err != nil {
			t.Fatal(err)
		}
		suffixes := []string{"-wal", "-shm"}
		if journal == "DELETE" {
			suffixes = []string{"-journal"}
		}
		for _, suffix := range suffixes {
			info, err := os.Stat(path + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0007 != 0 {
				t.Errorf("%s created public before any application chmod: %#o", suffix, info.Mode().Perm())
			}
		}
		if err := tx.Rollback().Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestDatabaseOpenRestrictsExistingCustomFileAndPreservesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom database.db")
	dsn := "file:" + filepath.ToSlash(path) + "?cache=shared"
	st, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(SettingOrphanCleanup, "true"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.GetSetting(SettingOrphanCleanup); got != "true" {
		t.Fatalf("record lost: %q", got)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("%s mode=%#o", suffix, info.Mode().Perm())
		}
	}
}

func TestProtectDatabaseRejectsSymlinkAndHandlesRollbackJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("target"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := protectDatabase(path, true); err == nil {
		t.Fatal("symlink database accepted")
	}
	os.Remove(path)
	if err := protectDatabase(path, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-journal", []byte("legacy journal"), 0664); err != nil {
		t.Fatal(err)
	}
	if err := protectDatabase(path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + "-journal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("rollback journal mode=%#o", info.Mode().Perm())
	}
}

func TestDatabaseGroupAccessMustBeExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed.db")
	policy, err := securefile.TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, bus.New(), policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0660 {
			t.Errorf("trusted file %s mode=%#o", suffix, info.Mode().Perm())
		}
	}
	st.Close()
	st, err = Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("standalone open did not remove implicit group access")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "unsafe.db"), bus.New(), securefile.Policy{}, policy); err == nil {
		t.Fatal("public database mode accepted")
	}
}

func TestDatabaseDSNProtectsTheActualDriverFilename(t *testing.T) {
	for _, kind := range []string{"plain query", "literal memory text", "literal colon memory", "literal percent", "literal hash", "encoded URI"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := "plain.db"
			dsn := ""
			switch kind {
			case "plain query":
				name = "plain.db"
			case "literal memory text":
				name = "named-mode=memory.db"
			case "literal colon memory":
				name = "has-:memory:-part.db"
			case "literal percent":
				name = "literal%20name.db"
			case "literal hash":
				name = "literal#name.db"
			case "encoded URI":
				name = "space name.db"
			}
			path := filepath.Join(dir, name)
			dsn = path
			if kind == "plain query" {
				dsn += "?_pragma=busy_timeout(5000)"
			}
			if kind == "encoded URI" {
				dsn = "file:" + strings.ReplaceAll(path, " ", "%20") + "?cache=shared"
			}
			actual, err := DatabaseFilePath(dsn)
			if err != nil || actual != path {
				t.Fatalf("parse=%q/%v want%q", actual, err, path)
			}
			st, err := Open(dsn, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if st.readDB == nil {
				t.Fatal("disk filename was misclassified as memory")
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				info, err := os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("actual driver file %s mode=%#o", suffix, info.Mode().Perm())
				}
			}
			if dsn != path && !strings.HasPrefix(dsn, "file:") {
				if _, err := os.Stat(dsn); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("created a query-suffixed shadow file")
				}
			}
		})
	}
	for _, dsn := range []string{":memory:", ":memory:?_pragma=busy_timeout(5000)", "file::memory:", "file:mem?mode=memory&cache=shared", "file:/tmp/unused.db?mode=%6demory"} {
		path, err := DatabaseFilePath(dsn)
		if err != nil || path != "" {
			t.Errorf("memory DSN %q =>%q/%v", dsn, path, err)
		}
	}
}

func TestGroupDatabaseRejectsUntrustedSidecarInheritanceBeforeCreatingIt(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	gid := 65533
	for {
		present := gid == os.Getegid()
		for _, g := range groups {
			present = present || g == gid
		}
		if !present {
			break
		}
		gid--
	}
	policy, err := securefile.TrustedGroup(gid)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "must-not-create.db")
	if _, err := Open(path, bus.New(), policy); err == nil {
		t.Fatal("accepted directory/process that cannot give sidecars the trusted gid")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe sidecar context already created a database")
	}
}
