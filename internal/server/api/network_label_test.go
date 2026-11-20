package api

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEditNetworkAttachmentLabel(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	const targets = "/api/v1/networks/1/attachments"
	const edit = targets + "/1/label"
	const newLabel = `{"label_key":" site ","label_value":" west "}`
	original := n.expect(n.admin, http.MethodPost, targets, `{"label_key":"pool","label_value":"a"}`, http.StatusCreated)["attachments"].([]any)[0].(map[string]any)
	n.expect(n.admin, http.MethodPost, targets, `{"label_key":"pool","label_value":"b"}`, http.StatusCreated)
	n.expect(n.member, http.MethodPut, edit, newLabel, http.StatusForbidden)
	n.expectCode(http.MethodPut, edit, `{"label_key":"bad key"}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPut, edit, `{"label_key":"pool","label_value":"b"}`, http.StatusConflict, ErrorNetworkAttachmentExists)
	n.expectCode(http.MethodPut, "/api/v1/networks/2/attachments/1/label", newLabel, http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	unchanged := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["attachments"].([]any)[0]
	if !reflect.DeepEqual(unchanged, original) {
		t.Fatal("rejected update changed the original attachment")
	}
	original["label_key"], original["label_value"] = "site", "west"
	for range 2 {
		result := n.expect(n.admin, http.MethodPut, edit, newLabel, http.StatusOK)["attachments"].([]any)
		if len(result) != 2 || !reflect.DeepEqual(result[0], original) {
			t.Fatalf("label update changed attachment identity, precedence or metadata: %v", result)
		}
	}
}

func TestNetworkDetachDirectFallsBackToMatchingLabel(t *testing.T) {
	n := newNetworkAPI(t)
	const fabric = "/api/v1/networks/1"
	const serving = "/api/v1/networks/2"
	const uuid = "gpu-overlap"
	if _, err := n.store.RegisterNode(uuid, store.HashToken("local"), uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.SetNodeLabels(uuid, map[string]string{"pool": "training"}); err != nil {
		t.Fatal(err)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"serving","roles":["serving"],"type":"host_ethernet"}`, http.StatusCreated)
	other := n.expect(n.admin, http.MethodPost, serving+"/attachments", `{"node_uuid":"gpu-overlap"}`, http.StatusCreated)
	label := n.expect(n.admin, http.MethodPost, fabric+"/attachments", `{"label_key":"pool","label_value":"training"}`, http.StatusCreated)
	both := n.expect(n.admin, http.MethodPost, fabric+"/attachments", `{"node_uuid":"gpu-overlap","ip":"192.0.2.19"}`, http.StatusCreated)

	if both["resolved_node_count"] != float64(1) || len(both["nodes"].([]any)) != 1 {
		t.Fatalf("direct and label matches must resolve to one node: %v", both["nodes"])
	}
	row := both["nodes"].([]any)[0].(map[string]any)
	if row["node_uuid"] != uuid || row["source"] != "node" || row["ip"] != "192.0.2.19" || row["bootstrap_source"] != "manual" {
		t.Fatalf("direct override did not win: %v", row)
	}
	attachment := fmt.Sprintf("/attachments/%.0f", row["attachment_id"])
	n.expectCode(http.MethodDelete, serving+attachment, "", http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	result := n.expect(n.admin, http.MethodDelete, fabric+attachment, "", http.StatusOK)
	if result["resolved_node_count"] != float64(1) || !reflect.DeepEqual(result["attachments"], label["attachments"]) || !reflect.DeepEqual(result["nodes"], label["nodes"]) {
		t.Fatalf("expected original label and auto-resolved node after detach: attachments=%v nodes=%v", result["attachments"], result["nodes"])
	}

	for source, want := range map[string][]any{"node": {}, "label": {uuid}} {
		ids := n.expect(n.member, http.MethodGet, fabric+"/search?kind=nodes&role=compute&source="+source, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("persisted source %q matched %v, want %v", source, ids, want)
		}
	}
	preserved := n.expect(n.member, http.MethodGet, serving, "", http.StatusOK)
	if !reflect.DeepEqual(preserved["attachments"], other["attachments"]) || !reflect.DeepEqual(preserved["nodes"], other["nodes"]) {
		t.Fatalf("detaching from fabric changed serving network: %v", preserved)
	}
}
