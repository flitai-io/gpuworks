package hub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type sentKind struct {
	kind string
	body json.RawMessage
}

// kubeSessions records what the hub writes to each fake session.
type kubeSessions struct {
	mu   sync.Mutex
	sent map[string][]sentKind
}

func (k *kubeSessions) take(uuid string) []sentKind {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.sent[uuid]
	k.sent[uuid] = nil
	return out
}

func (k *kubeSessions) waitFor(t *testing.T, uuid string, kinds ...string) map[string]json.RawMessage {
	t.Helper()
	got := map[string]json.RawMessage{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range k.take(uuid) {
			got[s.kind] = s.body
		}
		done := true
		for _, kind := range kinds {
			if got[kind] == nil {
				done = false
			}
		}
		if done {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %s received %v, want %v", uuid, got, kinds)
	return nil
}

// settle waits long enough for a push that should not happen to have shown
// itself, then reports what the node was sent.
func (k *kubeSessions) settle(uuid string) []sentKind {
	time.Sleep(100 * time.Millisecond)
	return k.take(uuid)
}

func (k *kubeSessions) connect(t *testing.T, h *Hub, uuid string, initialDone bool) *Session {
	t.Helper()
	s := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{}), initialPushDone: make(chan struct{}), caps: []string{proto.CapNetworkUpdate},
		sendFn: func(kind string, _ uint64, data any) error {
			raw, err := json.Marshal(data)
			k.mu.Lock()
			k.sent[uuid] = append(k.sent[uuid], sentKind{kind, raw})
			k.mu.Unlock()
			return err
		}}
	if initialDone {
		close(s.initialPushDone)
	}
	h.mu.Lock()
	h.sessions[uuid] = s
	h.mu.Unlock()
	t.Cleanup(s.markClosed)
	return s
}

func kubeEntryWithNodes(t *testing.T, st *store.Store, name, ports string, idle int, class string, nodes ...string) (*store.KubeCluster, []string) {
	t.Helper()
	entry, err := st.CreateKubeCluster(store.KubeCluster{Name: name, Namespace: name, ServerURL: "https://gw", EnginePorts: ports, IdleReturnSeconds: idle, RuntimeClass: class})
	if err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for _, nodeName := range nodes {
		node, _, err := st.EnrollKubeNode(entry.Token, "uid-"+name, nodeName)
		if err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, node.UUID)
	}
	return entry, uuids
}

func TestPushKubeSettingsReachesOnlyTheEntrysOnlineNodes(t *testing.T) {
	h := newTestHub(t)
	h.SetEnginePorts(func() proto.PortRange { return proto.PortRange{Start: 30000, End: 32767} })
	rec := &kubeSessions{sent: map[string][]sentKind{}}
	a, aNodes := kubeEntryWithNodes(t, h.store, "a", "40000-42767", 60, "", "a1", "a2", "a3")
	_, bNodes := kubeEntryWithNodes(t, h.store, "b", "45000-45999", 10, "nvidia", "b1")
	plain, _ := newTestNode(t, h.store)
	for _, uuid := range []string{aNodes[0], aNodes[1], bNodes[0], plain} { // a3 stays offline
		rec.connect(t, h, uuid, true)
	}

	// Editing the entry in the store, then the push the API makes.
	edited := *a
	edited.IdleReturnSeconds, edited.RuntimeClass, edited.EnginePorts = -1, "nvidia-legacy", "41000-41999"
	if _, err := h.store.UpdateKubeCluster(edited); err != nil {
		t.Fatal(err)
	}
	h.PushKubeSettings(a.ID)

	for _, uuid := range aNodes[:2] {
		got := rec.waitFor(t, uuid, proto.TKubeSettings, proto.TStateUpdate)
		var settings proto.KubeSettings
		if err := json.Unmarshal(got[proto.TKubeSettings], &settings); err != nil || settings != (proto.KubeSettings{IdleReturnSeconds: -1, RuntimeClass: "nvidia-legacy"}) {
			t.Fatalf("%s kube.settings = %s %v", uuid, got[proto.TKubeSettings], err)
		}
		var update struct {
			Network proto.NetworkConfig `json:"network"`
		}
		if err := json.Unmarshal(got[proto.TStateUpdate], &update); err != nil || update.Network.EnginePorts == nil || *update.Network.EnginePorts != (proto.PortRange{Start: 41000, End: 41999}) {
			t.Fatalf("%s network = %s %v: the agent reports its listeners in the entry's range", uuid, got[proto.TStateUpdate], err)
		}
	}
	for _, uuid := range []string{bNodes[0], plain, aNodes[2]} {
		if sent := rec.settle(uuid); len(sent) != 0 {
			t.Fatalf("%s was sent %v, not part of the edited entry or offline", uuid, sent)
		}
	}
}

// A connecting Kubernetes node gets its entry's settings right after the
// state replay, and its engine ports are the entry's; any other node sees
// the replay alone, as before.
func TestInitialReplaySendsKubeSettings(t *testing.T) {
	h := newTestHub(t)
	h.SetEnginePorts(func() proto.PortRange { return proto.PortRange{Start: 30000, End: 32767} })
	rec := &kubeSessions{sent: map[string][]sentKind{}}
	_, nodes := kubeEntryWithNodes(t, h.store, "a", "40000-42767", 300, "nvidia", "a1")
	plain, _ := newTestNode(t, h.store)

	kube := rec.connect(t, h, nodes[0], false)
	if err := kube.pushInitialState(); err != nil {
		t.Fatal(err)
	}
	sent := rec.take(nodes[0])
	if len(sent) != 2 || sent[0].kind != proto.TStateSync || sent[1].kind != proto.TKubeSettings {
		t.Fatalf("kube node was sent %v, want state.sync then kube.settings", sent)
	}
	var settings proto.KubeSettings
	if err := json.Unmarshal(sent[1].body, &settings); err != nil || settings != (proto.KubeSettings{IdleReturnSeconds: 300, RuntimeClass: "nvidia"}) {
		t.Fatalf("kube.settings = %s %v", sent[1].body, err)
	}
	var replay struct {
		Network proto.NetworkConfig `json:"network"`
	}
	if err := json.Unmarshal(sent[0].body, &replay); err != nil || replay.Network.EnginePorts == nil || *replay.Network.EnginePorts != (proto.PortRange{Start: 40000, End: 42767}) {
		t.Fatalf("state.sync network = %s %v", sent[0].body, err)
	}

	// A later full replay (SyncNode) repeats the state alone: an edit pushes
	// its own settings, and an unchanged value is not worth an agent's lookup.
	if err := kube.pushInitialState(); err != nil {
		t.Fatal(err)
	}
	if again := rec.take(nodes[0]); len(again) != 1 || again[0].kind != proto.TStateSync {
		t.Fatalf("second replay sent %v, want state.sync alone", again)
	}

	other := rec.connect(t, h, plain, false)
	if err := other.pushInitialState(); err != nil {
		t.Fatal(err)
	}
	sent = rec.take(plain)
	if len(sent) != 1 || sent[0].kind != proto.TStateSync {
		t.Fatalf("a node outside Kubernetes was sent %v", sent)
	}
	if err := json.Unmarshal(sent[0].body, &replay); err != nil || replay.Network.EnginePorts == nil || *replay.Network.EnginePorts != (proto.PortRange{Start: 30000, End: 32767}) {
		t.Fatalf("plain node state.sync network = %s %v", sent[0].body, err)
	}
}
