package store

import (
	"errors"
	"testing"
)

func TestUpdateKubeClusterWritesOnlyEditableSettings(t *testing.T) {
	s := newTestStore(t)
	c, err := s.CreateKubeCluster(KubeCluster{Name: "a", Namespace: "gpuworks-system", ServerURL: "https://s", StateDir: "/opt/gpuworks", EnginePorts: "30000-30999", Priority: 10000, IdleReturnSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	edit := *c
	edit.Name, edit.NodeSelector, edit.Priority, edit.AllowPreemption, edit.IdleReturnSeconds = "b", "gpu=yes", 0, true, KubeKeepCards
	edit.Namespace, edit.ServerURL, edit.StateDir, edit.Token = "other", "https://x", "/x", "stolen"
	got, err := s.UpdateKubeCluster(edit)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "b" || got.NodeSelector != "gpu=yes" || got.Priority != 0 || !got.AllowPreemption || got.IdleReturnSeconds != KubeKeepCards {
		t.Fatalf("settings not written: %+v", got)
	}
	if got.Namespace != "gpuworks-system" || got.ServerURL != "https://s" || got.StateDir != "/opt/gpuworks" || got.Token != c.Token {
		t.Fatalf("fixed settings changed: %+v", got)
	}
	// Zero values are written too: an edit clears what it leaves empty.
	edit = *got
	edit.NodeSelector = ""
	if got, err = s.UpdateKubeCluster(edit); err != nil || got.NodeSelector != "" {
		t.Fatalf("clear selector: %+v %v", got, err)
	}
}

func TestUpdateKubeClusterRefusesATakenNameAndAnUnknownEntry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateKubeCluster(KubeCluster{Name: "a", ServerURL: "https://s"}); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateKubeCluster(KubeCluster{Name: "b", ServerURL: "https://s"})
	if err != nil {
		t.Fatal(err)
	}
	edit := *b
	edit.Name = "a"
	if _, err := s.UpdateKubeCluster(edit); !errors.Is(err, ErrKubeClusterNameTaken) {
		t.Fatalf("taken name: %v", err)
	}
	// Keeping its own name is no clash.
	edit.Name = "b"
	if _, err := s.UpdateKubeCluster(edit); err != nil {
		t.Fatal(err)
	}
	edit.UUID = "missing"
	if _, err := s.UpdateKubeCluster(edit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry: %v", err)
	}
}
