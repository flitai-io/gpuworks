package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func imageTransferStore(t *testing.T, dsn string) (*Store, *Image, *Node) {
	t.Helper()
	s, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	image, err := s.UpsertImage(&Image{ImageRef: "registry.example/transfer:1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.RegisterNode("transfer-node", HashToken("transfer-node-token"), "transfer-node")
	if err != nil {
		t.Fatal(err)
	}
	return s, image, node
}

func TestImageTransferClaimHasOneWinnerAcrossStores(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent row", true: "failed row"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared.db")
			first, image, node := imageTransferStore(t, path)
			second, err := Open(path, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close() })
			var observed *ImageNode
			if existing {
				observed, err = first.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateFailed, LastTransferID: "previous"})
				if err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				row     *ImageNode
				started bool
				err     error
			}
			start, results := make(chan struct{}), make(chan result, 2)
			for i, s := range []*Store{first, second} {
				go func() {
					<-start
					row, started, err := s.TryStartImageTransfer(context.Background(), observed, &ImageNode{
						ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "local", TransferID: []string{"one", "two"}[i],
					})
					results <- result{row, started, err}
				}()
			}
			close(start)
			winners := 0
			var winner *ImageNode
			for range 2 {
				got := <-results
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.started {
					winners++
					winner = got.row
				} else if got.row != nil {
					t.Fatalf("a losing write returned an owned row: %+v", got.row)
				}
			}
			current, err := first.ImageNode(image.ID, node.ID)
			if err != nil || winners != 1 || winner == nil || current.TransferID != winner.TransferID || current.ID != winner.ID {
				t.Fatalf("winners=%d winner=%+v current=%+v err=%v", winners, winner, current, err)
			}
		})
	}
}

func TestImageTransferClaimRejectsChangedObservations(t *testing.T) {
	for _, change := range []string{"heartbeat", "completion", "cancel", "another attempt completed"} {
		t.Run(change, func(t *testing.T) {
			s, image, node := imageTransferStore(t, filepath.Join(t.TempDir(), "store.db"))
			observed, err := s.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "old"})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "heartbeat":
				err = s.DB().Model(&ImageNode{}).Where("id = ?", observed.ID).Update("updated_at", observed.UpdatedAt.Add(time.Second)).Error
			case "completion":
				err = s.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "old"})
			case "cancel":
				_, err = s.CancelImageNodeTransferIfActive(image.ID, node.ID, "old", "cancelled")
			case "another attempt completed":
				// Even identical state and timestamp cannot erase a new attempt's history.
				err = s.DB().Model(&ImageNode{}).Where("id = ?", observed.ID).UpdateColumn("last_transfer_id", "other").Error
			}
			if err != nil {
				t.Fatal(err)
			}
			_, started, err := s.TryStartImageTransfer(context.Background(), observed, &ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "new"})
			if err != nil || started {
				t.Fatalf("stale observation claimed a changed row: started=%v err=%v", started, err)
			}
		})
	}
}

func TestImageTransferClaimPreservesIdentityAndResetsProgress(t *testing.T) {
	s, image, node := imageTransferStore(t, filepath.Join(t.TempDir(), "store.db"))
	old, err := s.UpsertImageNode(&ImageNode{
		ImageID: image.ID, NodeID: node.ID, State: ImageStateFailed, Source: "peer", SourceNodeID: node.ID,
		ImageDigestID: "sha256:previous", LastTransferID: "previous", LastRateLimitKiB: 99,
		SizeBytes: 100, TransferredBytes: 80, RateBPS: 10, RateLimitKiB: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	next := &ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, Source: "local", TransferID: "new"}
	row, started, err := s.TryStartImageTransfer(context.Background(), old, next)
	if err != nil || !started {
		t.Fatalf("claim: started=%v err=%v", started, err)
	}
	if row.ID != old.ID || row.ImageDigestID != old.ImageDigestID || row.LastTransferID != "previous" || row.LastRateLimitKiB != 99 || row.SourceNodeID != 0 || row.TransferredBytes != 0 || row.RateBPS != 0 || row.RateLimitKiB != 0 || row.TransferID != "new" {
		t.Fatalf("retry lost identity or retained old progress: %+v", row)
	}
	if next.ID != 0 {
		t.Fatal("claim mutated the caller's input")
	}
	if err := s.FailImageTransferIfActive(image.ID, node.ID, "previous", "late RPC error"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ImageNode(image.ID, node.ID)
	if got.State != ImageStateSyncing || got.TransferID != "new" {
		t.Fatalf("old start failure replaced the retry: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, started, err := s.TryStartImageTransfer(ctx, got, next); started || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled claim: started=%v err=%v", started, err)
	}
}

func TestImageTransferClaimRollsBackWhenReadbackFails(t *testing.T) {
	s, image, node := imageTransferStore(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.DB().Callback().Query().Before("gorm:query").Register("test:claim_readback_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "ImageNode" {
			tx.AddError(errors.New("readback unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	row, started, err := s.TryStartImageTransfer(context.Background(), nil, &ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "attempt"})
	if err == nil || started || row != nil {
		t.Fatalf("failed transaction acknowledged a start: %+v %v %v", row, started, err)
	}
	if err := s.DB().Callback().Query().Remove("test:claim_readback_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImageNode(image.ID, node.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back claim left a row: %v", err)
	}
}
