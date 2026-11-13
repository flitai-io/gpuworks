package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Expose only the base file loader: udocker and hostproc do not report a ref.
type fileOnlyRuntime struct {
	runtime.Driver
	load func(context.Context, string, string) error
}

func (d *fileOnlyRuntime) LoadImageFile(ctx context.Context, ref, path string) error {
	return d.load(ctx, ref, path)
}

func localImportNode(t *testing.T, d runtime.Driver) (*NodeRuntime, chan proto.ImageNodeStatus) {
	t.Helper()
	n := NewNodeRuntime(slog.New(slog.DiscardHandler), d, t.TempDir())
	statuses := make(chan proto.ImageNodeStatus, 4096)
	n.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses <- data.(proto.ImageNodeStatus)
		}
		return nil
	}
	return n, statuses
}

func TestImageLoadPathLifecycle(t *testing.T) {
	for _, finish := range []string{"success", "cancel", "timeout"} {
		t.Run(finish, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path, original := localArchive(t)
				base := recording.New("")
				started := make(chan context.Context, 1)
				release := make(chan struct{})
				d := &fileOnlyRuntime{Driver: base, load: func(ctx context.Context, ref, path string) error {
					started <- ctx
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
						return base.LoadImageFile(ctx, ref, path)
					}
				}}
				n, statuses := localImportNode(t, d)
				requestCtx, requestDone := context.WithCancel(context.Background())
				defer requestDone()
				if _, err := n.OnRPC(requestCtx, proto.RPCImageLoadPath, loadPathParams(path)); err != nil {
					t.Fatal(err)
				}
				defer n.cancelImageOperation(loadURLCanonical, "local-import")
				loadCtx := <-started
				requestDone()
				synctest.Wait()
				if loadCtx.Err() != nil {
					t.Fatal("ending the accepted HTTP/RPC request cancelled the import")
				}
				<-statuses // Initial loading state.
				time.Sleep(31 * time.Second)
				synctest.Wait()
				select {
				case heartbeat := <-statuses:
					if heartbeat.State != "syncing" || heartbeat.TransferID != "local-import" || heartbeat.SizeBytes != int64(len(original)) || heartbeat.TransferredBytes != 0 {
						t.Fatalf("silent runtime heartbeat = %+v", heartbeat)
					}
				default:
					t.Fatal("silent runtime stopped reporting while loading")
				}
				switch finish {
				case "success":
					close(release)
				case "cancel":
					raw, _ := json.Marshal(proto.ImageCancelParams{CanonicalRef: loadURLCanonical, TransferID: "local-import"})
					if _, err := n.OnRPC(context.Background(), proto.RPCImageCancel, raw); err != nil {
						t.Fatal(err)
					}
				case "timeout":
					time.Sleep(imageOperationTimeout)
				}
				synctest.Wait()
				var terminal proto.ImageNodeStatus
				terminals := 0
				for len(statuses) > 0 {
					st := <-statuses
					if st.State != "syncing" {
						terminal, terminals = st, terminals+1
					}
				}
				if terminals != 1 || terminal.TransferID != "local-import" {
					t.Fatalf("terminal count=%d status=%+v", terminals, terminal)
				}
				if finish == "success" {
					if terminal.State != "present" || terminal.TransferredBytes != int64(len(original)) || len(base.LoadedPaths()) != 1 {
						t.Fatalf("base file loader did not complete: %+v", terminal)
					}
				} else if terminal.State != "failed" || terminal.TransferredBytes != 0 || len(base.LoadedPaths()) != 0 {
					t.Fatalf("interrupted import reported success: %+v", terminal)
				}
				if finish == "cancel" && !strings.Contains(terminal.Message, "cancelled") {
					t.Fatalf("cancel reason lost: %+v", terminal)
				}
				if finish == "timeout" && !strings.Contains(terminal.Message, imageOperationTimeout.String()) {
					t.Fatalf("timeout reason lost: %+v", terminal)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if len(statuses) != 0 || n.cancelImageOperation(loadURLCanonical, "local-import") {
					t.Fatal("finished import retained a heartbeat or cancellation registration")
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != string(original) {
					t.Fatalf("original archive changed: %v", err)
				}
			})
		})
	}
}

func TestImageLoadPathRejectsInvalidRPCAndUnsupportedRuntime(t *testing.T) {
	path, _ := localArchive(t)
	for _, raw := range []string{"{", `{}`, `{"canonical_ref":"image@sha256:abcd","archive_path":"/tmp/image.tar"}`} {
		n, _ := localImportNode(t, recording.New(""))
		if _, err := n.OnRPC(context.Background(), proto.RPCImageLoadPath, json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid RPC: %s", raw)
		}
	}
	n, statuses := localImportNode(t, &imagePullTestDriver{})
	if _, err := n.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path)); err == nil || !strings.Contains(err.Error(), "cannot load") {
		t.Fatalf("runtime without file loading: %v", err)
	}
	if len(statuses) != 0 {
		t.Fatal("unsupported runtime started an import")
	}
}

func TestImageLoadPathKeepsQueuedTransferAlive(t *testing.T) {
	path, original := localArchive(t)
	d := recording.New("")
	n, statuses := localImportNode(t, d)
	n.imagePullMu.Lock()
	locked := true
	defer func() {
		if locked {
			n.cancelImageOperation(loadURLCanonical, "local-import")
			n.imagePullMu.Unlock()
		}
	}()
	if _, err := n.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path)); err != nil {
		t.Fatal(err)
	}
	// Mutex waits are not durably blocked in synctest; observe the real ticker.
	deadline := time.After(40 * time.Second)
	for locked {
		select {
		case st := <-statuses:
			if st.TransferID == "" { // The immediate queue notification.
				continue
			}
			if st.State != "syncing" || st.TransferID != "local-import" || st.SizeBytes != int64(len(original)) || st.TransferredBytes != 0 || len(d.LoadedPaths()) != 0 {
				t.Fatalf("queued heartbeat or premature load: %+v paths=%v", st, d.LoadedPaths())
			}
			n.imagePullMu.Unlock()
			locked = false
		case <-deadline:
			t.Fatal("queued transfer stopped reporting while waiting for the image slot")
		}
	}
	deadline = time.After(5 * time.Second)
	for {
		select {
		case st := <-statuses:
			if st.State == "syncing" {
				continue
			}
			if st.State != "present" || len(d.LoadedPaths()) != 1 {
				t.Fatalf("released transfer did not load exactly once: %+v paths=%v", st, d.LoadedPaths())
			}
			return
		case <-deadline:
			t.Fatal("released transfer never finished")
		}
	}
}

func TestImageLoadPathChecksLoadedReference(t *testing.T) {
	for _, failTag := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace existing canonical", true: "tag failure"}[failTag], func(t *testing.T) {
			h := newArchiveLoadHarness(t)
			path, original := localArchive(t)
			h.driver.InjectLoadedRef("publisher/runner:new")
			if failTag {
				h.driver.InjectTagFailure(errors.New("tag denied"))
			}
			if _, err := h.node.OnRPC(context.Background(), proto.RPCImageLoadPath, loadPathParams(path)); err != nil {
				t.Fatal(err)
			}
			status := h.awaitTerminal(t)
			want := "present"
			if failTag {
				want = "failed"
			}
			if status.State != want || !strings.Contains(strings.Join(h.driver.Calls(), "|"), "TagImage(publisher/runner:new->"+loadURLCanonical+")") {
				t.Fatalf("reference check: status=%+v calls=%v", status, h.driver.Calls())
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != string(original) {
				t.Fatalf("archive lost after tagging: %v", err)
			}
		})
	}
}
