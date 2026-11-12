package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
	"gpuworks.ai/gpuworks/internal/proto"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// TestAcquireKubeAgentLockRefusesASecondHolder: two gpuworks agent
// processes must never run one node's workload at once.
func TestAcquireKubeAgentLockRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireKubeAgentLock(dir)
	if err != nil {
		t.Fatalf("first lock = %v", err)
	}
	defer first.Close()
	if _, err := acquireKubeAgentLock(dir); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("second lock on the same state directory = %v, want a refusal naming the reason", err)
	}
}

// TestAcquireKubeAgentLockReleasesWhenTheFdCloses: the lock is a flock on
// an open file description, not a process-wide lock, so closing the file
// that holds it frees it for the next holder.
func TestAcquireKubeAgentLockReleasesWhenTheFdCloses(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireKubeAgentLock(dir)
	if err != nil {
		t.Fatalf("first lock = %v", err)
	}
	if _, err := acquireKubeAgentLock(dir); err == nil {
		t.Fatal("a second holder was not refused while the first is open")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireKubeAgentLock(dir)
	if err != nil {
		t.Fatalf("lock after release = %v", err)
	}
	defer second.Close()
}

func TestWriteTerminationLogWritesTheReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	old := terminationLogPath
	terminationLogPath = path
	t.Cleanup(func() { terminationLogPath = old })
	writeTerminationLog("another gpuworks agent already runs here")
	got, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(got)) != "another gpuworks agent already runs here" {
		t.Fatalf("termination log = %q, %v", got, err)
	}
}

// fakeCensus is a kubeAcceleratorCensus a test controls directly.
type fakeCensus []proto.AcceleratorPresence

func (c fakeCensus) Vendors() []proto.AcceleratorPresence { return c }

// TestKubeCensusForPrefersAnOverride: a process test's GPUWORKS_FAKE_GPU_VENDORS
// substitute (cfg.AcceleratorHost) must be used in place of the real PCI
// bus census whenever one is given.
func TestKubeCensusForPrefersAnOverride(t *testing.T) {
	if _, ok := kubeCensusFor(nil).(*gpu.VendorCensus); !ok {
		t.Fatal("kubeCensusFor(nil) did not fall back to the real PCI bus census")
	}
	override := fakeCensus{{Vendor: "nvidia", Cards: 1}}
	if got := kubeCensusFor(override); got.(fakeCensus)[0].Vendor != "nvidia" {
		t.Fatalf("kubeCensusFor did not return the override: %#v", got)
	}
}

func TestKubeSupportedAccelerator(t *testing.T) {
	if _, ok := kubeSupportedAccelerator(nil); ok {
		t.Fatal("a nil census reported a supported accelerator")
	}
	if _, ok := kubeSupportedAccelerator(fakeCensus(nil)); ok {
		t.Fatal("an empty census reported a supported accelerator")
	}
	if _, ok := kubeSupportedAccelerator(fakeCensus{{Vendor: "apple", Cards: 1}}); ok {
		t.Fatal("a vendor this build cannot run on Kubernetes was reported supported")
	}
	if vendor, ok := kubeSupportedAccelerator(fakeCensus{{Vendor: "nvidia", Cards: 1}}); !ok || vendor != "nvidia" {
		t.Fatalf("kubeSupportedAccelerator = %q, %v, want nvidia", vendor, ok)
	}
}

// TestKubeStandByWithNoAcceleratorWaitsAndNeverEnrolls: the first thing a
// node with no accelerator on the bus yet does must not be an enrollment
// request — it stands by instead.
func TestKubeStandByWithNoAcceleratorWaitsAndNeverEnrolls(t *testing.T) {
	old := kubeStandByPoll
	kubeStandByPoll = 5 * time.Millisecond
	t.Cleanup(func() { kubeStandByPoll = old })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), t.TempDir(), fakeCensus(nil))
	if !stoodBy || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("kubeStandBy with no accelerator = %v, %v, want stood by until the deadline", stoodBy, err)
	}
}

// TestKubeStandByWithAnUnsupportedVendorWaits: a vendor on the bus this
// build has no Kubernetes profile for is the same as none at all.
func TestKubeStandByWithAnUnsupportedVendorWaits(t *testing.T) {
	old := kubeStandByPoll
	kubeStandByPoll = 5 * time.Millisecond
	t.Cleanup(func() { kubeStandByPoll = old })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), t.TempDir(), fakeCensus{{Vendor: "apple", Cards: 1}})
	if !stoodBy || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("kubeStandBy with an unsupported vendor = %v, %v, want stood by until the deadline", stoodBy, err)
	}
}

// TestKubeStandByWithASupportedAcceleratorEnrollsImmediately: a card whose
// CLI may well be broken must still enroll and report the failure, not
// stand by — stand-by keys on the bus, never on a CLI's answer.
func TestKubeStandByWithASupportedAcceleratorEnrollsImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), t.TempDir(), fakeCensus{{Vendor: "nvidia", Cards: 1}})
	if stoodBy || err != nil {
		t.Fatalf("kubeStandBy with a supported accelerator = %v, %v, want it to enroll immediately", stoodBy, err)
	}
}

// TestKubeStandByWithAnExistingIdentityEnrollsImmediately: stand-by only
// applies before a state directory's first enrollment.
func TestKubeStandByWithAnExistingIdentityEnrollsImmediately(t *testing.T) {
	state := t.TempDir()
	raw, _ := json.Marshal(kube.Identity{NodeUUID: "n-1", NodeToken: "tok-1"})
	if err := os.WriteFile(filepath.Join(state, kube.IdentityFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), state, fakeCensus(nil))
	if stoodBy || err != nil {
		t.Fatalf("kubeStandBy with an existing identity = %v, %v, want it to enroll immediately despite no accelerator on the bus", stoodBy, err)
	}
}

// TestKubeStandByRestartsOnceASupportedAcceleratorAppears: a node that
// stood by picks the appearance up on its next poll and hands off to
// kubeReexec, the same restart mechanism accelerator_choice.go uses.
func TestKubeStandByRestartsOnceASupportedAcceleratorAppears(t *testing.T) {
	old := kubeStandByPoll
	kubeStandByPoll = 5 * time.Millisecond
	t.Cleanup(func() { kubeStandByPoll = old })
	oldReexec := kubeReexec
	var reexecs int
	kubeReexec = func() error { reexecs++; return nil }
	t.Cleanup(func() { kubeReexec = oldReexec })

	var supported atomic.Bool
	census := censusFunc(func() []proto.AcceleratorPresence {
		if supported.Load() {
			return []proto.AcceleratorPresence{{Vendor: "nvidia", Cards: 1}}
		}
		return nil
	})
	go func() {
		time.Sleep(12 * time.Millisecond)
		supported.Store(true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), t.TempDir(), census)
	if !stoodBy || err != nil || reexecs != 1 {
		t.Fatalf("kubeStandBy once an accelerator appears = %v, %v, reexecs=%d", stoodBy, err, reexecs)
	}
}

// censusFunc adapts a function to kubeAcceleratorCensus.
type censusFunc func() []proto.AcceleratorPresence

func (f censusFunc) Vendors() []proto.AcceleratorPresence { return f() }

// TestKubeStandByReturnsAnErrorWhenReexecFails: a failed restart is
// reported rather than silently retried forever.
func TestKubeStandByReturnsAnErrorWhenReexecFails(t *testing.T) {
	old := kubeStandByPoll
	kubeStandByPoll = 5 * time.Millisecond
	t.Cleanup(func() { kubeStandByPoll = old })
	oldReexec := kubeReexec
	wantErr := errors.New("exec failed")
	kubeReexec = func() error { return wantErr }
	t.Cleanup(func() { kubeReexec = oldReexec })

	var supported atomic.Bool
	census := censusFunc(func() []proto.AcceleratorPresence {
		if supported.Load() {
			return []proto.AcceleratorPresence{{Vendor: "nvidia", Cards: 1}}
		}
		return nil
	})
	go func() {
		time.Sleep(12 * time.Millisecond)
		supported.Store(true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stoodBy, err := kubeStandBy(ctx, discardLog(), t.TempDir(), census)
	if !stoodBy || !errors.Is(err, wantErr) {
		t.Fatalf("kubeStandBy when kubeReexec fails = %v, %v, want the reexec error", stoodBy, err)
	}
}

// acceleratorPreferenceFile mirrors internal/agent/app/accelerator_choice.go's
// private constant of the same name; cmd/gpuworks-agent has no access to it,
// so tests that set up a pre-existing preference duplicate its spelling.
const acceleratorPreferenceFile = "accelerator-preference"

func enrollServer(t *testing.T, nodeUUID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		raw, _ := json.Marshal(kube.Identity{NodeUUID: nodeUUID, NodeToken: "tok-" + nodeUUID, IdleReturnSeconds: -1})
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnrollKubeNodeClearsThePreferenceOnlyWhenTheNodeUUIDChanges: a state
// directory's saved accelerator preference belongs to whichever cluster
// entry last enrolled it; a new entry enrolling the same hostPath (a new
// node UUID) must not inherit it, but re-enrolling as the same node must.
func TestEnrollKubeNodeClearsThePreferenceOnlyWhenTheNodeUUIDChanges(t *testing.T) {
	t.Setenv("GPUWORKS_KUBE_ENROLL_TOKEN", "tok")
	t.Setenv("GPUWORKS_KUBE_NODE_NAME", "gpu-1")
	k := kubefake.New(t, "gpuworks-system", "gpu-1")

	t.Run("first enrollment leaves an unrelated preference alone", func(t *testing.T) {
		state := t.TempDir()
		pref := filepath.Join(state, acceleratorPreferenceFile)
		if err := os.WriteFile(pref, []byte("nvidia\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := enrollServer(t, "n-1")
		if _, err := enrollKubeNodeWith(context.Background(), discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: state}, k.Client()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(pref); err != nil {
			t.Fatalf("preference file after a node's first enrollment = %v, want it untouched", err)
		}
	})

	t.Run("re-enrolling the same node UUID keeps the preference", func(t *testing.T) {
		state := t.TempDir()
		raw, _ := json.Marshal(kube.Identity{NodeUUID: "n-1", NodeToken: "old"})
		if err := os.WriteFile(filepath.Join(state, kube.IdentityFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		pref := filepath.Join(state, acceleratorPreferenceFile)
		if err := os.WriteFile(pref, []byte("nvidia\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := enrollServer(t, "n-1")
		if _, err := enrollKubeNodeWith(context.Background(), discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: state}, k.Client()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(pref); err != nil {
			t.Fatalf("preference file after re-enrolling the same node = %v, want it kept", err)
		}
	})

	t.Run("a new node UUID for this state directory clears the preference", func(t *testing.T) {
		state := t.TempDir()
		raw, _ := json.Marshal(kube.Identity{NodeUUID: "n-1", NodeToken: "old"})
		if err := os.WriteFile(filepath.Join(state, kube.IdentityFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		pref := filepath.Join(state, acceleratorPreferenceFile)
		if err := os.WriteFile(pref, []byte("nvidia\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := enrollServer(t, "n-2")
		id, err := enrollKubeNodeWith(context.Background(), discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: state}, k.Client())
		if err != nil || id.NodeUUID != "n-2" {
			t.Fatalf("enrollKubeNodeWith = %+v, %v", id, err)
		}
		if _, err := os.Stat(pref); err == nil || !os.IsNotExist(err) {
			t.Fatalf("preference file after a new node UUID = %v, want it cleared", err)
		}
	})
}
