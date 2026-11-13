package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAgentUpgradeMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected after tables")
	m := migration{Version: 181, Name: "agent_upgrade_runs", Apply: func(tx *gorm.DB) error {
		if err := createAgentUpgradeTables(tx); err != nil {
			return err
		}
		return failure
	}}
	if err := applyMigration(db, m); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	if db.Migrator().HasTable(&AgentUpgradeRun{}) || db.Migrator().HasTable(&AgentUpgradeItem{}) {
		t.Fatal("schema escaped failed transaction")
	}
	var count int64
	db.Model(&SchemaMigration{}).Where("version = ?", 181).Count(&count)
	if count != 0 {
		t.Fatal("marker escaped failed transaction")
	}
	m.Apply = createAgentUpgradeTables
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasColumn(&AgentUpgradeItem{}, "verified_at") || !db.Migrator().HasIndex(&AgentUpgradeItem{}, "idx_agent_upgrade_member") {
		t.Fatal("upgrade schema/index missing")
	}
	db.Model(&SchemaMigration{}).Where("version = ?", 181).Count(&count)
	if count != 1 {
		t.Fatalf("markers=%d", count)
	}
}

func TestAgentUpgradeGlobalReservationIdempotencyAndCAS(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	create := func(key string) (*AgentUpgradeRun, error) {
		return st.CreateAgentUpgrade(&AgentUpgradeRun{IdempotencyKey: key, RequestFingerprint: key, TargetVersion: "target"}, []AgentUpgradeItem{{NodeUUID: "n1", Phase: "queued"}}, now)
	}
	r, err := create("first")
	if err != nil {
		t.Fatal(err)
	}
	again, err := create("first")
	if err != nil || again.ID != r.ID {
		t.Fatalf("idempotency=%+v %v", again, err)
	}
	if _, err := create("second"); !errors.Is(err, ErrAgentUpgradeConflict) {
		t.Fatalf("overlap=%v", err)
	}
	items, _ := st.AgentUpgradeItems(r.ID)
	ok, err := st.ClaimAgentUpgrade(items[0].ID, "session", now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	ok, err = st.ClaimAgentUpgrade(items[0].ID, "another", now, now.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("double claim=%v %v", ok, err)
	}
	if _, err := st.UpdateAgentUpgradeItem(items[0].ID, []string{"transferring"}, map[string]any{"phase": "unconfirmed", "finished_at": now}); err != nil {
		t.Fatal(err)
	}
	if _, err := create("second"); !errors.Is(err, ErrAgentUpgradeConflict) {
		t.Fatalf("terminal unconfirmed released global slot: %v", err)
	}
	ok, err = st.VerifyAgentUpgrade(items[0].ID, now, "admin", "operator_verified", "checked old process ended")
	if err != nil || !ok {
		t.Fatalf("verify=%v %v", ok, err)
	}
	if _, err := create("second"); err != nil {
		t.Fatal(err)
	}
	old, _ := st.AgentUpgradeItem(r.ID, "n1")
	if old.Phase != "unconfirmed" || old.VerifiedBy != "admin" {
		t.Fatalf("history rewritten=%+v", old)
	}
}

func TestAgentUpgradeContinueHandoffIsAtomic(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	old, err := st.CreateAgentUpgrade(&AgentUpgradeRun{IdempotencyKey: "old", RequestFingerprint: "old", TargetVersion: "target"}, []AgentUpgradeItem{{NodeUUID: "n1", Phase: "queued"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PauseAgentUpgrade(old.ID, "confirmation_required"); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("item insertion failed")
	callback := "test:agent-upgrade-handoff"
	if err := st.DB().Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "agent_upgrade_items" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	req := func(key string) (*AgentUpgradeRun, error) {
		return st.CreateAgentUpgrade(&AgentUpgradeRun{IdempotencyKey: key, RequestFingerprint: key, TargetVersion: "target", SourceRunID: old.ID}, []AgentUpgradeItem{{NodeUUID: "n1", Phase: "queued"}}, now)
	}
	if _, err := req("new"); !errors.Is(err, failure) {
		t.Fatalf("injection=%v", err)
	}
	st.DB().Callback().Create().Remove(callback)
	items, _ := st.AgentUpgradeItems(old.ID)
	if items[0].Phase != "queued" {
		t.Fatal("failed handoff cancelled old queue")
	}
	next, err := req("new")
	if err != nil {
		t.Fatal(err)
	}
	items, _ = st.AgentUpgradeItems(old.ID)
	if items[0].Phase != "cancelled" || items[0].Reason != "superseded" {
		t.Fatalf("old=%+v", items)
	}
	if ok, err := st.ClaimAgentUpgrade(items[0].ID, "late", now, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("old runner claimed after handoff: %v %v", ok, err)
	}
	nextItems, _ := st.AgentUpgradeItems(next.ID)
	if len(nextItems) != 1 || nextItems[0].Phase != "queued" {
		t.Fatalf("new=%+v", nextItems)
	}
}

func TestAgentUpgradeCreationConcurrencyReservesOnlyOneRun(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			_, err := st.CreateAgentUpgrade(&AgentUpgradeRun{IdempotencyKey: fmt.Sprint(i), RequestFingerprint: fmt.Sprint(i)}, []AgentUpgradeItem{{NodeUUID: fmt.Sprint(i), Phase: "queued"}}, now)
			results <- err
		}(i)
	}
	successes := 0
	for i := 0; i < 8; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAgentUpgradeConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful global reservations=%d", successes)
	}
}

func TestAgentUpgradeRetentionPreservesUnresolvedReservations(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		phase := "succeeded"
		if i == 0 {
			phase = "unconfirmed"
		}
		_, err := st.CreateAgentUpgrade(&AgentUpgradeRun{IdempotencyKey: fmt.Sprint(i), RequestFingerprint: fmt.Sprint(i)}, []AgentUpgradeItem{{NodeUUID: fmt.Sprint(i), Phase: phase, FinishedAt: &now}}, now)
		if err != nil {
			if i > 0 && errors.Is(err, ErrAgentUpgradeConflict) { // insert completed historical fixtures without bypassing a live operation
				r := &AgentUpgradeRun{IdempotencyKey: fmt.Sprint(i), RequestFingerprint: fmt.Sprint(i)}
				if err := st.DB().Create(r).Error; err != nil {
					t.Fatal(err)
				}
				if err := st.DB().Create(&AgentUpgradeItem{RunID: r.ID, NodeUUID: fmt.Sprint(i), Phase: phase, FinishedAt: &now}).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				t.Fatal(err)
			}
		}
	}
	if err := st.PruneAgentUpgrades(20); err != nil {
		t.Fatal(err)
	}
	runs, err := st.AgentUpgradeRuns()
	if err != nil || len(runs) != 21 {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
	blocker, err := st.AgentUpgradeBlocker()
	if err != nil || blocker == nil || blocker.NodeUUID != "0" {
		t.Fatalf("unresolved history removed=%+v %v", blocker, err)
	}
}
