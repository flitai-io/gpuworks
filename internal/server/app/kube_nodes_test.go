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
	strikes := map[string]bool{}
	sweep := func() {
		sweepGoneKubeNodes(context.Background(), slog.New(slog.DiscardHandler), st, call, func(u string) { kicked = append(kicked, u) }, now.Add(-15*time.Minute), strikes)
	}
	// The first answer only marks them: a rolling update can be between
	// deleting and re-creating a node's agent pod.
	sweep()
	if len(kicked) != 0 || len(strikes) != 2 {
		t.Fatalf("first sweep removed %v, marked %v", kicked, strikes)
	}
	asked = nil
	sweep()
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
	for range 2 {
		sweepGoneKubeNodes(context.Background(), slog.New(slog.DiscardHandler), st, failing, func(string) {}, now.Add(-15*time.Minute), strikes)
	}
	if _, err := st.NodeByUUID(stillThere.UUID); err != nil {
		t.Fatalf("a node was removed without an answer: %v", err)
	}
	if len(strikes) != 0 {
		t.Fatalf("marks kept for nodes no sweep found gone: %v", strikes)
	}
}

// A node found gone once and then back is not removed by the next answer
// that it is gone again: the two must be in a row.
func TestSweepNeedsTwoAnswersInARow(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := st.CreateKubeCluster(store.KubeCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, n := range []struct{ name, state string }{{"peer", store.NodeStateOnline}, {"flaky", store.NodeStateOffline}} {
		node, _, err := st.EnrollKubeNode(c.Token, "uid", n.name)
		if err != nil {
			t.Fatal(err)
		}
		seen := now
		if n.state == store.NodeStateOffline {
			seen = now.Add(-time.Hour)
		}
		if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{"state": n.state, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
	}
	answers := []bool{false, true, false}
	call := func(context.Context, string, string, any) (json.RawMessage, error) {
		agent := answers[0]
		answers = answers[1:]
		return json.Marshal(proto.KubeNodeExistsResult{Exists: true, Agent: &agent})
	}
	strikes := map[string]bool{}
	for range 3 {
		sweepGoneKubeNodes(context.Background(), slog.New(slog.DiscardHandler), st, call, func(string) { t.Fatal("removed") }, now.Add(-15*time.Minute), strikes)
	}
	if len(strikes) != 1 {
		t.Fatalf("strikes = %v, want the last answer's mark", strikes)
	}
}
