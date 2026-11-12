package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// forecastRig is a cluster of named nodes with an image in the catalog and a
// model, from which the forecast of one target node is read.
type forecastRig struct {
	t      *testing.T
	st     *store.Store
	h      *Service
	online map[string]bool
	cached map[string]bool
	image  *store.Image
	model  *store.Model
}

const forecastImage = "registry.example/team/runner:v1"

func newForecastRig(t *testing.T) *forecastRig {
	t.Helper()
	st := newAcquisitionStore(t)
	rig := &forecastRig{t: t, st: st, online: map[string]bool{}, cached: map[string]bool{}}
	rig.h = New(Deps{
		Store:  st,
		Online: func(uuid string) bool { return rig.online[uuid] },
		ImageExists: func(_ context.Context, _, uuid string) (bool, error) {
			return rig.cached[uuid], nil
		},
	})
	var err error
	if rig.image, err = st.UpsertImage(&store.Image{ImageRef: forecastImage, Name: "runner", Engine: "vllm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rig.model, err = st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main"}); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *forecastRig) node(uuid string, online bool, labels map[string]string) *store.Node {
	r.t.Helper()
	node, err := r.st.RegisterNode(uuid, store.HashToken(uuid), uuid)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(labels) > 0 {
		if _, err := r.st.SetNodeLabels(uuid, labels); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid}, Network: &proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: fmt.Sprintf("192.0.2.%d", node.ID)},
	}}); err != nil {
		r.t.Fatal(err)
	}
	r.online[uuid] = online
	node, err = r.st.NodeByUUID(uuid)
	if err != nil {
		r.t.Fatal(err)
	}
	return node
}

func (r *forecastRig) holdsImage(node *store.Node) {
	r.t.Helper()
	if _, err := r.st.UpsertImageNode(&store.ImageNode{ImageVersionID: r.image.ID, NodeID: node.ID, State: store.ImageStatePresent}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *forecastRig) holdsModel(node *store.Node) {
	r.t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + node.UUID}); err != nil {
		r.t.Fatal(err)
	}
}

// forecast is the wire form of the forecast for one node.
func (r *forecastRig) forecast(uuid string) dto.AcquisitionNode {
	r.t.Helper()
	facts := r.h.evaluateNodeAcquisition(context.Background(), []string{uuid}, forecastImage, nil, r.model, store.SourceRules{}, nil)
	if len(facts) != 1 {
		r.t.Fatalf("forecast = %+v", facts)
	}
	return facts[0]
}

func (r *forecastRig) wireForecast(uuid string) string {
	r.t.Helper()
	b, err := json.Marshal(r.forecast(uuid))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *forecastRig) deleteDefaultGroup() {
	r.t.Helper()
	groups, err := r.st.SyncGroups()
	if err != nil || len(groups) == 0 {
		r.t.Fatalf("groups = %+v err=%v", groups, err)
	}
	for _, g := range groups {
		if err := r.st.DeleteSyncGroup(g.ID); err != nil {
			r.t.Fatal(err)
		}
	}
}

// A node that has the image runs it; one whose sync group has an online
// holder copies it from there; every other node pulls.
func TestForecastImagePlans(t *testing.T) {
	rig := newForecastRig(t)
	holder := rig.node("holder", true, map[string]string{"rack": "r1"})
	rig.node("same-rack", true, map[string]string{"rack": "r1"})
	rig.node("other-rack", true, map[string]string{"rack": "r2"})
	rig.holdsImage(holder)

	// The installation's default group is every node: any online holder qualifies.
	jsonEqual(t, rig.forecast("same-rack"), `{"node_uuid":"same-rack","node_name":"same-rack","image_cached":false,"image_plan":"copy","image_copy_from":"holder",
		"weights_present":false,"weights_plan":"download","weights_provider":"huggingface","weights_fetch_hosts":["huggingface.co"]}`)

	// The holder itself has it (the node's own runtime answers).
	rig.cached["holder"] = true
	if f := rig.forecast("holder"); f.ImagePlan != "present" || f.ImageCached == nil || !*f.ImageCached || f.ImageCopyFrom != "" || len(f.ImagePullRefs) != 0 {
		t.Fatalf("holder forecast = %+v", f)
	}

	// An offline holder cannot send it.
	rig.online["holder"] = false
	if f := rig.forecast("same-rack"); f.ImagePlan != "pull" || f.ImageCopyFrom != "" {
		t.Fatalf("offline holder: %+v", f)
	}
	rig.online["holder"] = true

	// A holder whose row is not "present" (still syncing, failed) is no source.
	failed := rig.node("failed-holder", true, nil)
	if _, err := rig.st.UpsertImageNode(&store.ImageNode{ImageVersionID: rig.image.ID, NodeID: failed.ID, State: store.ImageStateFailed}); err != nil {
		t.Fatal(err)
	}
	if err := rig.st.DB().Where("node_id = ?", holder.ID).Delete(&store.ImageNode{}).Error; err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("same-rack"); f.ImagePlan != "pull" {
		t.Fatalf("only a failed row holds the image: %+v", f)
	}
	rig.holdsImage(holder)

	// With the default group gone nothing is copied automatically.
	rig.deleteDefaultGroup()
	if f := rig.forecast("same-rack"); f.ImagePlan != "pull" || f.ImageCopyFrom != "" {
		t.Fatalf("no sync group: %+v", f)
	}
	// A group over rack r1 lets same-rack copy from the holder, but not other-rack.
	if _, err := rig.st.CreateSyncGroup(store.SyncGroupSpec{Name: "rack-1", Requirements: nodelabel.Requirements{"rack": {"r1"}}}); err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("same-rack"); f.ImagePlan != "copy" || f.ImageCopyFrom != "holder" {
		t.Fatalf("same rack: %+v", f)
	}
	if f := rig.forecast("other-rack"); f.ImagePlan != "pull" || f.ImageCopyFrom != "" {
		t.Fatalf("other rack shares no group with the holder: %+v", f)
	}
}

// An image the catalog does not know can only be pulled, whoever holds it.
func TestForecastUncataloguedImageIsPulled(t *testing.T) {
	rig := newForecastRig(t)
	rig.node("a", true, nil)
	facts := rig.h.evaluateNodeAcquisition(context.Background(), []string{"a"}, "unlisted.example/x/y:1", nil, nil, store.SourceRules{}, nil)
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	jsonEqual(t, facts[0], `{"node_uuid":"a","node_name":"a","image_cached":false,"image_plan":"pull","image_pull_refs":["unlisted.example/x/y:1"],"image_pull_ranked":true,"weights_present":false,"weights_plan":""}`)
	// A node the request names that does not exist is left out, not invented.
	if got := rig.h.evaluateNodeAcquisition(context.Background(), []string{"ghost", "a"}, forecastImage, nil, nil, store.SourceRules{}, nil); len(got) != 1 || got[0].NodeUUID != "a" {
		t.Fatalf("forecast with an unknown node = %+v", got)
	}
}

// The refs of a pull are the node's pull source rules applied to the image's
// copies: a rule's endpoints first, then (if the rule allows) the image's own
// registry, ranked by how fast the node reaches it. A rule forbidding the
// registry with nothing to offer leaves the node nothing to try.
func TestForecastImagePullFollowsPullSourceRules(t *testing.T) {
	rig := newForecastRig(t)
	rig.node("mainland", true, map[string]string{"pool": "cn"})
	rig.node("abroad", true, map[string]string{"pool": "us"})

	if _, err := rig.st.CreatePullSourceRule(store.PullSourceRuleSpec{
		Match: "registry.example", Endpoints: []string{"mirror.cn.example/registry"}, AllowUpstream: false,
		Requirements: nodelabel.Requirements{"pool": {"cn"}},
	}); err != nil {
		t.Fatal(err)
	}
	// The rule applies to the mainland node only.
	jsonEqual(t, json.RawMessage(rig.wireForecast("mainland")), `{"node_uuid":"mainland","node_name":"mainland","image_cached":false,"image_plan":"pull",
		"image_pull_refs":["mirror.cn.example/registry/team/runner:v1"],"image_pull_fixed":1,"weights_present":false,"weights_plan":"download","weights_provider":"huggingface","weights_fetch_hosts":["huggingface.co"]}`)
	jsonEqual(t, json.RawMessage(rig.wireForecast("abroad")), `{"node_uuid":"abroad","node_name":"abroad","image_cached":false,"image_plan":"pull",
		"image_pull_refs":["`+forecastImage+`"],"image_pull_ranked":true,"weights_present":false,"weights_plan":"download","weights_provider":"huggingface","weights_fetch_hosts":["huggingface.co"]}`)

	// Allowing the registry after the mirror keeps both, mirror first; only
	// the registry's own copy is ranked, and ranked is reported.
	rules, _ := rig.st.PullSourceRules()
	if _, err := rig.st.UpdatePullSourceRule(rules[0].ID, store.PullSourceRuleSpec{
		Match: "registry.example", Endpoints: []string{"mirror.cn.example/registry"}, AllowUpstream: true,
		Requirements: nodelabel.Requirements{"pool": {"cn"}},
	}); err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("mainland"); len(f.ImagePullRefs) != 2 || f.ImagePullRefs[0] != "mirror.cn.example/registry/team/runner:v1" || f.ImagePullRefs[1] != forecastImage || !f.ImagePullRanked || f.ImagePullFixed != 1 {
		t.Fatalf("mirror then upstream: %+v", f)
	}

	// A rule that matches, forbids the registry and offers no endpoint leaves
	// the node nothing to try; the forecast says why instead of listing refs.
	if _, err := rig.st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "registry.example", AllowUpstream: false, Requirements: nodelabel.Requirements{"pool": {"us"}}}); err != nil {
		t.Fatal(err)
	}
	f := rig.forecast("abroad")
	if f.ImagePlan != "pull" || len(f.ImagePullRefs) != 0 || f.ImagePullError == nil {
		t.Fatalf("a node with nothing to pull from: %+v", f)
	}
	var wire struct {
		ImagePullError map[string]any `json:"image_pull_error"`
	}
	if err := json.Unmarshal([]byte(rig.wireForecast("abroad")), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ImagePullError["code"] != "IMAGE_SOURCE_FORBIDDEN" ||
		fmt.Sprint(wire.ImagePullError["params"]) != "map[node:abroad route:/api/v1/pull/source-rules]" ||
		!strings.Contains(fmt.Sprint(wire.ImagePullError["message"]), "no pull source is allowed") {
		t.Fatalf("the reason is a product error the client renders: %s", rig.wireForecast("abroad"))
	}
}

// How each node gets the weights: from itself, from a node of its sync group,
// from the shared storage it reads or caches from, or by downloading.
func TestForecastWeightPlans(t *testing.T) {
	rig := newForecastRig(t)
	holder := rig.node("holder", true, nil)
	rig.node("fresh", true, nil)
	rig.holdsModel(holder)

	jsonEqual(t, json.RawMessage(rig.wireForecast("holder")), `{"node_uuid":"holder","node_name":"holder","image_cached":false,"image_plan":"pull","image_pull_refs":["`+forecastImage+`"],"image_pull_ranked":true,
		"weights_present":true,"weights_plan":"present"}`)
	jsonEqual(t, json.RawMessage(rig.wireForecast("fresh")), `{"node_uuid":"fresh","node_name":"fresh","image_cached":false,"image_plan":"pull","image_pull_refs":["`+forecastImage+`"],"image_pull_ranked":true,
		"weights_present":false,"weights_plan":"copy","weights_copy_from":"holder"}`)

	rig.online["holder"] = false
	if f := rig.forecast("fresh"); f.WeightsPlan != "download" || f.WeightsProvider != "huggingface" || f.WeightsCopyFrom != "" {
		t.Fatalf("offline holder: %+v", f)
	}
	// A download goes where the site rules say, in the order the node tries.
	mirror, err := rig.st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("fresh"); !reflect.DeepEqual(f.WeightsFetchHosts, []string{"hf-mirror.com", "huggingface.co"}) || f.WeightsFetchError != nil {
		t.Fatalf("with a mirror rule: %+v", f)
	}
	if err := rig.st.DeletePullSourceRule(mirror.ID); err != nil {
		t.Fatal(err)
	}
	forbid, err := rig.st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", AllowUpstream: false})
	if err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("fresh"); len(f.WeightsFetchHosts) != 0 || f.WeightsFetchError == nil || f.WeightsFetchError.Code != "WEIGHTS_FETCH_FORBIDDEN" || f.WeightsFetchError.Params["node"] != "fresh" {
		t.Fatalf("forbidden: %+v", f)
	}
	if err := rig.st.DeletePullSourceRule(forbid.ID); err != nil {
		t.Fatal(err)
	}
	rig.online["holder"] = true
	rig.deleteDefaultGroup()
	if f := rig.forecast("fresh"); f.WeightsPlan != "download" || f.WeightsCopyFrom != "" {
		t.Fatalf("no sync group: %+v", f)
	}

	// A shared storage the node caches from (nfs_cache): the node fills its
	// cache from it; the forecast names the storage.
	fresh, _ := rig.st.NodeByUUID("fresh")
	cache, err := rig.st.CreateStorage(&store.Storage{Name: "team-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true,
		Path: "/mnt/team-cache", CachePath: "/data/cache", Export: "nfs.example:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.st.ApplyStorageStatus(fresh.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
		ID: cache.ID, State: proto.StorageStateReady, Path: "/mnt/team-cache", CachePath: "/data/cache", Writable: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.UpsertModelFile(&store.ModelFile{ModelID: rig.model.ID, StorageID: cache.ID, Path: "org/model", State: proto.MFPresent, SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if f := rig.forecast("fresh"); f.WeightsPlan != "storage" || f.WeightsStorage != "team-cache" || f.WeightsPresent {
		t.Fatalf("cache fill from the shared storage: %+v", f)
	}
}
