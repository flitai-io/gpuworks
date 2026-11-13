package store

import (
	"errors"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestKubeClusterLiveSettingsAndNodes(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a", Namespace: "a", ServerURL: "https://gw", EnginePorts: "40000-42767", IdleReturnSeconds: -1, RuntimeClass: "nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b", Namespace: "b", ServerURL: "https://gw", EnginePorts: "45000-45999", IdleReturnSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	a1, _, err := st.EnrollKubeNode(a.Token, "uid-a", "n1")
	if err != nil {
		t.Fatal(err)
	}
	a2, _, err := st.EnrollKubeNode(a.Token, "uid-a", "n2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(b.Token, "uid-b", "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("plain", HashToken("plain-token"), "plain"); err != nil {
		t.Fatal(err)
	}

	live, err := st.KubeClusterLiveSettings(a.ID)
	if err != nil || live != (KubeLiveSettings{IdleReturnSeconds: -1, RuntimeClass: "nvidia", EnginePorts: "40000-42767"}) {
		t.Fatalf("live settings = %+v, %v", live, err)
	}
	if _, err := st.KubeClusterLiveSettings(b.ID + 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry = %v", err)
	}
	edited := *a
	edited.IdleReturnSeconds, edited.RuntimeClass, edited.EnginePorts = 300, "", "41000-41999"
	if _, err := st.UpdateKubeCluster(edited); err != nil {
		t.Fatal(err)
	}
	if live, err = st.KubeClusterLiveSettings(a.ID); err != nil || live != (KubeLiveSettings{IdleReturnSeconds: 300, EnginePorts: "41000-41999"}) {
		t.Fatalf("after an edit = %+v, %v", live, err)
	}

	uuids, err := st.KubeNodeUUIDs(a.ID)
	if err != nil || len(uuids) != 2 || !slices.Contains(uuids, a1.UUID) || !slices.Contains(uuids, a2.UUID) {
		t.Fatalf("entry a nodes = %v, %v; want its two, not the other entry's or the plain node", uuids, err)
	}
	if uuids, err = st.KubeNodeUUIDs(b.ID + 100); err != nil || len(uuids) != 0 {
		t.Fatalf("missing entry nodes = %v, %v", uuids, err)
	}
}
