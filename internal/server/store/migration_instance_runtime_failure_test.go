package store

import (
	"encoding/json"
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestInstanceRuntimeFailureMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Instance{UUID: "legacy", Message: "old detail"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := dropColumns(s.db, "instances", "runtime_failure"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = 201").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "instance_runtime_failure")
	boom := errors.New("after column")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("rollback=%v", err)
	}
	if s.db.Migrator().HasColumn(&Instance{}, "RuntimeFailure") {
		t.Fatal("failed transaction retained column")
	}
	var count int64
	s.db.Model(&SchemaMigration{}).Where("version = 201").Count(&count)
	if count != 0 {
		t.Fatal("failed transaction retained marker")
	}
	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	in, err := s.InstanceByUUID("legacy")
	if err != nil || in.Message != "old detail" || in.RuntimeFailure != "" {
		t.Fatalf("legacy=%+v %v", in, err)
	}
	if err := dropColumns(s.db, "instances", "runtime_failure"); err != nil {
		t.Fatal(err)
	}
	s.db.Exec("DELETE FROM schema_migrations WHERE version = 201")
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceRuntimeFailureMigrationAfterModelSourceIDIndex(t *testing.T) {
	s := newTestStore(t)
	if err := dropColumns(s.db, "instances", "runtime_failure"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = 201").Error; err != nil {
		t.Fatal(err)
	}
	var applied SchemaMigration
	if err := s.db.First(&applied, 200).Error; err != nil || applied.Name != "model_source_id_index" {
		t.Fatalf("existing v200 marker = %+v, err = %v", applied, err)
	}
	m := migrationByName(t, "instance_runtime_failure")
	if m.Version != 201 {
		t.Fatalf("runtime failure migration version = %d, want 201", m.Version)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !s.db.Migrator().HasColumn(&Instance{}, "RuntimeFailure") {
		t.Fatal("upgrade with the published v200 marker skipped runtime_failure")
	}
}

func TestInstanceRuntimeFailureLifecycleAndOldAgent(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Instance{UUID: "failure", State: proto.InstStarting}).Error; err != nil {
		t.Fatal(err)
	}
	failure := &proto.RuntimeFailure{Code: proto.ErrorAcceleratorResourceUnavailable, Vendor: "kunlunxin", Resource: "memlock"}
	for _, tc := range []struct {
		status proto.InstanceStatus
		want   bool
	}{
		{proto.InstanceStatus{State: proto.InstFailed, Message: "requires unlimited memlock", RuntimeFailure: failure}, true},
		{proto.InstanceStatus{}, true},
		{proto.InstanceStatus{State: proto.InstFailed, Message: "old agent error"}, true},
		{proto.InstanceStatus{State: proto.InstFailed, Message: "requires unlimited memlock", RuntimeFailure: failure}, true},
		{proto.InstanceStatus{State: proto.InstRunning}, true},
	} {
		tc.status.UUID = "failure"
		if err := s.UpdateInstanceStatus(tc.status); err != nil {
			t.Fatal(err)
		}
		in, err := s.InstanceByUUID("failure")
		if err != nil {
			t.Fatal(err)
		}
		if (in.RuntimeFailure != "") != tc.want {
			t.Fatalf("stale diagnostic: %+v", in)
		}
		if tc.want {
			var got proto.RuntimeFailure
			if json.Unmarshal([]byte(in.RuntimeFailure), &got) != nil || got != *failure {
				t.Fatalf("snapshot=%s", in.RuntimeFailure)
			}
		}
	}
	// Finished generations reject delayed reports, including old agents. An
	// authoritative server transition clears the associated diagnostic.
	if err := s.SetInstanceState("failure", proto.InstStopped, "stopped by operator"); err != nil {
		t.Fatal(err)
	}
	in, err := s.InstanceByUUID("failure")
	if err != nil || in.RuntimeFailure != "" {
		t.Fatalf("server transition retained diagnostic: %+v %v", in, err)
	}

}

func TestConditionalInstanceTransitionClearsRuntimeFailure(t *testing.T) {
	s := newTestStore(t)
	row := Instance{UUID: "conditional", DeploymentID: 7, State: proto.InstFailed, Message: "injection failure", RuntimeFailure: `{"code":"ACCELERATOR_RESOURCE_UNAVAILABLE","resource":"memlock"}`}
	if err := s.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionInstances(7, []string{row.UUID}, []string{proto.InstRunning}, proto.InstStopped, "operator stop"); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("stale transition=%v", err)
	}
	before, err := s.InstanceByUUID(row.UUID)
	if err != nil || before.RuntimeFailure != row.RuntimeFailure {
		t.Fatalf("rejected transition changed evidence: %+v %v", before, err)
	}
	if err := s.TransitionInstances(7, []string{row.UUID}, []string{proto.InstFailed}, proto.InstStopped, "operator stop"); err != nil {
		t.Fatal(err)
	}
	after, err := s.InstanceByUUID(row.UUID)
	if err != nil || after.RuntimeFailure != "" || after.Message != "operator stop" {
		t.Fatalf("transition retained stale evidence: %+v %v", after, err)
	}
}
