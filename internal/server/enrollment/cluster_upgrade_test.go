package enrollment_test

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// The cluster upgrade command of a separately built server reads this answer
// and names these steps. The wire names are pinned literally.
func TestClusterUpgradeCommandReadsTheStepAnswer(t *testing.T) {
	for _, step := range []string{enrollment.ClusterUpgradeStatus, enrollment.ClusterUpgradeBegin, enrollment.ClusterUpgradeMigrate, enrollment.ClusterUpgradeFinish, enrollment.ClusterUpgradeAbort, enrollment.ClusterUpgradeLead} {
		if !enrollment.ValidClusterUpgradeAction(step) {
			t.Fatalf("step %q is not accepted", step)
		}
	}
	if enrollment.ClusterUpgradeLead != "lead" {
		t.Fatalf("the handover step is named %q on the wire", enrollment.ClusterUpgradeLead)
	}
	if enrollment.ValidClusterUpgradeAction("restart") || enrollment.ClusterUpgradePath != "/_cluster/upgrade" {
		t.Fatal("the step names or the path changed")
	}
	wire := `{"action":"migrate","leader":true,"phase":"upgrading","phase_revision":5,"applied":245,"known":246,"applied_extensions":5,"known_extensions":6,"applied_by_extension":{"license":5},"base":245,"ready":true,"migrating":true,"members":[{"address":"192.0.2.1:9001","reported":true,"markers":246,"last":"drop_node_placement_policy","extensions":6,"fresh":true}],"agent_connections":3,"storage_tier":"slow-storage@100ms","recorded_storage_tier":"slow-storage@200ms","refused":"r","refused_code":"single_member","error":"e"}`
	var response enrollment.ClusterUpgradeResponse
	if err := json.Unmarshal([]byte(wire), &response); err != nil {
		t.Fatal(err)
	}
	if response.Action != "migrate" || !response.Leader || response.Phase != "upgrading" || response.PhaseRevision != 5 ||
		response.Applied != 245 || response.Known != 246 || response.AppliedExtensions != 5 || response.KnownExtensions != 6 ||
		len(response.AppliedByExtension) != 1 || response.AppliedByExtension["license"] != 5 ||
		response.AgentConnections == nil || *response.AgentConnections != 3 || response.RefusedCode != "single_member" || response.StorageTier != "slow-storage@100ms" || response.RecordedStorageTier != "slow-storage@200ms" ||
		response.Base != 245 || !response.Ready || !response.Migrating || response.Refused != "r" || response.Error != "e" ||
		len(response.Members) != 1 || response.Members[0] != (enrollment.ClusterUpgradeMember{Address: "192.0.2.1:9001", Reported: true, Markers: 246, Last: "drop_node_placement_policy", Extensions: 6, Fresh: true}) {
		t.Fatalf("decoded %+v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil || string(encoded) != wire {
		t.Fatalf("round trip = %s, %v\nwant        %s", encoded, err, wire)
	}
	quiet, err := json.Marshal(enrollment.ClusterUpgradeResponse{Action: "status"})
	if err != nil || string(quiet) != `{"action":"status","leader":false,"phase":"","phase_revision":0,"applied":0,"known":0,"applied_extensions":0,"known_extensions":0,"base":0,"ready":false,"migrating":false,"members":null}` {
		t.Fatalf("an empty answer = %s, %v", quiet, err)
	}
	// A member that holds no agent connection says zero; one that cannot
	// say leaves the field out, and a reader tells the two apart.
	none := 0
	zero, err := json.Marshal(enrollment.ClusterUpgradeResponse{Action: "status", AgentConnections: &none})
	if err != nil || !strings.Contains(string(zero), `"agent_connections":0`) {
		t.Fatalf("a member without agent connections = %s, %v", zero, err)
	}
	var old enrollment.ClusterUpgradeResponse
	if err := json.Unmarshal(quiet, &old); err != nil || old.AgentConnections != nil {
		t.Fatalf("an answer without the count decoded as %v, %v; want it unknown", old.AgentConnections, err)
	}
	// A refusal that is only reported, and every earlier release's, has
	// no code.
	if old.RefusedCode != "" || strings.Contains(string(quiet), "refused_code") {
		t.Fatalf("an answer without a refusal code decoded as %q: %s", old.RefusedCode, quiet)
	}
	// A member whose program does not say which tier it runs leaves the
	// field out (the quiet answer above has none); it is never guessed.
	if old.StorageTier != "" || old.RecordedStorageTier != "" || strings.Contains(string(quiet), "storage_tier") {
		t.Fatalf("an answer without the tier decoded as %q", old.StorageTier)
	}
}
