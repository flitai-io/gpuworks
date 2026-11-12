package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSweepRemovesOnlyNodesTheirClusterNoLongerHas(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := st.CreateKubeCluster(store.KubeCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(name, state string, seen time.Time) store.Node {
		n, _, err := st.EnrollKubeNode(c.Token, "uid", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{"state": state, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
		return *n
	}
	now := time.Now()
	peer := enroll("peer", store.NodeStateOnline, now)
	gone := enroll("gone", store.NodeStateOffline, now.Add(-time.Hour))
	stillThere := enroll("rebooting", store.NodeStateOffline, now.Add(-time.Hour))
	recent := enroll("blip", store.NodeStateOffline, now)
	unselected := enroll("unselected", store.NodeStateOffline, now.Add(-time.Hour))
	oldPeer := enroll("old-peer-answer", store.NodeStateOffline, now.Add(-time.Hour))
	var asked []string
	call := func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if uuid != peer.UUID || method != proto.RPCKubeNodeExists {
			t.Fatalf("asked %s %s", uuid, method)
		}
		name := params.(proto.KubeNodeExistsParams).NodeName
		asked = append(asked, name)
		no, yes := false, true
		switch name {
		case "gone":
			return json.Marshal(proto.KubeNodeExistsResult{})
		case "unselected":
			return json.Marshal(proto.KubeNodeExistsResult{Exists: true, Agent: &no})
		case "old-peer-answer":
			return json.RawMessage(`{"exists":true}`), nil // an agent that predates the agent field
		}
		return json.Marshal(proto.KubeNodeExistsResult{Exists: true, Agent: &yes})
	}
	var kicked []string
	sweepGoneKubeNodes(context.Background(), slog.New(slog.DiscardHandler), st, call, func(u string) { kicked = append(kicked, u) }, now.Add(-15*time.Minute))
	if len(asked) != 4 {
		t.Fatalf("asked about %v; want the long-offline nodes only", asked)
	}
	for _, n := range []store.Node{gone, unselected} {
		if _, err := st.NodeByUUID(n.UUID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("node %s remains: %v", n.KubeNodeName, err)
		}
	}
	for _, n := range []store.Node{peer, stillThere, recent, oldPeer} {
		if _, err := st.NodeByUUID(n.UUID); err != nil {
			t.Fatalf("node %s removed: %v", n.KubeNodeName, err)
		}
	}
	if len(kicked) != 2 || kicked[0] != gone.UUID || kicked[1] != unselected.UUID {
		t.Fatalf("kicked %v", kicked)
	}
	// Nobody to ask, or an agent that cannot answer: nothing is removed.
	failing := func(context.Context, string, string, any) (json.RawMessage, error) { return nil, errors.New("offline") }
	sweepGoneKubeNodes(context.Background(), slog.New(slog.DiscardHandler), st, failing, func(string) {}, now.Add(-15*time.Minute))
	if _, err := st.NodeByUUID(stillThere.UUID); err != nil {
		t.Fatalf("a node was removed without an answer: %v", err)
	}
}
