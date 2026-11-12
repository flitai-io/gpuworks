package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncFailureDiagnosticNamesAttemptEndpoints(t *testing.T) {
	st, model, src, dst, _ := setupModelSyncController(t)
	const transfer = "peer-failure-evidence"
	if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID, transfer, 12); err != nil {
		t.Fatal(err)
	}
	mf, _ := st.ModelFileByModelNode(model.ID, dst.ID)
	var logs bytes.Buffer
	c := New(st, &modelSyncHub{copyErr: errors.New("send peer model archive: target reset")}, slog.New(slog.NewJSONHandler(&logs, nil)))
	key := store.ModelFileKey(mf.ID)
	run := &modelSyncRun{transferID: transfer, cancel: func() {}}
	c.modelSyncs[key] = run
	c.runModelSync(context.Background(), key, run, peerSource{node: src, path: "/models/Qwen3", sizeBytes: 12}, dst, mf, netplan.StoragePath{SourceIP: "192.0.2.10", TargetIP: "192.0.2.20"})
	var record map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) == nil && item["msg"] == "peer model sync failed" {
			record = item
		}
	}
	if record["transfer"] != transfer || record["source_node"] != src.UUID || record["target_node"] != dst.UUID || record["source_ip"] != "192.0.2.10" || record["target_ip"] != "192.0.2.20" || record["observation_scope"] != "control_plane" {
		t.Fatalf("peer failure missing original path: %v", record)
	}
	if !strings.Contains(record["err"].(string), "target reset") {
		t.Fatalf("cause lost: %v", record)
	}
	got, err := st.ModelFileByModelNode(model.ID, dst.ID)
	if err != nil || got.State != "download_required" {
		t.Fatalf("failure behavior changed: %+v %v", got, err)
	}
}

func TestModelSyncConfirmationTimeoutKeepsAttemptEndpoints(t *testing.T) {
	for _, state := range []string{"active", "replaced", "completed"} {
		t.Run(state, func(t *testing.T) {
			st, model, src, dst, _ := setupModelSyncController(t)
			const transfer = "peer-confirmation-evidence"
			if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: dst.ID}, src.ID, transfer, 12); err != nil {
				t.Fatal(err)
			}
			mf, _ := st.ModelFileByModelNode(model.ID, dst.ID)
			var logs bytes.Buffer
			c := New(st, &modelSyncHub{}, slog.New(slog.NewJSONHandler(&logs, nil)))
			key := store.ModelFileKey(mf.ID)
			run := &modelSyncRun{transferID: transfer, cancel: func() {}}
			c.modelSyncs[key] = run
			if state == "replaced" {
				c.modelSyncs[key] = &modelSyncRun{transferID: "new-owner", cancel: func() {}}
			}
			if state == "completed" {
				if err := st.UpdateModelFileStatus(dst.ID, proto.ModelFileStatus{Key: key, State: proto.MFPresent, Path: mf.Path, BytesTotal: 12, BytesDone: 12, TransferID: transfer, Acquisition: proto.ModelAcquirePeer}); err != nil {
					t.Fatal(err)
				}
			}
			// Later reported network changes must not overwrite this attempt's path.
			reportManagementAddress(t, st, src.UUID, "198.51.100.1")
			c.expireModelSyncConfirmation(key, run, mf, peerSource{node: src}, dst, netplan.StoragePath{SourceIP: "192.0.2.10", TargetIP: "192.0.2.20"})
			var record map[string]any
			for _, line := range strings.Split(logs.String(), "\n") {
				var item map[string]any
				if json.Unmarshal([]byte(line), &item) == nil && item["msg"] == "model sync target confirmation timed out" {
					record = item
				}
			}
			if state != "active" {
				if record != nil {
					t.Fatalf("inactive/completed attempt reported failure: %v", record)
				}
				return
			}
			if record["transfer"] != transfer || record["source_ip"] != "192.0.2.10" || record["target_ip"] != "192.0.2.20" || record["source_node"] != src.UUID || record["target_node"] != dst.UUID {
				t.Fatalf("confirmation evidence lost attempt path: %v", record)
			}
		})
	}
}
