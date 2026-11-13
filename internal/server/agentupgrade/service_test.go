package agentupgrade

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type testTransport struct {
	mu          sync.Mutex
	connections map[string]Connection
	calls       []string
	pushErr     error
	pullErr     error
	push        func(context.Context) error
}

func (f *testTransport) AgentUpgradeConnection(uuid string) (Connection, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.connections[uuid]
	return c, ok
}
func (f *testTransport) PushAgentUpgrade(ctx context.Context, uuid, session, name string, b []byte) error {
	f.mu.Lock()
	f.calls = append(f.calls, "push:"+uuid)
	err, fn := f.pushErr, f.push
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return err
}
func (f *testTransport) RequestAgentUpgrade(_ context.Context, uuid, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "pull:"+uuid)
	return f.pullErr
}
func (f *testTransport) set(uuid string, c Connection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connections[uuid] = c
}
func (f *testTransport) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type fixture struct {
	s   *Service
	st  *store.Store
	tr  *testTransport
	now time.Time
	cfg Config
}

func newFixture(t *testing.T, count int, push bool) *fixture {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, tr: &testTransport{connections: map[string]Connection{}}, now: time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)}
	caps := []string{}
	if push {
		caps = []string{proto.CapUpgradePush}
	}
	for i := 1; i <= count; i++ {
		uuid := fmt.Sprintf("n%d", i)
		n, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(n).Updates(map[string]any{"agent_version": "old", "os": "linux", "arch": "amd64"}).Error; err != nil {
			t.Fatal(err)
		}
		f.tr.set(uuid, Connection{SessionID: "old-" + uuid, Version: "old", Ready: true, Caps: caps})
	}
	f.cfg = Config{TargetVersion: "target", ArtifactDir: filepath.Join(t.TempDir(), "artifacts"), Transport: f.tr, ReadBinary: func(string) ([]byte, error) { return []byte("verified-agent-binary"), nil }, Now: func() time.Time { return f.now }, TransferTimeout: time.Minute, ConfirmTimeout: time.Minute, PollInterval: time.Millisecond}
	f.s, err = New(st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) create(t *testing.T, key string, uuids []string, source uint) RunView {
	t.Helper()
	p, err := f.s.Preview(context.Background(), uuids)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Create(context.Background(), CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: key, RequestedBy: "admin", SourceRunID: source})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) tick(t *testing.T) {
	t.Helper()
	if err := f.s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) item(t *testing.T, id uint, uuid string) store.AgentUpgradeItem {
	t.Helper()
	i, err := f.st.AgentUpgradeItem(id, uuid)
	if err != nil {
		t.Fatal(err)
	}
	return *i
}

func TestUpgradeRunUsesFullSnapshotAndWaitsForNewFormalSession(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "all", nil, 0)
	if r.Counts.Total != 3 {
		t.Fatalf("snapshot=%+v", r.Counts)
	}
	if _, err := f.st.RegisterNode("late", store.HashToken("late"), "late"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatal("first upgrade not dispatched")
	}
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatal("verified released serial slot before formal registration")
	}
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: false, Caps: []string{proto.CapUpgradePush}})
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatal("unready/smoke-like connection counted as success")
	}
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
	f.tick(t)
	if f.tr.count() != 2 {
		t.Fatal("formal registration did not release next node")
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "succeeded" {
		t.Fatalf("item=%+v", i)
	}
	items, _ := f.st.AgentUpgradeItems(r.Record.ID)
	if len(items) != 3 {
		t.Fatal("new node expanded immutable snapshot")
	}
}

func TestUnconfirmedBlocksEveryNodeAndLateProofReleasesWithoutRewritingHistory(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "first", []string{"n1"}, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" {
		t.Fatalf("item=%+v", i)
	}
	p, _ := f.s.Preview(context.Background(), []string{"n2"})
	_, err := f.s.Create(context.Background(), CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "second"})
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "AGENT_UPGRADE_ACTIVE" {
		t.Fatalf("uncertain reservation bypass=%v", err)
	}
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: true})
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase != "unconfirmed" || i.VerifiedAt == nil || i.VerificationCode != "target_registered" {
		t.Fatalf("late proof=%+v", i)
	}
	f.create(t, "second", []string{"n2"}, 0)
}

func TestStopCancelsQueueAndKeepsInFlightObserved(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "all", nil, 0)
	f.tick(t)
	stopped, err := f.s.Stop(context.Background(), r.Record.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopping" || stopped.Counts.Cancelled != 1 {
		t.Fatalf("stop=%+v", stopped)
	}
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: true})
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatal("cancelled queue was dispatched")
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "succeeded" {
		t.Fatalf("active was falsely cancelled=%+v", i)
	}
}

func TestLegacyPinOutlivesFinishedRunAndRefusesChangedTarget(t *testing.T) {
	f := newFixture(t, 1, false)
	r := f.create(t, "legacy", nil, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	name := proto.AgentBinaryName("linux", "amd64")
	data, handled, err := f.s.DownloadBinary(name)
	if err != nil || !handled || string(data) != "verified-agent-binary" {
		t.Fatalf("pin=%q %v %v", data, handled, err)
	}
	cfg := f.cfg
	cfg.TargetVersion = "another-server-build"
	restarted, err := New(f.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, handled, err := restarted.DownloadBinary(name); !handled || err == nil {
		t.Fatalf("changed target served live bytes: %v %v", handled, err)
	}
	if err := restarted.Release(context.Background(), r.Record.ID, "n1", "admin", "checked original upgrade process ended"); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := restarted.DownloadBinary(name); handled || err != nil {
		t.Fatalf("released pin=%v %v", handled, err)
	}
}

func TestRestartObservesPendingAttemptWithoutResending(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "one", nil, 0)
	f.tick(t)
	restarted, err := New(f.st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.tr.count() != 1 {
		t.Fatal("restart resent uncertain upgrade")
	}
	f.tr.set("n1", Connection{SessionID: "new", Version: "target", Ready: true})
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "succeeded" {
		t.Fatalf("recovery=%+v", i)
	}
}

func TestExplicitRefusalAndRollbackAreNotConfusedWithUnconfirmedTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rejected error
		want     string
	}{
		{"refused", &RejectedError{Detail: "agent rejected upgrade: binary invalid"}, "failed"},
		{"busy", &RejectedError{Detail: "agent upgrade already in progress", Busy: true}, "observing_recovery"},
		{"network", errors.New("read upgrade answer: EOF"), "observing_recovery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1, true)
			f.tr.pushErr = tc.rejected
			r := f.create(t, "one", nil, 0)
			f.tick(t)
			if i := f.item(t, r.Record.ID, "n1"); i.Phase != tc.want {
				t.Fatalf("item=%+v", i)
			}
		})
	}
	f := newFixture(t, 1, true)
	r := f.create(t, "rollback", nil, 0)
	f.tick(t)
	f.tr.set("n1", Connection{SessionID: "rolled-back-session", Version: "old", Ready: false})
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "rolled-back-session", Version: "old", State: "failed", Error: "rolled back: new binary never registered", At: f.now})
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "failed" || i.Reason != "agent_failed" {
		t.Fatalf("rollback result was lost before first heartbeat: %+v", i)
	}
}

func TestFixedArtifactsIdempotencyAndTargetChanges(t *testing.T) {
	f := newFixture(t, 1, false)
	binary := []byte("initial binary")
	f.s.cfg.ReadBinary = func(string) ([]byte, error) { return binary, nil }
	p, err := f.s.Preview(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "fixed", RequestedBy: "admin"}
	r, err := f.s.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	binary = []byte("replaced override")
	again, err := f.s.Create(context.Background(), req)
	if err != nil || again.Record.ID != r.Record.ID {
		t.Fatalf("repeat lost original=%+v %v", again, err)
	}
	data, handled, err := f.s.DownloadBinary(proto.AgentBinaryName("linux", "amd64"))
	if err != nil || !handled || string(data) != "initial binary" {
		t.Fatalf("pin drifted=%q %v %v", data, handled, err)
	}
	req.RequestedBy = "other"
	if _, err := f.s.Create(context.Background(), req); err == nil {
		t.Fatal("changed request reused idempotency key")
	}
	if _, err := f.s.Stop(context.Background(), r.Record.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = "stale"
	req.RequestedBy = "admin"
	var typed *Error
	if _, err := f.s.Create(context.Background(), req); !errors.As(err, &typed) || typed.Code != "AGENT_UPGRADE_TARGET_CHANGED" {
		t.Fatalf("stale target=%v", err)
	}
}

func TestServerUpgradeGateAndStopAudit(t *testing.T) {
	f := newFixture(t, 1, true)
	finish, err := f.s.BeginServerUpgrade()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.s.Preview(context.Background(), nil)
	req := CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "guard"}
	if _, err := f.s.Create(context.Background(), req); err == nil {
		t.Fatal("create bypassed server upgrade guard")
	}
	finish(false)
	r, err := f.s.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BeginServerUpgrade(); err == nil {
		t.Fatal("server upgrade bypassed active Agent run")
	}
	if _, err := f.s.Stop(context.Background(), r.Record.ID, "first-admin"); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	if _, err := f.s.Stop(context.Background(), r.Record.ID, "second-admin"); err != nil {
		t.Fatal(err)
	}
	saved, err := f.st.AgentUpgradeRun(r.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.StopRequestedBy != "first-admin" || saved.StopRequestedAt == nil || saved.StopRequestedAt.Equal(f.now) {
		t.Fatalf("stop audit overwritten=%+v", saved)
	}
	finish, err = f.s.BeginServerUpgrade()
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	if _, err := f.s.Create(context.Background(), req); err == nil {
		t.Fatal("accepted server upgrade released maintenance guard")
	}
}

func TestUpgradePaginationHandlesMaximumPage(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "page", nil, 0)
	maxInt := int(^uint(0) >> 1)
	p, err := f.s.List(context.Background(), maxInt, 200)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("page=%+v err=%v", p, err)
	}
	items, err := f.s.Items(context.Background(), r.Record.ID, maxInt, 200, "")
	if err != nil || len(items.Items) != 0 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestShutdownDoesNotResendPotentiallyAcceptedUpgrade(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "shutdown", nil, 0)
	called := make(chan struct{})
	f.tr.push = func(ctx context.Context) error { close(called); <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.s.Run(ctx) }()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade never dispatched")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "observing_recovery" {
		t.Fatalf("shutdown=%+v", i)
	}
	restarted, err := New(f.st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.tr.count() != 1 {
		t.Fatal("shutdown recovery duplicated dispatch")
	}
}

func TestAgentFailureDoesNotRequireTheFirstHeartbeat(t *testing.T) {
	f := newFixture(t, 1, false)
	r := f.create(t, "early-failure", nil, 0)
	f.tick(t)
	f.tr.set("n1", Connection{SessionID: "old-n1", Version: "old", Ready: false})
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "old-n1", State: "failed", Error: "self-upgrade requires server URL and node token", At: f.now})
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "failed" {
		t.Fatalf("explicit current-session failure was lost=%+v", i)
	}
}

func TestLateReconnectPreservesTimeoutEvenWithoutIntermediatePoll(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "all", nil, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tr.set("n1", Connection{SessionID: "late-target", Version: "target", Ready: true})
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase != "unconfirmed" || i.Reason != "confirmation_timeout" || i.VerifiedAt == nil || i.VerificationCode != "target_registered" {
		t.Fatalf("late reconnect rewrote timeout history: %+v", i)
	}
	if f.tr.count() != 1 || f.item(t, r.Record.ID, "n2").Phase != "queued" {
		t.Fatal("late evidence automatically resumed paused queue")
	}
}

func TestRestartWithChangedArtifactsInterruptsQueueAndLegacyPin(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			f := newFixture(t, 2, false)
			r := f.create(t, "all", nil, 0)
			f.tick(t)
			cfg := f.cfg
			cfg.ReadBinary = func(string) ([]byte, error) {
				if missing {
					return nil, errors.New("release artifact unavailable")
				}
				return []byte("different-agent-same-version"), nil
			}
			restarted, err := New(f.st, cfg)
			if err != nil {
				t.Fatal(err)
			}
			run, err := restarted.Get(context.Background(), r.Record.ID)
			if err != nil || run.State != "interrupted" || run.Record.InterruptedReason != "artifact_changed" {
				t.Fatalf("changed build resumed: %+v %v", run, err)
			}
			if _, handled, err := restarted.DownloadBinary(proto.AgentBinaryName("linux", "amd64")); !handled || err == nil {
				t.Fatalf("changed build served pinned bytes: %v %v", handled, err)
			}
			f.tr.set("n1", Connection{SessionID: "target-after-restart", Version: "target", Ready: true})
			if err := restarted.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.tr.count() != 1 || f.item(t, r.Record.ID, "n2").Phase != "queued" {
				t.Fatal("interrupted queue dispatched after observing original completion")
			}
		})
	}
}

func TestDispatchRechecksPlatformAndLegacyDownloadReachability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change map[string]any
		reason string
	}{
		{"platform", map[string]any{"arch": "arm64"}, "platform_changed"},
		{"connection", map[string]any{"connect_mode": "server_dial"}, "legacy_unverified"},
		{"kubernetes", map[string]any{"kube_cluster_id": 7}, "kubernetes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1, false)
			r := f.create(t, "queued", nil, 0)
			if err := f.st.DB().Model(&store.Node{}).Where("uuid = ?", "n1").Updates(tc.change).Error; err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			if i := f.item(t, r.Record.ID, "n1"); i.Phase != "skipped" || i.Reason != tc.reason {
				t.Fatalf("changed eligibility dispatched: %+v", i)
			}
			if f.tr.count() != 0 {
				t.Fatal("issued upgrade using stale platform or download conditions")
			}
		})
	}
}

// A Kubernetes node's agent is a pod of its cluster's DaemonSet: the batch
// upgrade leaves it to the node's own upgrade, which rolls the DaemonSet.
func TestPreviewLeavesKubernetesNodesToTheirDaemonSet(t *testing.T) {
	f := newFixture(t, 2, false)
	if err := f.st.DB().Model(&store.Node{}).Where("uuid = ?", "n1").Update("kube_cluster_id", 7).Error; err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Preview(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range p.Nodes {
		got[c.NodeUUID] = c.Eligibility
	}
	if got["n1"] != "kubernetes" || got["n2"] != "eligible" {
		t.Fatalf("eligibility = %v, want n1 kubernetes and n2 eligible", got)
	}
}

// An agent that runs from its node's state directory (kubeboot) is upgraded
// like any other: the preview offers it and the dispatch sends it the binary,
// while its neighbour on the image's own binary is still left to its
// DaemonSet.
func TestKubernetesAgentsOnTheStateDirectoryAreUpgradedInPlace(t *testing.T) {
	f := newFixture(t, 2, true)
	if err := f.st.DB().Model(&store.Node{}).Where("uuid IN ?", []string{"n1", "n2"}).Update("kube_cluster_id", 7).Error; err != nil {
		t.Fatal(err)
	}
	f.tr.set("n1", Connection{SessionID: "old-n1", Version: "old", Ready: true, Caps: []string{proto.CapUpgradePush, proto.CapKubeBinaryUpgrade}})
	p, err := f.s.Preview(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Candidate{}
	for _, c := range p.Nodes {
		got[c.NodeUUID] = c
	}
	if got["n1"].Eligibility != "eligible" || got["n1"].Transport != "push" {
		t.Fatalf("state-directory agent = %+v, want eligible over push", got["n1"])
	}
	if got["n2"].Eligibility != "kubernetes" {
		t.Fatalf("image-binary agent = %+v, want kubernetes", got["n2"])
	}
	r := f.create(t, "queued", []string{"n1"}, 0)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase == "skipped" {
		t.Fatalf("state-directory agent skipped at dispatch: %+v", i)
	}
	if f.tr.count() != 1 {
		t.Fatalf("upgrades sent = %d, want 1", f.tr.count())
	}
}

func TestFleetNoticeUsesPackageMetadataAndCountsUntilRealignment(t *testing.T) {
	f := newFixture(t, 3, true)
	nodes, err := f.st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	nodes[0].AgentVersion = "target"
	f.s.cfg.ReadBinary = func(string) ([]byte, error) { t.Fatal("listing nodes read an upgrade binary"); return nil, nil }
	checks := 0
	f.s.cfg.BinaryAvailable = func(name string) bool { checks++; return false }
	result, err := f.s.Fleet(context.Background(), nodes)
	if err != nil || checks != 1 || result.Eligibility[nodes[0].UUID] != "already_current" || result.Eligibility[nodes[1].UUID] != "artifact_missing" || result.Eligibility[nodes[2].UUID] != "artifact_missing" {
		t.Fatalf("missing local package: %+v, checks %d error %v", result, checks, err)
	}
	f.s.cfg.TargetVersion = "dev"
	result, err = f.s.Fleet(context.Background(), nodes)
	if err != nil || len(result.Eligibility) != 0 || result.Target != "dev" || checks != 1 {
		t.Fatalf("development target: %+v, checks %d error %v", result, checks, err)
	}
}

func TestMixedBatchSkipsNodesAlignedBeforePreviewOrWhileQueued(t *testing.T) {
	f := newFixture(t, 10, true)
	for _, uuid := range []string{"n1", "n2", "n3"} {
		if err := f.st.SetNodeAgentInfo(uuid, "target", ""); err != nil {
			t.Fatal(err)
		}
	}
	r := f.create(t, "mixed", nil, 0)
	if r.Counts.Skipped != 3 || r.Counts.Queued != 7 {
		t.Fatal(r.Counts)
	}
	f.tr.set("n10", Connection{SessionID: "other-upgrade", Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
	for i := 0; i < 12; i++ {
		f.tick(t)
		pending, err := f.st.AgentUpgradePendingItems()
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range pending {
			if activePhase(item.Phase) {
				f.tr.set(item.NodeUUID, Connection{SessionID: "confirmed-" + item.NodeUUID, Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
			}
		}
		if len(pending) == 0 {
			break
		}
	}
	done, err := f.s.Get(context.Background(), r.Record.ID)
	if err != nil || done.Counts.Skipped != 4 || done.Counts.Succeeded != 6 || f.tr.count() != 6 {
		t.Fatalf("dispatches=%d result=%+v err=%v", f.tr.count(), done.Counts, err)
	}
}
