//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

const compsharePDDeploymentTimeout = 75 * time.Minute

// TestCompsharePDDeployChatTwoVMDocker is the cross-node Ethernet PD contract
// (ADR 0028) on two rented CompShare VMs: a prefill instance on VM1 and a
// decode instance on VM2, KV over the cloud private network. It proves the
// deployment starts through the platform, the server's own handoff check
// records a TCP transfer, a chat through /v1 is decoded from transferred KV,
// and the gateway falls back to the decoder when the prefill engine is gone.
func TestCompsharePDDeployChatTwoVMDocker(t *testing.T) {
	runPDDeployChatTwoVMDocker(t, compshareBackend(t))
}

// runPDDeployChatTwoVMDocker is the provider-neutral body of the case.
func runPDDeployChatTwoVMDocker(t *testing.T, b *vmBackend) {
	harness := newHostedControlPlane(t, hostedControlPlaneConfig{
		RunID: cloudTestRunID(), RuntimeDrivers: [2]string{"docker", "docker"},
		Backend: b,
		PrepareVMs: func(t *testing.T, envs []*cloudTestEnv) {
			b.EnsureDocker(t, envs)
			b.EnsureImage(t, envs)
		},
	})
	model := hostedPrepareRealModel(t, harness)
	prefillNode, decodeNode := harness.Nodes[0], harness.Nodes[1]
	body := map[string]any{
		"model_id": model.ID, "served_name": b.servedName("pd"),
		"engine": "vllm", "image": b.DistributedImage,
		"quick_params": map[string]any{"max_model_len": 4096},
		"placement": map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
			"prefill": map[string]any{"tp": 1, "units": []any{map[string]any{"node_uuid": prefillNode.UUID, "gpus": []int{0}}}},
			"decode":  map[string]any{"tp": 1, "units": []any{map[string]any{"node_uuid": decodeNode.UUID, "gpus": []int{0}}}},
		}}},
	}
	d := createHostedDeployment(t, harness, body, 2, compsharePDDeploymentTimeout)

	var prefill, decode dto.Instance
	for _, inst := range d.instances {
		switch inst.Role {
		case "prefill":
			prefill = inst
		case "decode":
			decode = inst
		}
	}
	if prefill.NodeUUID != prefillNode.UUID || decode.NodeUUID != decodeNode.UUID {
		t.Fatalf("PD instances placed on prefill=%s decode=%s, want %s and %s", prefill.NodeUUID, decode.NodeUUID, prefillNode.UUID, decodeNode.UUID)
	}

	verified := hostedWaitPDHandoff(t, harness, prefill.UUID, 10*time.Minute)
	if verified.Code != "PD_HANDOFF_VERIFIED" || verified.Params["transport"] != "tcp" {
		t.Fatalf("handoff check on the private network = %s %v, want PD_HANDOFF_VERIFIED over tcp", verified.Code, verified.Params)
	}

	decodeVM := harness.VMs[1]
	promptSource := func(source string) float64 {
		cmd := fmt.Sprintf(`curl -fsS http://127.0.0.1:%d/metrics | awk '/^vllm:prompt_tokens_by_source_total/ && /source="%s"/ {s += $NF} END {print s+0}'`, decode.APIPort, source)
		out, err := runCloudCommand(t.Context(), decodeVM.Instance, cmd)
		if err != nil {
			t.Fatalf("read decode metrics: %v\n%s", err, out)
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(tailLine(out)), 64)
		if err != nil {
			t.Fatalf("decode metric %s = %q", source, out)
		}
		return value
	}
	external := promptSource("external_kv_transfer")
	d.chat(t)
	if after := promptSource("external_kv_transfer"); after <= external {
		t.Fatalf("decode external KV tokens %v -> %v: the chat was not prefilled remotely", external, after)
	}

	// The prefill engine goes away: the gateway sends the request to the
	// decoder alone, which computes the prompt itself.
	stop := fmt.Sprintf(`set -eu; c=$(docker ps --format '{{.Names}}' | grep -F %s | head -n1); test -n "$c"; docker stop -t 5 "$c"`, shellQuoteForTest("gpuworks-"+prefill.UUID[:12]))
	if out, err := runCloudCommand(t.Context(), harness.VMs[0].Instance, stop); err != nil {
		t.Fatalf("stop the prefill engine: %v\n%s", err, out)
	}
	local := promptSource("local_compute")
	d.chat(t)
	if after := promptSource("local_compute"); after <= local {
		t.Fatalf("decode local prompt tokens %v -> %v: the fallback request did not reach the decoder", local, after)
	}
	d.stop(t)

	// The same PD deployment again, started from one manual script shared by
	// both roles: each instance must run its own role's branch, and the KV
	// still has to cross from the prefill instance to the decoder.
	manual := hostedCreateWithManualDraft(t, harness, body, b.servedName("pd-manual"),
		"role=${GPUWORKS_PD_ROLE}", 2, compsharePDDeploymentTimeout)
	var manualPrefill dto.Instance
	for _, inst := range manual.instances {
		vm := harness.VMs[1]
		if inst.Role == "prefill" {
			manualPrefill, vm = inst, harness.VMs[0]
		}
		// The script holds both roles' branches; the expanded case subject
		// is what selects this instance's.
		assertManualCommandRan(t, vm, inst, "role="+inst.Role, fmt.Sprintf("case %q in", inst.Role+"/0"))
	}
	if verified := hostedWaitPDHandoff(t, harness, manualPrefill.UUID, 10*time.Minute); verified.Code != "PD_HANDOFF_VERIFIED" {
		t.Fatalf("manual PD handoff check = %s %v, want PD_HANDOFF_VERIFIED", verified.Code, verified.Params)
	}
	manual.chat(t)
	manual.stop(t)
}

// hostedWaitPDHandoff waits for the server's handoff check on a prefill
// instance and returns its timeline entry.
func hostedWaitPDHandoff(t *testing.T, harness *hostedControlPlane, uuid string, timeout time.Duration) dto.InstanceEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var events apiwire.CursorPage[dto.InstanceEvent]
		if err := harness.API.GetJSON(t.Context(), "/api/v1/instances/"+uuid+"/events", &events); err == nil {
			for _, event := range events.Items {
				if strings.HasPrefix(event.Code, "PD_HANDOFF_") {
					return event
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("no PD handoff check recorded for %s within %s", uuid, timeout)
	return dto.InstanceEvent{}
}
