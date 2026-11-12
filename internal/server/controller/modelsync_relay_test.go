package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncRelayPermissionAndTransportEvidence(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "direct only"}[disabled], func(t *testing.T) {
			st, model, source, target, _ := setupModelSyncController(t)
			if disabled {
				target.ManagementRolesOff = `["storage_sync"]`
			}
			const transfer = "relay-evidence"
			if err := st.BeginModelSync(store.FileRef{ModelID: model.ID, NodeID: target.ID}, source.ID, transfer, 12); err != nil {
				t.Fatal(err)
			}
			mf, _ := st.ModelFileByModelNode(model.ID, target.ID)
			var logs bytes.Buffer
			transport := "relay"
			if disabled {
				transport = "direct"
			}
			hub := &modelSyncHub{copyResult: &proto.ModelSyncCopyResult{TransferID: transfer, Transport: transport}}
			c := New(st, hub, slog.New(slog.NewJSONHandler(&logs, nil)))
			key := store.ModelFileKey(mf.ID)
			run := &modelSyncRun{transferID: transfer, cancel: func() {}}
			c.modelSyncs[key] = run
			c.runModelSync(context.Background(), key, run, peerSource{node: source, path: "/models/Qwen3", sizeBytes: 12}, target, mf, netplan.StoragePath{})
			c.clearModelSync(key, run)
			if len(hub.requests) != 1 || hub.requests[0].AllowRelay == disabled {
				t.Fatalf("requests=%+v disabled=%v", hub.requests, disabled)
			}
			var record map[string]any
			for _, line := range strings.Split(logs.String(), "\n") {
				var item map[string]any
				if json.Unmarshal([]byte(line), &item) == nil && item["msg"] == "peer model sync peer-confirmed" {
					record = item
				}
			}
			if record["transport"] != transport {
				t.Fatalf("transport evidence missing: %s", logs.String())
			}
		})
	}
}
