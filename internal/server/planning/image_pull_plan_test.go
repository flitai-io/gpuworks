package planning

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func testImagePullStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func registerLabelled(t *testing.T, st *store.Store, uuid string, labels map[string]string) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(uuid, store.HashToken("token-"+uuid), uuid)
	if err != nil {
		t.Fatal(err)
	}
	if labels != nil {
		if node, err = st.SetNodeLabels(uuid, labels); err != nil {
			t.Fatal(err)
		}
	}
	return node
}

func candidateRefs(candidates []proto.ImagePullCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Ref)
	}
	return out
}

// A launch records the image's copies and plans the node's candidates from
// them; the catalog row also gives the digest the node holds (ADR 0031).
func TestImagePullPlanForRecordsCopiesAndPlansCandidates(t *testing.T) {
	st := testImagePullStore(t, "planning_image_pull_basic")
	node := registerLabelled(t, st, "pull-node", nil)

	unknown := &store.Deployment{ID: 1, Image: "private.example/app:v1"}
	plan, err := ImagePullPlanFor(st, nil, LaunchSourceFromDeployment(unknown), node, deploy.CatalogSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Sources, []string{"private.example/app:v1"}) || !reflect.DeepEqual(plan.Refs(), []string{"private.example/app:v1"}) || plan.DigestID != "" {
		t.Fatalf("unknown image plan = %+v", plan)
	}

	image, err := st.UpsertImage(&store.Image{
		Name: "app", ImageRef: "registry.example/app:v1", Source: "official", Enabled: true,
		RegistrySources: `["mirror.example/app:v1"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, ImageVersionID: image.ID, NodeID: node.ID, State: store.ImageStatePresent, ImageDigestID: "sha256:node-copy"}); err != nil {
		t.Fatal(err)
	}
	selection := deploy.CatalogSelection{Image: image.ImageRef, ImageSources: []string{"ghcr.io/team/app:v1"}}
	plan, err = ImagePullPlanFor(st, nil, LaunchSourceFromDeployment(&store.Deployment{ID: 2, Image: image.ImageRef}), node, selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DigestID != "sha256:node-copy" {
		t.Fatalf("digest = %q", plan.DigestID)
	}
	wantSources := []string{"ghcr.io/team/app:v1", "mirror.example/app:v1", "registry.example/app:v1"}
	if !reflect.DeepEqual(plan.Sources, wantSources) {
		t.Fatalf("sources = %v, want %v", plan.Sources, wantSources)
	}
	// With no rule every copy is the node's to rank.
	for _, candidate := range plan.Candidates {
		if !candidate.Rank {
			t.Fatalf("candidate %+v not ranked without rules", candidate)
		}
	}
	// A template's sources belong to its own image only.
	other, err := ImagePullPlanFor(st, nil, LaunchSourceFromDeployment(&store.Deployment{ID: 3, Image: image.ImageRef}), node, deploy.CatalogSelection{Image: "other/image:v1", ImageSources: []string{"ghcr.io/team/app:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(other.Sources, []string{"mirror.example/app:v1", "registry.example/app:v1"}) {
		t.Fatalf("sources of another template's image = %v", other.Sources)
	}
}

func TestPullCandidatesApplyOnlyTheRulesSelectingTheNode(t *testing.T) {
	st := testImagePullStore(t, "planning_pull_rules_for_node")
	idc := registerLabelled(t, st, "idc-node", map[string]string{"site": "idc-a"})
	office := registerLabelled(t, st, "office-node", map[string]string{"site": "office"})
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{
		Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy"}, AllowUpstream: false,
		Requirements: nodelabel.Requirements{"site": {"idc-a"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{
		Match: "docker.io", Endpoints: []string{"docker.m.daocloud.io"}, AllowUpstream: true,
		Requirements: nodelabel.Requirements{"site": {"office"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := PullCandidatesFor(st, nil, idc, "vllm/vllm-openai:v1", []string{"vllm/vllm-openai:v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateRefs(got), []string{"harbor.corp.example/dockerhub-proxy/vllm/vllm-openai:v1"}) {
		t.Fatalf("idc candidates = %v", candidateRefs(got))
	}
	got, err = PullCandidatesFor(st, nil, office, "vllm/vllm-openai:v1", []string{"vllm/vllm-openai:v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateRefs(got), []string{"docker.m.daocloud.io/vllm/vllm-openai:v1", "vllm/vllm-openai:v1"}) {
		t.Fatalf("office candidates = %v", candidateRefs(got))
	}
	// A node no rule selects pulls the image from where it is.
	plain := registerLabelled(t, st, "plain-node", nil)
	got, err = PullCandidatesFor(st, nil, plain, "vllm/vllm-openai:v1", []string{"vllm/vllm-openai:v1"})
	if err != nil || !reflect.DeepEqual(got, []proto.ImagePullCandidate{{Ref: "vllm/vllm-openai:v1", Rank: true}}) {
		t.Fatalf("plain candidates = %v, %v", got, err)
	}
	// A rule that only forbids, with nothing to offer, leaves the node nowhere
	// to pull from: that is said, not hidden.
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "ghcr.io", AllowUpstream: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := PullCandidatesFor(st, nil, plain, "ghcr.io/team/app:v1", nil); err == nil {
		t.Fatal("a node with no allowed source got candidates")
	}
}

func TestSyncPeersFollowSharedGroups(t *testing.T) {
	st := testImagePullStore(t, "planning_sync_peers")
	a1 := registerLabelled(t, st, "a1", map[string]string{"site": "idc-a"})
	a2 := registerLabelled(t, st, "a2", map[string]string{"site": "idc-a"})
	b1 := registerLabelled(t, st, "b1", map[string]string{"site": "idc-b"})
	loose := registerLabelled(t, st, "loose", nil)

	// The default group selects every node: everyone may copy from everyone.
	peers, err := SyncPeersFor(st, nil, a1, []store.Node{*a2, *b1, *loose, *a1})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 3 {
		t.Fatalf("peers under the default group = %+v", peers)
	}

	groups, _ := st.SyncGroups()
	if err := st.DeleteSyncGroup(groups[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSyncGroup(store.SyncGroupSpec{Name: "A", Requirements: nodelabel.Requirements{"site": {"idc-a"}}, RateLimitKiB: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSyncGroup(store.SyncGroupSpec{Name: "A narrow", Requirements: nodelabel.Requirements{"site": {"idc-a"}}, RateLimitKiB: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSyncGroup(store.SyncGroupSpec{Name: "B", Requirements: nodelabel.Requirements{"site": {"idc-b"}}}); err != nil {
		t.Fatal(err)
	}
	peers, err = SyncPeersFor(st, nil, a1, []store.Node{*a2, *b1, *loose})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(peers, []SyncPeer{{NodeID: a2.ID, RateLimitKiB: 100}}) {
		t.Fatalf("peers of a1 = %+v, want a2 at the strictest shared cap", peers)
	}
	// A node in no group copies from no one automatically.
	if peers, _ := SyncPeersFor(st, nil, loose, []store.Node{*a1, *a2, *b1}); len(peers) != 0 {
		t.Fatalf("peers of an ungrouped node = %+v", peers)
	}
}

func TestProviderLabelsSelectRentedNodes(t *testing.T) {
	st := testImagePullStore(t, "planning_provider_labels")
	rented := registerLabelled(t, st, "rented", nil)
	owned := registerLabelled(t, st, "owned", nil)
	provider, err := st.CreateProvider(&store.Provider{Name: "rp", Kind: "runpod", State: "valid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: provider.ID, ExternalID: "pod-1", Region: "US-VA-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkProviderInstanceNode(provider.ID, "pod-1", rented.ID); err != nil {
		t.Fatal(err)
	}
	reqs := nodelabel.Requirements{nodelabel.KeyProvider: {"runpod"}}
	facts := LoadNodeFacts(st, nil, []store.Node{*rented, *owned}, reqs)
	if err := MatchRequirements(rented, facts, reqs); err != nil {
		t.Fatalf("rented node does not match provider=runpod: %v", err)
	}
	if err := MatchRequirements(owned, facts, reqs); err == nil {
		t.Fatal("a node added by hand matched provider=runpod")
	}
	region := nodelabel.Requirements{nodelabel.KeyRegion: {"US-VA-1"}}
	if err := MatchRequirements(rented, LoadNodeFacts(st, nil, []store.Node{*rented}, region), region); err != nil {
		t.Fatalf("region does not match: %v", err)
	}
	labels := SystemLabels(rented, NodeFacts{Providers: facts.Providers})
	if got := SystemLabelValues(labels, nodelabel.KeyProvider); !reflect.DeepEqual(got, []string{"runpod"}) {
		t.Fatalf("provider label = %v", got)
	}
}

// One selection of a node's rules serves both kinds: image pulls read only the
// registry rules and model fetches only the site rules, so a site rule can
// never be mistaken for an image prefix.
func TestRulesForSelectsOnceAndEachKindReadsItsOwn(t *testing.T) {
	st := testImagePullStore(t, "planning_rules_for_kinds")
	idc := registerLabelled(t, st, "idc-node", map[string]string{"site": "idc-a"})
	office := registerLabelled(t, st, "office-node", map[string]string{"site": "office"})
	for _, spec := range []store.PullSourceRuleSpec{
		{Match: "https://huggingface.co", Endpoints: []string{"http://hf-proxy.corp:8090"}, AllowUpstream: false, Requirements: nodelabel.Requirements{"site": {"idc-a"}}},
		{Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy"}, AllowUpstream: true},
		{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true},
	} {
		if _, err := st.CreatePullSourceRule(spec); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := RulesFor(st, nil, idc)
	if err != nil || len(rules) != 3 {
		t.Fatalf("idc rules = %#v, %v", rules, err)
	}
	if image := ImageRules(rules); len(image) != 1 || image[0].Match != "docker.io" {
		t.Fatalf("image rules = %#v", image)
	}
	site := URLRules(rules)
	if len(site) != 2 || site[0].Endpoints[0] != "http://hf-proxy.corp:8090" || site[1].Endpoints[0] != "https://hf-mirror.com" {
		t.Fatalf("site rules = %#v, want both in rule order", site)
	}
	got, err := PullCandidatesWith(ImageRules(rules), idc, "vllm/vllm-openai:v1", nil)
	if err != nil || !reflect.DeepEqual(candidateRefs(got), []string{"harbor.corp.example/dockerhub-proxy/vllm/vllm-openai:v1", "vllm/vllm-openai:v1"}) {
		t.Fatalf("image candidates with site rules present = %v, %v", candidateRefs(got), err)
	}
	rules, err = RulesFor(st, nil, office)
	if err != nil || len(URLRules(rules)) != 1 || len(ImageRules(rules)) != 1 {
		t.Fatalf("office rules = %#v, %v: the idc-only site rule must not select it", rules, err)
	}
	if rules, err := RulesFor(st, nil, nil); rules != nil || err != nil {
		t.Fatalf("no node selects no rule: %#v, %v", rules, err)
	}
}

// Sharing a sync group is not a way to copy: a peer the target shares no
// storage-sync network with is dropped, and a peer on a storage-sync network
// of their own goes ahead of one reached over the management network.
func TestWithStoragePathsKeepsReachablePeersOwnNetworkFirst(t *testing.T) {
	st := testImagePullStore(t, "planning_storage_paths")
	node := func(uuid, ip string, rolesOff ...string) *store.Node {
		t.Helper()
		registerLabelled(t, st, uuid, nil)
		if ip != "" {
			if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid}, Network: &proto.NetInventory{
				Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
			}}); err != nil {
				t.Fatal(err)
			}
		}
		if len(rolesOff) > 0 {
			if err := st.SetNodeManagementRolesOff(uuid, rolesOff); err != nil {
				t.Fatal(err)
			}
		}
		n, err := st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	target := node("target", "10.0.0.1")
	mgmt := node("mgmt", "10.0.0.2")
	fabric := node("fabric", "10.0.0.3")
	closed := node("closed", "10.0.0.4", netplan.RoleStorageSync)
	silent := node("silent", "")
	sources := []store.Node{*mgmt, *fabric, *closed, *silent}
	peers := []SyncPeer{{NodeID: mgmt.ID, RateLimitKiB: 100}, {NodeID: fabric.ID}, {NodeID: closed.ID}, {NodeID: silent.ID}}

	storage := netplan.Network{ID: 7, Name: "storage"}
	resolve := func([]store.Node) (netplan.Resolution, error) {
		return netplan.Resolution{
			{NodeID: target.ID, Role: netplan.RoleStorageSync, Network: storage, IP: "172.16.0.1"},
			{NodeID: fabric.ID, Role: netplan.RoleStorageSync, Network: storage, IP: "172.16.0.3"},
		}, nil
	}
	got, err := WithStoragePaths(resolve, target, sources, peers)
	if err != nil {
		t.Fatal(err)
	}
	want := []SyncPeer{{NodeID: fabric.ID, Network: "storage"}, {NodeID: mgmt.ID, RateLimitKiB: 100, Network: netplan.ManagementName}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peers = %+v, want %+v (no path to a node that turned storage sync off or reported no address)", got, want)
	}

	// A target that turned storage sync off on the management network keeps
	// only the peer on its storage network.
	if err := st.SetNodeManagementRolesOff("target", []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	target, _ = st.NodeByUUID("target")
	got, err = WithStoragePaths(resolve, target, sources, peers)
	if err != nil || !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("peers = %+v err=%v, want only the storage network peer", got, err)
	}

	// Networks that cannot be resolved are an error, not an empty answer.
	failing := func([]store.Node) (netplan.Resolution, error) { return nil, errors.New("catalog unreadable") }
	if _, err := WithStoragePaths(failing, target, sources, peers); err == nil {
		t.Fatal("an unreadable network catalog was read as no peers")
	}
	if got, err := WithStoragePaths(resolve, target, sources, nil); err != nil || len(got) != 0 {
		t.Fatalf("no peers in: %+v %v", got, err)
	}
}
