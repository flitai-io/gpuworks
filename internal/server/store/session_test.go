package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func sessionTestStore(t *testing.T) (*Store, *User) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	s := &Store{db: db, Bus: bus.New()}
	if err := db.AutoMigrate(&User{}, &ManagementSession{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	u, ok := s.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("verify initial password")
	}
	return s, u
}

func createTestManagementSession(t *testing.T, s *Store, u *User) (string, *ManagementSession) {
	t.Helper()
	raw, session, err := s.CreateManagementSession(u, 24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || session.TokenHash != HashToken(raw) || session.TokenHash == raw {
		t.Fatal("credential must only be stored as its hash")
	}
	return raw, session
}

func TestManagementSessionLogoutIsolationAndCurrentRole(t *testing.T) {
	s, u := sessionTestStore(t)
	_, first := createTestManagementSession(t, s, u)
	_, second := createTestManagementSession(t, s, u)
	if err := s.db.Model(u).Update("role", "user").Error; err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ManagementSessionByHash(first.TokenHash)
	if err != nil || got.Role != "user" {
		t.Fatalf("current role=%+v err=%v", got, err)
	}
	sub := s.Bus.Subscribe(4, "management-session")
	defer sub.Close()
	if err := s.DeleteManagementSession(first.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ManagementSessionByHash(first.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session err=%v", err)
	}
	if _, _, err := s.ManagementSessionByHash(second.TokenHash); err != nil {
		t.Fatalf("other login invalidated: %v", err)
	}
	if err := s.DeleteManagementSession(first.TokenHash); err != nil {
		t.Fatalf("delete not idempotent: %v", err)
	}
	select {
	case event := <-sub.C():
		if event.Key != first.TokenHash {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("no revocation event")
	}
}

func TestManagementSessionPasswordChangeRevokesAllAndRejectsOldSnapshot(t *testing.T) {
	s, u := sessionTestStore(t)
	_, first := createTestManagementSession(t, s, u)
	_, second := createTestManagementSession(t, s, u)
	sub := s.Bus.Subscribe(4, "management-session")
	defer sub.Close()
	if err := s.SetUserPassword(u.Username, "gpuworks-new-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{first.TokenHash, second.TokenHash} {
		if _, _, err := s.ManagementSessionByHash(hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old credential err=%v", err)
		}
		if _, err := s.RenewManagementSession(hash, time.Hour); !errors.Is(err, ErrNotFound) {
			t.Fatalf("renew revoked credential err=%v", err)
		}
	}
	if _, _, err := s.CreateManagementSession(u, time.Hour, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale password snapshot err=%v", err)
	}
	current, ok := s.VerifyUser(u.Username, "gpuworks-new-test-password-2026")
	if !ok {
		t.Fatal("new password rejected")
	}
	createTestManagementSession(t, s, current)
	select {
	case event := <-sub.C():
		if event.Key != strconv.FormatUint(uint64(u.ID), 10) || event.Reason != "password-changed" {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("no password-change event")
	}
}

func TestManagementSessionRenewAndExpiry(t *testing.T) {
	s, u := sessionTestStore(t)
	_, session := createTestManagementSession(t, s, u)
	now := time.Now().UTC()
	absolute := now.Add(time.Hour)
	if err := s.db.Model(session).Updates(map[string]any{"expires_at": now.Add(time.Minute), "absolute_expires_at": absolute}).Error; err != nil {
		t.Fatal(err)
	}
	renewed, err := s.RenewManagementSession(session.TokenHash, 24*time.Hour)
	if err != nil || !renewed.ExpiresAt.Equal(absolute) {
		t.Fatalf("renew=%+v err=%v", renewed, err)
	}
	for _, field := range []string{"expires_at", "absolute_expires_at"} {
		if err := s.db.Model(session).Updates(map[string]any{"expires_at": now.Add(time.Hour), "absolute_expires_at": now.Add(time.Hour), field: now.Add(-time.Second)}).Error; err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ManagementSessionByHash(session.TokenHash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s expiry lookup err=%v", field, err)
		}
		if _, err := s.RenewManagementSession(session.TokenHash, time.Hour); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s expiry renewal err=%v", field, err)
		}
	}
	createTestManagementSession(t, s, u)
	var count int64
	if err := s.db.Model(&ManagementSession{}).Where("token_hash = ?", session.TokenHash).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("expired session cleanup count=%d err=%v", count, err)
	}
}

func TestManagementSessionMissingUserAndDatabaseErrors(t *testing.T) {
	s, u := sessionTestStore(t)
	_, session := createTestManagementSession(t, s, u)
	if err := s.db.Delete(u).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ManagementSessionByHash(session.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user lookup err=%v", err)
	}
	if _, err := s.RenewManagementSession(session.TokenHash, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user renew err=%v", err)
	}
	if err := s.db.Migrator().DropTable(&ManagementSession{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ManagementSessionByHash(session.TokenHash); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("database failure hidden: %v", err)
	}
}

func TestManagementSessionPasswordChangeRollback(t *testing.T) {
	s, u := sessionTestStore(t)
	_, session := createTestManagementSession(t, s, u)
	sub := s.Bus.Subscribe(4, "management-session")
	defer sub.Close()
	want := errors.New("injected session delete failure")
	if err := s.db.Callback().Delete().Before("gorm:delete").Register("fail_session_delete", func(tx *gorm.DB) {
		if tx.Statement.Table == "management_sessions" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserPassword(u.Username, "gpuworks-new-test-password-2026"); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := s.VerifyUser(u.Username, "gpuworks-test-password-2026"); !ok {
		t.Fatal("original password was not restored after failed transaction")
	}
	if _, _, err := s.ManagementSessionByHash(session.TokenHash); err != nil {
		t.Fatalf("session revoked on rollback: %v", err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("published failed transaction: %+v", event)
	default:
	}
}

func TestManagementSessionPersistsAcrossRestartAndExternalPasswordChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	u, ok := s.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("verify")
	}
	_, session := createTestManagementSession(t, s, u)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.ManagementSessionByHash(session.TokenHash); err != nil {
		t.Fatalf("session lost after restart: %v", err)
	}
	external, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	if err := external.SetUserPassword("admin", "gpuworks-changed-by-cli-2026"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ManagementSessionByHash(session.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("external reset not observed: %v", err)
	}
}

func TestManagementSessionInvalidLifetimesAndInitialAbsoluteLimit(t *testing.T) {
	s, u := sessionTestStore(t)
	for _, tt := range []struct{ idle, max time.Duration }{{0, time.Hour}, {time.Hour, 0}, {-time.Second, time.Hour}, {time.Hour, -time.Second}} {
		if raw, session, err := s.CreateManagementSession(u, tt.idle, tt.max); err == nil || raw != "" || session != nil {
			t.Fatalf("invalid lifetimes created credential: %q %+v %v", raw, session, err)
		}
	}
	if _, _, err := s.CreateManagementSession(nil, time.Hour, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil account err=%v", err)
	}
	_, session, err := s.CreateManagementSession(u, 24*time.Hour, time.Hour)
	if err != nil || !session.ExpiresAt.Equal(session.AbsoluteExpiresAt) {
		t.Fatalf("initial absolute limit: %+v %v", session, err)
	}
	if _, err := s.RenewManagementSession(session.TokenHash, 0); err == nil {
		t.Fatal("zero renewal lifetime accepted")
	}
}

func TestManagementSessionCreateAndDeleteFailuresAreAtomic(t *testing.T) {
	s, u := sessionTestStore(t)
	_, session := createTestManagementSession(t, s, u)
	expired := time.Now().UTC().Add(-time.Hour)
	if err := s.db.Model(session).Update("expires_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected session insertion failure")
	if err := s.db.Callback().Create().Before("gorm:create").Register("fail_session_insert", func(tx *gorm.DB) {
		if tx.Statement.Table == "management_sessions" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if raw, created, err := s.CreateManagementSession(u, time.Hour, time.Hour); !errors.Is(err, want) || raw != "" || created != nil {
		t.Fatalf("create err=%v raw=%q session=%+v", err, raw, created)
	}
	var count int64
	if err := s.db.Model(&ManagementSession{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("failed insertion did not roll back cleanup: count=%d err=%v", count, err)
	}
	sub := s.Bus.Subscribe(4, "management-session")
	defer sub.Close()
	want = errors.New("injected session revocation failure")
	if err := s.db.Callback().Delete().Before("gorm:delete").Register("fail_session_revoke", func(tx *gorm.DB) {
		if tx.Statement.Table == "management_sessions" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManagementSession(session.TokenHash); !errors.Is(err, want) {
		t.Fatalf("revoke err=%v", err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("published failed revocation: %+v", event)
	default:
	}
}

// Every authenticated management request resolves its session; that read must
// not queue behind a write in flight on the writer's single connection.
func TestManagementSessionLookupDoesNotWaitForTheWriter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sessions.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, _, err := s.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	u, ok := s.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("verify")
	}
	_, session := createTestManagementSession(t, s, u)

	release := make(chan struct{})
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE users SET role = role").Error; err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	lookup := make(chan error, 1)
	go func() {
		_, _, err := s.ManagementSessionByHash(session.TokenHash)
		lookup <- err
	}()
	select {
	case err := <-lookup:
		if err != nil {
			t.Fatalf("lookup beside an open write: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session lookup is queued behind the writer connection")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
