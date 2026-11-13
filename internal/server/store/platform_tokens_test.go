package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestPlatformTokenLifecycle(t *testing.T) {
	s := newTestStore(t)
	if _, _, _, err := s.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	var admin User
	if err := s.db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreatePlatformToken(admin.ID, "bad", "admin", nil); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
	full, row, err := s.CreatePlatformToken(admin.ID, "ci", PlatformTokenRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "gwp_") || len(full) != 47 || row.Prefix != full[:12] || strings.Contains(row.Hash, full) || row.Hash == "" {
		t.Fatalf("token %q row %+v", full, row)
	}
	got, user, ok := s.VerifyPlatformToken(full)
	if !ok || got.ID != row.ID || got.Scope != PlatformTokenRead || user.Username != "admin" || got.LastUsedAt == nil {
		t.Fatalf("verify = %+v %+v %v", got, user, ok)
	}
	// last_used_at is written at most once a minute.
	first := *got.LastUsedAt
	again, _, _ := s.VerifyPlatformToken(full)
	if again.LastUsedAt == nil || !again.LastUsedAt.Equal(first) {
		t.Fatalf("last used moved within the minute: %v -> %v", first, again.LastUsedAt)
	}
	for _, bad := range []string{"", "gw-" + full[4:], full + "x", full[:len(full)-1]} {
		if _, _, ok := s.VerifyPlatformToken(bad); ok {
			t.Fatalf("%q verified", bad)
		}
	}

	past := time.Now().Add(-time.Minute)
	expired, _, err := s.CreatePlatformToken(admin.ID, "old", PlatformTokenWrite, &past)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.VerifyPlatformToken(expired); ok {
		t.Fatal("an expired token verified")
	}
	rows, err := s.ListPlatformTokens(admin.ID)
	if err != nil || len(rows) != 2 || rows[0].Name != "old" {
		t.Fatalf("list = %+v %v", rows, err)
	}
	if others, _ := s.ListPlatformTokens(admin.ID + 1); len(others) != 0 {
		t.Fatalf("another user sees %d tokens", len(others))
	}
	if _, err := s.DeletePlatformToken(admin.ID+1, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's revoke = %v", err)
	}
	if _, err := s.DeletePlatformToken(admin.ID, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.VerifyPlatformToken(full); ok {
		t.Fatal("a revoked token verified")
	}

	// A token acts with its user as they are: a role changed or a user
	// deleted outside the store is picked up once the cached check lapses.
	full, _, _ = s.CreatePlatformToken(admin.ID, "role", PlatformTokenWrite, nil)
	if _, user, ok := s.VerifyPlatformToken(full); !ok || user.Role != "admin" {
		t.Fatalf("before the role change: %+v %v", user, ok)
	}
	if err := s.db.Model(&User{}).Where("id = ?", admin.ID).Update("role", "user").Error; err != nil {
		t.Fatal(err)
	}
	if _, user, _ := s.VerifyPlatformToken(full); user.Role != "admin" {
		t.Fatalf("a cached check re-read the user: %+v", user)
	}
	s.platformTokens.forget(auth.HashPlatformToken(full))
	if _, user, ok := s.VerifyPlatformToken(full); !ok || user.Role != "user" {
		t.Fatalf("after the role change: %+v %v", user, ok)
	}
	if err := s.db.Delete(&User{}, admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	s.platformTokens.forget(auth.HashPlatformToken(full))
	if _, _, ok := s.VerifyPlatformToken(full); ok {
		t.Fatal("a deleted user's token verified")
	}
}

// A revoke that lands while another request is reading the token must win:
// the read may not put the revoked token back into the cache.
func TestPlatformTokenRevokeRacingAVerifyIsNotCached(t *testing.T) {
	s := newTestStore(t)
	full, row, err := s.CreatePlatformToken(1, "race", PlatformTokenRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash := auth.HashPlatformToken(full)
	generation := s.platformTokens.snapshot() // a verifier starts reading
	if _, err := s.DeletePlatformToken(1, row.ID); err != nil {
		t.Fatal(err)
	}
	s.platformTokens.remember(generation, hash, verifiedPlatformToken{token: *row, until: time.Now().Add(time.Hour)})
	if _, ok := s.platformTokens.lookup(hash, time.Now()); ok {
		t.Fatal("a read that raced the revoke was cached")
	}
	if _, _, ok := s.VerifyPlatformToken(full); ok {
		t.Fatal("a revoked token verified")
	}
}

func TestPlatformTokensMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 137)
	db := open("platform-tokens")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"UserID", "Name", "Prefix", "Hash", "Scope", "ExpiresAt", "LastUsedAt", "CreatedAt"} {
		if !db.Migrator().HasColumn(&PlatformToken{}, column) {
			t.Fatalf("platform_tokens lacks %s", column)
		}
	}
	if !db.Migrator().HasIndex(&PlatformToken{}, "Hash") {
		t.Fatal("platform_tokens.hash is not indexed")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 137).Error; err != nil || marker.Name != "platform_tokens" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("platform-tokens-rollback")
	want := errors.New("injected platform_tokens failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasTable(&PlatformToken{}) {
		t.Fatal("the table survived the failed migration")
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasTable(&PlatformToken{}) {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}
