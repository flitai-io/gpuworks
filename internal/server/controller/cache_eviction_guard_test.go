package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCacheEvictionWaitsForLaunchPinAndRetries(t *testing.T) {
	for _, mode := range []string{"async", "inline"} {
		t.Run(mode, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			prepareCache(t, f, 20)
			file := cacheFile(t, f, "candidate", 60, true)
			release, ok := f.st.TryPinNodeCaches([]uint{f.node.ID})
			if !ok {
				t.Fatal("pin launch")
			}
			defer release()
			if mode == "async" {
				runCache(t, f, "clear")
				op, err := f.st.CacheOperation(f.cached.ID, f.node.ID)
				if err != nil || op.State != "waiting" {
					t.Fatalf("busy eviction: %+v %v", op, err)
				}
			} else if err := f.c.evictCacheRow(t.Context(), f.node, file.ID); !errors.Is(err, errCacheEvictionBusy) {
				t.Fatalf("busy inline eviction = %v", err)
			}
			if len(f.hub.calls) != 0 {
				t.Fatalf("launch pin allowed RPCs: %v", f.hub.calls)
			}
			if _, err := f.st.ModelFileByID(file.ID); err != nil {
				t.Fatal("launch pin did not preserve cache row")
			}
			release()
			var err error
			if mode == "async" {
				err = f.c.reconcileStorageCache(t.Context(), f.cached.ID, f.node.ID)
			} else {
				err = f.c.evictCacheRow(t.Context(), f.node, file.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.ModelFileByID(file.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retry did not remove eligible row: %v", err)
			}
		})
	}
}

func TestCacheEvictionPinsNodeThroughRPCAndReleasesOnOutcome(t *testing.T) {
	for _, mode := range []string{"async", "inline"} {
		for _, failure := range []bool{false, true} {
			name := mode + "/success"
			if failure {
				name = mode + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				f := newStorageControllerFixture(t)
				prepareCache(t, f, 20)
				file := cacheFile(t, f, "candidate", 60, true)
				checkedIndexDelete := false
				if err := f.st.DB().Callback().Delete().Before("gorm:delete").Register("assert-cache-reservation", func(tx *gorm.DB) {
					if tx.Statement.Table != "model_files" {
						return
					}
					checkedIndexDelete = true
					if release, ok := f.st.TryPinNodeCaches([]uint{f.node.ID}); ok {
						release()
						t.Error("eviction released reservation before index deletion")
					}
				}); err != nil {
					t.Fatal(err)
				}
				entered, resume := make(chan struct{}), make(chan struct{})
				h := &cacheFailureHub{storageHub: f.hub, fail: failure, hook: func() {
					close(entered)
					<-resume
				}}
				f.c.hub = h
				if mode == "async" {
					if err := f.st.RequestCacheOperations(f.cached.ID, []uint{f.node.ID}, "clear", true); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() {
					if mode == "async" {
						done <- f.c.reconcileStorageCache(context.Background(), f.cached.ID, f.node.ID)
					} else {
						done <- f.c.evictCacheRow(context.Background(), f.node, file.ID)
					}
				}()
				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("eviction skipped RPC: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("eviction stalled")
				}
				// Check both the launch reservation and cache index while the
				// remote removal is in flight, before its acknowledgement.
				if release, ok := f.st.TryPinNodeCaches([]uint{f.node.ID}); ok {
					release()
					t.Error("launch crossed in-flight eviction")
				}
				if _, err := f.st.ModelFileByID(file.ID); err != nil {
					t.Error("row removed before RPC acknowledgement")
				}
				close(resume)
				select {
				case err := <-done:
					if mode == "inline" && (err != nil) != failure {
						t.Fatalf("inline result=%v failure=%v", err, failure)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("eviction did not finish")
				}
				if release, ok := f.st.TryPinNodeCaches([]uint{f.node.ID}); !ok {
					t.Fatal("eviction leaked reservation")
				} else {
					release()
				}
				if checkedIndexDelete == failure {
					t.Fatalf("index deletion checked=%v failure=%v", checkedIndexDelete, failure)
				}
				_, err := f.st.ModelFileByID(file.ID)
				if failure && err != nil || !failure && !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("row result=%v failure=%v", err, failure)
				}
			})
		}
	}
}
