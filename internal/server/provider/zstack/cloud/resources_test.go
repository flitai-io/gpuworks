package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
)

func TestApplyResourceMutationUpdatesCloudReservationAndDisablesGPU(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Fatal("missing Cloud authorization")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/api-jobs/resource-update" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/resource-configurations/host-1/kvm/reservedCpu" {
			_, _ = w.Write([]byte(`{"value":"4"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/resource-configurations/host-1/kvm/reservedMemory" {
			_, _ = w.Write([]byte(`{"value":"8G"}`))
			return
		}
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, map[string]any{"path": r.URL.Path, "body": body})
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"location":"/v1/api-jobs/resource-update"}`))
	}))
	defer server.Close()
	driver := New()
	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	if err := driver.ApplyResourceMutation(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1", Delta: 2}); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyResourceMutation(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "memory", ResourceID: "host-1", Delta: 2 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyResourceMutation(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "gpu", ResourceID: "gpu-1", DesiredState: "Disabled"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("PUT requests = %d, want 3", len(requests))
	}
	cpu := requests[0]["body"].(map[string]any)["updateResourceConfig"].(map[string]any)
	if cpu["value"] != "6" || requests[0]["path"] != "/v1/resource-configurations/kvm/reservedCpu/host-1/actions" {
		t.Fatalf("CPU update = %#v", requests[0])
	}
	memory := requests[1]["body"].(map[string]any)["updateResourceConfig"].(map[string]any)
	if memory["value"] != "10G" || requests[1]["path"] != "/v1/resource-configurations/kvm/reservedMemory/host-1/actions" {
		t.Fatalf("memory update = %#v", requests[1])
	}
	gpu := requests[2]["body"].(map[string]any)["updatePciDevice"].(map[string]any)
	if gpu["state"] != "Disabled" || requests[2]["path"] != "/v1/pci-device/pci-devices/gpu-1/actions" {
		t.Fatalf("GPU update = %#v", requests[2])
	}
}

func TestInspectResourceReadsCurrentCloudState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Fatal("missing Cloud authorization")
		}
		switch r.URL.Path {
		case "/v1/resource-configurations/host-1/kvm/reservedCpu":
			_, _ = w.Write([]byte(`{"value":"6"}`))
		case "/v1/resource-configurations/host-1/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"10G"}`))
		case "/v1/gpu-device/gpu-devices":
			if r.URL.Query().Get("q") != "uuid=gpu-1" {
				t.Errorf("inspection must query its GPU: %s", r.URL)
			}
			_, _ = w.Write([]byte(`{"inventories":[{"uuid":"gpu-1","state":"Disabled","description":"operator note"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	driver := New()
	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	cpu, err := driver.InspectResource(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1"})
	if err != nil || cpu.Amount != 6 {
		t.Fatalf("CPU observation = %+v, err=%v", cpu, err)
	}
	memory, err := driver.InspectResource(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "memory", ResourceID: "host-1"})
	if err != nil || memory.Amount != 10<<30 {
		t.Fatalf("memory observation = %+v, err=%v", memory, err)
	}
	gpu, err := driver.InspectResource(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "gpu", ResourceID: "gpu-1"})
	if err != nil || gpu.State != "Disabled" || gpu.Description != "operator note" {
		t.Fatalf("GPU observation = %+v, err=%v", gpu, err)
	}
}

func TestInspectResourceRejectsUnexpectedGPUIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"inventories":[{"uuid":"other-gpu","state":"Disabled","description":"another operator's note"}]}`))
	}))
	defer server.Close()
	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	if _, err := New().InspectResource(t.Context(), creds, resourceclaim.ResourceMutation{ResourceType: "gpu", ResourceID: "gpu-1"}); err == nil {
		t.Fatal("inspection accepted another GPU's state and description")
	}
}

func TestCPUReservationToleratesMissingResourceConfig(t *testing.T) {
	var putCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Fatal("missing Cloud authorization")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/resource-configurations/host-1/kvm/reservedCpu" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":"false","diagnostic":{"code":"20007","message":"global configuration [category:kvm, name:kvm.reservedCpu] cannot bind to resource"}}`))
			return
		}
		if r.Method == http.MethodPut {
			putCalls++
		}
		http.Error(w, "unexpected request", http.StatusNotFound)
	}))
	defer server.Close()

	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	if err := New().ApplyResourceMutation(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1", Delta: 2}); err != nil {
		t.Fatalf("CPU reservation should be skipped when ResourceConfig is unavailable: %v", err)
	}
	observation, err := New().InspectResource(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1"})
	if err != nil {
		t.Fatalf("CPU observation should fall back to zero: %v", err)
	}
	if observation.Amount != 0 {
		t.Fatalf("CPU observation = %d, want 0", observation.Amount)
	}
	if putCalls != 0 {
		t.Fatalf("PUT calls = %d, want 0", putCalls)
	}
}

func TestCPUReservationInspectionUsesGlobalFallback(t *testing.T) {
	var putCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/resource-configurations/host-1/kvm/reservedCpu" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":"false","diagnostic":{"code":"20007"},"error":{"code":"SYS.1007","globalErrorCode":"ORG_ZSTACK_RESOURCECONFIG_10007"}}`))
			return
		}
		if r.URL.Path == "/v1/global-configurations" {
			_, _ = w.Write([]byte(`{"inventories":[{"name":"reservedCpu","category":"kvm","value":"4","defaultValue":"0"}]}`))
			return
		}
		if r.Method == http.MethodPut {
			putCalls++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	if err := New().ApplyResourceMutation(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1", Delta: 2}); err != nil {
		t.Fatalf("CPU mutation should tolerate a host-level config miss: %v", err)
	}
	if putCalls != 0 {
		t.Fatalf("PUT calls = %d, want 0 for a shared global baseline", putCalls)
	}
	observation, err := New().InspectResource(context.Background(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host-1"})
	if err != nil {
		t.Fatalf("CPU observation should use the global fallback: %v", err)
	}
	if observation.Amount != 4 {
		t.Fatalf("CPU observation = %d, want 4", observation.Amount)
	}
}
