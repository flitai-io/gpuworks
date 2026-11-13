package kube

import (
	"context"
	"sync"
	"testing"
	"time"
)

func runtimeClassOf(t *testing.T, r *rig, id string) string {
	t.Helper()
	if rc := r.srv.Pod(testNS, id).Spec.RuntimeClassName; rc != nil {
		return *rc
	}
	return ""
}

// The server hands the NVIDIA RuntimeClass over live: instance pods started
// after it use it, those already running keep theirs, and an empty value
// makes the agent look for the cluster's own class again.
func TestApplySettingsChangesTheRuntimeClassOfTheNextInstancePod(t *testing.T) {
	ctx := context.Background()
	// An old manifest still names a class; the server's value wins.
	r := detectedRig(t, "from-old-manifest", []string{"nvidia"}, nil, 0)
	run := func(name string) string {
		t.Helper()
		s := spec(name, 0)
		s.DockerArgs = []string{"--gpus", "device=0"}
		id, err := r.d.Run(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := run("gpuworks-rc1000000000")
	if got := runtimeClassOf(t, r, first); got != "from-old-manifest" {
		t.Fatalf("before any settings: %q", got)
	}

	r.d.ApplySettings(ctx, 10*time.Minute, "nvidia-cdi")
	if err := r.d.Remove(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := run("gpuworks-rc2000000000")
	if got := runtimeClassOf(t, r, second); got != "nvidia-cdi" {
		t.Fatalf("after the server named nvidia-cdi: %q", got)
	}

	// Empty: the cluster defines "nvidia", so that is the class.
	r.d.ApplySettings(ctx, 10*time.Minute, "")
	third := run("gpuworks-rc3000000000")
	if got := runtimeClassOf(t, r, third); got != "nvidia" {
		t.Fatalf("after an empty setting with an nvidia class in the cluster: %q", got)
	}
	if got := runtimeClassOf(t, r, second); got != "nvidia-cdi" {
		t.Fatalf("a running instance's pod changed to %q", got)
	}

	// Empty with no class in the cluster: the node's default runtime.
	delete(r.srv.RuntimeClasses, "nvidia")
	r.d.ApplySettings(ctx, 10*time.Minute, "")
	if err := r.d.Remove(ctx, third); err != nil {
		t.Fatal(err)
	}
	if got := runtimeClassOf(t, r, run("gpuworks-rc4000000000")); got != "" {
		t.Fatalf("empty setting without a cluster class: %q", got)
	}
}

// Detect, which runs once at start, never undoes what the server gave.
func TestDetectKeepsTheClassTheServerAlreadyGave(t *testing.T) {
	r := detectedRig(t, "", []string{"nvidia"}, nil, 0)
	r.d.ApplySettings(context.Background(), time.Minute, "given")
	if ok, err := r.d.Detect(context.Background()); !ok || err != nil {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	if got := r.d.runtimeClass.get(); got != "given" {
		t.Fatalf("class after a second Detect = %q", got)
	}
}

// The idle return is read at every sweep, so a pushed value is the next
// sweep's: a shorter one returns a card that the old one would have kept.
func TestApplySettingsChangesWhenIdleCardsGoBack(t *testing.T) {
	r := newRig(t, 2) // idle return 10 minutes
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 1 {
		t.Fatalf("hold pods after 2 of 10 idle minutes = %v", r.holdPods())
	}
	r.d.ApplySettings(ctx, time.Minute, "nvidia")
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 0 {
		t.Fatalf("hold pods after the idle return was cut to a minute = %v", r.holdPods())
	}
}

// A cluster switched to keeping cards makes a running agent borrow its free
// cards now, without a restart.
func TestApplySettingsKeepBorrowsTheFreeCardsAtOnce(t *testing.T) {
	r := newRig(t, 2)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { r.d.Start(ctx); close(done) }()
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods before keeping: %v", holds)
	}
	r.d.ApplySettings(ctx, -1, "nvidia")
	deadline := time.Now().Add(5 * time.Second)
	for len(r.holdPods()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("hold pods after the cluster switched to keeping cards = %v", r.holdPods())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// And cards stay: a sweep past any grace period returns none (the agent's
	// own loop is stopped first, it reads the clock too).
	stop()
	<-done
	r.now = r.now.Add(24 * time.Hour)
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 2 {
		t.Fatalf("hold pods after a sweep while keeping = %v", r.holdPods())
	}
}

// Settings arrive on the connection's goroutine while instances start and
// the pool sweeps; run with -race.
func TestApplySettingsIsSafeWhileInstancesStart(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			r.d.ApplySettings(ctx, time.Duration(i+1)*time.Minute, []string{"nvidia", "nvidia-cdi"}[i%2])
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			r.d.cards.sweep(ctx)
			_ = r.d.runtimeClass.get()
		}
	}()
	if _, err := r.d.Run(ctx, spec("gpuworks-race00000000", 0)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
