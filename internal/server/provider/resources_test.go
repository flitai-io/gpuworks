package provider

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestEveryKnownKindDeclaresTheResourcesItConnects(t *testing.T) {
	valid := map[Resource]bool{ResourceVM: true, ResourceContainer: true, ResourceHost: true, ResourceKubernetes: true}
	for _, kind := range knownProviderKinds {
		resources := ResourcesOf(kind)
		if len(resources) == 0 {
			t.Errorf("provider kind %q declares no resource", kind)
		}
		seen := map[Resource]bool{}
		for i, r := range resources {
			if !valid[r.Type] || seen[r.Type] {
				t.Errorf("provider kind %q resource %q is unknown or repeated", kind, r.Type)
			}
			seen[r.Type] = true
			// Instances come before clusters, the order the console lists them in.
			if r.Type == ResourceKubernetes && i != len(resources)-1 {
				t.Errorf("provider kind %q lists Kubernetes before an instance resource", kind)
			}
		}
	}
	for kind := range kindResources {
		if _, known := kindCategories[kind]; !known {
			t.Errorf("resources are declared for %q, which is not a known kind", kind)
		}
	}
	// A caller's copy is its own: changing it does not change the dictionary.
	got := ResourcesOf("aliyun")
	got[0].Product = "changed"
	if again := ResourcesOf("aliyun"); again[0].Product != "ECS" {
		t.Fatalf("ResourcesOf hands out the dictionary itself: %+v", again)
	}
	if raw, _ := json.Marshal(ResourcesOf("aliyun")); string(raw) != `[{"type":"vm","product":"ECS"},{"type":"kubernetes","product":"ACK"}]` {
		t.Fatalf("aliyun resources on the wire = %s", raw)
	}
	if raw, _ := json.Marshal(ResourcesOf("zstack")); string(raw) != `[{"type":"host"}]` {
		t.Fatalf("zstack resources on the wire = %s", raw)
	}
	if raw, _ := json.Marshal(ResourcesOf("extension-provider")); string(raw) != `[]` {
		t.Fatalf("an undeclared kind's resources on the wire = %s", raw)
	}
}

func TestInstanceResourceFollowsTheInstanceKindThenTheOnlyChoice(t *testing.T) {
	for _, c := range []struct {
		kind, instanceKind string
		want               Resource
	}{
		{"aliyun", "vm", ResourceVM},
		{"aliyun", "", ResourceVM}, // one instance resource: nothing to tell apart
		{"compshare", "vm", ResourceVM},
		{"compshare", "container", ResourceContainer},
		{"compshare", "", ""}, // two instance resources and no word from the instance
		{"runpod", "container", ResourceContainer},
		{"zstack", "", ResourceHost},
		{"zstack", "vm", ResourceHost},
		{"extension-provider", "vm", ""},
	} {
		if got := InstanceResource(c.kind, c.instanceKind); got != c.want {
			t.Errorf("InstanceResource(%q, %q) = %q, want %q", c.kind, c.instanceKind, got, c.want)
		}
	}
}

func TestEveryClusterTypeHasANameInEachLanguage(t *testing.T) {
	for _, clusterType := range knownClusterTypes {
		if !providerTextComplete(ClusterTypeLabel(clusterType)) {
			t.Errorf("cluster type %q has incomplete labels: %#v", clusterType, ClusterTypeLabel(clusterType))
		}
	}
	if got := ClusterTypeLabel(ClusterTypeACKManaged); got[localization.English] != "ACK managed" || got[localization.Chinese] != "ACK 托管版" {
		t.Fatalf("ACK managed label = %#v", got)
	}
	// A type the dictionary does not know has no name, rather than a made-up one.
	if got := ClusterTypeLabel("ack_future"); got != nil {
		t.Fatalf("unknown cluster type label = %#v", got)
	}
	if got := ClusterTypeLabel(""); got != nil {
		t.Fatalf("empty cluster type label = %#v", got)
	}
}
