package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Independent pools force the uniqueness constraint (rather than one Store's
// single writer connection) to arbitrate two requests that both passed the
// friendly precheck before either actually inserted/renamed the group.
func TestSyncGroupNamesAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync-groups.db")
	var stores []*Store
	for range 2 {
		s, err := Open(path, bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores = append(stores, s)
	}
	raceSyncGroupWrites(t, stores, false, func(s *Store, _ int) error {
		_, err := s.CreateSyncGroup(SyncGroupSpec{Name: "raced"})
		return err
	})
	left, err := stores[0].CreateSyncGroup(SyncGroupSpec{Name: "left"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := stores[1].CreateSyncGroup(SyncGroupSpec{Name: "right"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []uint{left.ID, right.ID}
	raceSyncGroupWrites(t, stores, true, func(s *Store, index int) error {
		_, err := s.UpdateSyncGroup(ids[index], SyncGroupSpec{Name: "renamed"})
		return err
	})
	for _, name := range []string{"raced", "renamed"} {
		var count int64
		if err := stores[0].DB().Model(&SyncGroup{}).Where("name = ?", name).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("name %q count=%d err=%v", name, count, err)
		}
	}
}

func raceSyncGroupWrites(t *testing.T, stores []*Store, update bool, action func(*Store, int) error) {
	t.Helper()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	barrier := func(tx *gorm.DB) {
		if tx.Statement.Table == "sync_groups" {
			ready <- struct{}{}
			<-release
		}
	}
	for _, s := range stores {
		if update {
			if err := s.DB().Callback().Update().Before("gorm:update").Register("sync_group_name_race", barrier); err != nil {
				t.Fatal(err)
			}
			defer s.DB().Callback().Update().Remove("sync_group_name_race")
		} else {
			if err := s.DB().Callback().Create().Before("gorm:create").Register("sync_group_name_race", barrier); err != nil {
				t.Fatal(err)
			}
			defer s.DB().Callback().Create().Remove("sync_group_name_race")
		}
	}
	results := make(chan error, 2)
	for i, s := range stores {
		go func() { results <- action(s, i) }()
	}
	for range 2 {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("request failed before reaching write barrier: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("both requests did not reach the same write boundary")
		}
	}
	close(release)
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
			continue
		}
		var invalid *PullSettingError
		if !errors.As(err, &invalid) || invalid.Field != "name" || invalid.Reason != "name_duplicate" {
			t.Errorf("race error is not a localized name conflict: %v", err)
		}
	}
	if success != 1 {
		t.Errorf("race produced %d successful writes, want one", success)
	}
}
