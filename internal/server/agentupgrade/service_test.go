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
	// The words alone, from a session that is not the baseline, are not a
	// rollback: the hub turns an agent's report into the code.
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "rolled-back-session", Version: "old", State: "failed", Error: "rolled back: new binary never registered", At: f.now})
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase == "failed" {
		t.Fatalf("an uncoded observation was read as a rollback: %+v", i)
	}
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "rolled-back-session", Version: "old", State: "failed", Error: "rolled back: new binary never registered", Code: proto.ErrorAgentUpgradeRolledBack, At: f.now})
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "failed" || i.Reason != "rolled_back" || i.Error != "rolled back: new binary never registered" {
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
	// The timeout stays in the history, and the batch goes on by itself: the
	// attempt is settled, so nothing holds the slot and n2 is dispatched.
	if n2 := f.item(t, r.Record.ID, "n2"); !activePhase(n2.Phase) {
		t.Fatalf("the batch did not go on to n2 once n1 was settled: %+v", n2)
	}
	if run, err := f.st.AgentUpgradeRun(r.Record.ID); err != nil || run.PauseReason != "" {
		t.Fatalf("run = %+v, %v; an unconfirmed attempt must not pause its batch", run, err)
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

// A node the platform no longer has cannot confirm or fail its attempt, and
// no upgrade can still be running on it: an unconfirmed attempt is settled
// as node_removed by the system, the slot is free for other nodes, and the
// history keeps the timeout. A node that is merely offline keeps blocking.
func TestRemovedNodeSettlesItsUnconfirmedAttempt(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "first", []string{"n1"}, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	f.tr.mu.Lock()
	delete(f.tr.connections, "n1") // gone offline
	f.tr.mu.Unlock()
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		t.Fatalf("an offline node's attempt was settled: %+v", i)
	}
	if err := f.st.DeleteNode("n1"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase != "unconfirmed" || i.Reason != "confirmation_timeout" || i.VerifiedAt == nil || i.VerifiedBy != "system" || i.VerificationCode != ReasonNodeRemoved {
		t.Fatalf("removed node's attempt = %+v", i)
	}
	f.create(t, "second", []string{"n2"}, 0)
}

// An attempt still in flight when its node is removed is not settled early:
// a deleted Kubernetes or Slurm node's agent keeps running and enrolls again,
// and the upgrade it was given may still be applying. It waits out its
// deadline like any attempt, and is settled then; its batch goes on to the
// next node without waiting for an administrator.
func TestRemovedNodeSettlesItsAttemptOnlyAfterTheDeadline(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "first", []string{"n1", "n2"}, 0)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); !activePhase(i.Phase) {
		t.Fatalf("n1 not in flight: %+v", i)
	}
	if err := f.st.DeleteNode("n1"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); !activePhase(i.Phase) || i.VerifiedAt != nil {
		t.Fatalf("an attempt in flight was settled before its deadline: %+v", i)
	}
	if b, err := f.st.AgentUpgradeBlocker(f.s.heldSince()); err != nil || b == nil {
		t.Fatalf("the slot was freed before the deadline: %v", err)
	}
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase != "unconfirmed" || i.Reason != "confirmation_timeout" || i.VerifiedAt == nil || i.VerificationCode != ReasonNodeRemoved {
		t.Fatalf("after the deadline = %+v", i)
	}
	run, err := f.st.AgentUpgradeRun(r.Record.ID)
	if err != nil || run.PauseReason != "" {
		t.Fatalf("run = %+v, %v; an unconfirmed attempt must not pause its batch", run, err)
	}
	// The removed node's attempt is settled, so it holds nothing, and the
	// batch went on to its next node in the same pass.
	if n2 := f.item(t, r.Record.ID, "n2"); !activePhase(n2.Phase) {
		t.Fatalf("the batch did not go on to n2: %+v", n2)
	}
}

// An attempt that missed its deadline holds the slot only for
// UnconfirmedHold: by then its agent has confirmed or rolled back, so other
// nodes may be upgraded; the attempt stays unconfirmed and unverified until
// its node says how it ended.
func TestUnconfirmedAttemptHoldsTheSlotOnlyForAWhile(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "first", []string{"n1"}, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	f.tr.mu.Lock()
	delete(f.tr.connections, "n1")
	f.tr.mu.Unlock()
	p, _ := f.s.Preview(context.Background(), []string{"n2"})
	_, err := f.s.Create(context.Background(), CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "early"})
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "AGENT_UPGRADE_ACTIVE" {
		t.Fatalf("within the hold = %v; want the slot held", err)
	}
	if _, err := f.s.BeginServerUpgrade(); err == nil {
		t.Fatal("the server upgrade went ahead within the hold")
	}
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	if done, err := f.s.BeginServerUpgrade(); err != nil {
		t.Fatalf("server upgrade after the hold = %v", err)
	} else {
		done(false)
	}
	second := f.create(t, "second", []string{"n2"}, 0)
	f.tick(t)
	if i := f.item(t, second.Record.ID, "n2"); !activePhase(i.Phase) {
		t.Fatalf("another node was not upgraded after the hold: %+v", i)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		t.Fatalf("the unconfirmed attempt was rewritten: %+v", i)
	}
}

// A node back on a new session at its old version did not take the upgrade:
// once watched for OldVersionSettle its unconfirmed attempt is settled as
// reconnected_old_version. One still in flight is left to its deadline.
func TestNodeBackOnItsOldVersionSettlesItsUnconfirmedAttempt(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "first", []string{"n1"}, 0)
	f.tick(t)
	f.tr.set("n1", Connection{SessionID: "back-n1", Version: "old", Ready: true, Caps: []string{proto.CapUpgradePush}})
	f.now = f.now.Add(f.s.cfg.OldVersionSettle + time.Second)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); !activePhase(i.Phase) {
		t.Fatalf("an attempt in flight was settled by an old-version session: %+v", i)
	}
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		t.Fatalf("settled before it was watched: %+v", i)
	}
	f.now = f.now.Add(f.s.cfg.OldVersionSettle - time.Second)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.VerifiedAt != nil {
		t.Fatalf("settled before OldVersionSettle: %+v", i)
	}
	f.now = f.now.Add(2 * time.Second)
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.VerifiedAt == nil || i.VerifiedBy != "system" || i.VerificationCode != ReasonReconnectedOldVersion {
		t.Fatalf("back on the old version = %+v", i)
	}
	if len(f.s.oldVersion) != 0 {
		t.Fatalf("watch left behind: %v", f.s.oldVersion)
	}
}

// A rolled-back agent reports so just after it registers: that report, with
// the agent's own words, settles the attempt, not the old-version watch.
func TestRollbackReportWinsOverTheOldVersionWatch(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "first", []string{"n1"}, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	f.tr.set("n1", Connection{SessionID: "back-n1", Version: "old", Ready: true, Caps: []string{proto.CapUpgradePush}})
	f.tick(t)
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "back-n1", Version: "old", State: "failed", Code: proto.ErrorAgentUpgradeRolledBack, Error: "new binary did not register", At: f.now})
	f.now = f.now.Add(f.s.cfg.OldVersionSettle + time.Second)
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if i.VerificationCode != "rolled_back" || i.VerificationNote != "new binary did not register" {
		t.Fatalf("rolled back = %+v", i)
	}
}

// Once an unconfirmed attempt no longer holds the slot its node may be
// upgraded again; a failure the agent then reports is the new attempt's, not
// the old one's.
func TestAFailureIsTheAttemptInFlightsNotAnOlderUnconfirmedOnes(t *testing.T) {
	f := newFixture(t, 1, true)
	old := f.create(t, "old", []string{"n1"}, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	again := f.create(t, "again", []string{"n1"}, 0)
	f.tick(t)
	if i := f.item(t, again.Record.ID, "n1"); !activePhase(i.Phase) {
		t.Fatalf("the node was not upgraded again: %+v", i)
	}
	f.s.Observe(Observation{NodeUUID: "n1", SessionID: "old-n1", Version: "old", State: "failed", Error: "no space left", At: f.now})
	f.tick(t)
	if i := f.item(t, again.Record.ID, "n1"); i.Phase != "failed" || i.Error != "no space left" {
		t.Fatalf("the new attempt = %+v; want its own failure", i)
	}
	// The old attempt is settled by what the node itself shows (still
	// connected on its old session and version past the hold), never by the
	// new attempt's failure.
	if i := f.item(t, old.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerificationCode != ReasonStillOnOldVersion || i.VerificationNote != "" {
		t.Fatalf("the old attempt took the new one's failure: %+v", i)
	}
}

// One node that never confirms does not stop its batch: after the hold the
// next node is dispatched with nobody continuing, verifying or noting
// anything, and the unconfirmed attempt stays in the history as it ended.
func TestBatchGoesOnPastANodeThatNeverConfirms(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "all", nil, 0)
	f.tick(t)
	f.tr.mu.Lock()
	delete(f.tr.connections, "n1") // n1 drops off and never comes back
	f.tr.mu.Unlock()
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		t.Fatalf("n1 = %+v; want unconfirmed and unverified", i)
	}
	if n2 := f.item(t, r.Record.ID, "n2"); n2.Phase != "queued" {
		t.Fatalf("n2 was dispatched within the hold: %+v", n2)
	}
	if run, err := f.s.Get(context.Background(), r.Record.ID); err != nil || run.State != "running" || run.Record.PauseReason != "" {
		t.Fatalf("run = %+v, %v; want running, not paused", run, err)
	}
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	f.tick(t)
	if n2 := f.item(t, r.Record.ID, "n2"); !activePhase(n2.Phase) {
		t.Fatalf("the batch did not go on to n2 after the hold: %+v", n2)
	}
	f.tr.set("n2", Connection{SessionID: "new-n2", Version: "target", Ready: true})
	f.tick(t)
	f.tick(t)
	if n3 := f.item(t, r.Record.ID, "n3"); !activePhase(n3.Phase) {
		t.Fatalf("the batch did not go on to n3: %+v", n3)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.Reason != "confirmation_timeout" {
		t.Fatalf("n1's history was rewritten: %+v", i)
	}
}

// A server upgrade waits only for what is really going on, and its refusal
// says what and for how long: an attempt in flight or within its hold (both
// end by themselves), or a batch that will still dispatch nodes. A queue
// whose batch is stopped, paused or interrupted dispatches nothing, so the
// upgrade does not wait for an administrator to settle it.
func TestServerUpgradeWaitsOnlyForUpgradesThatAreGoingOn(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "all", nil, 0)
	f.tick(t)
	_, err := f.s.BeginServerUpgrade()
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "AGENT_UPGRADE_ACTIVE" {
		t.Fatalf("in flight = %v; want a refusal", err)
	}
	// n1 is in flight and two more nodes wait behind it: the server waits
	// for the batch, so the refusal must not promise n1's few minutes.
	if _, run, queued, _, ok := refusal.ServerUpgradeWait(); !ok || run != r.Record.ID || queued != 2 {
		t.Fatalf("in flight with a queue waits for run=%d queued=%d ok=%v; want the batch with 2 queued", run, queued, ok)
	}

	f.tr.mu.Lock()
	delete(f.tr.connections, "n1")
	f.tr.mu.Unlock()
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t) // n1 unconfirmed, within its hold
	_, err = f.s.BeginServerUpgrade()
	if !errors.As(err, &refusal) {
		t.Fatalf("within the hold = %v; want a refusal", err)
	}
	if _, run, queued, _, ok := refusal.ServerUpgradeWait(); !ok || run != r.Record.ID || queued != 2 {
		t.Fatalf("within the hold with a queue waits for run=%d queued=%d ok=%v; want the batch with 2 queued", run, queued, ok)
	}

	// Past the hold the unconfirmed attempt holds nothing, though nobody
	// verified it; what is left is the batch's live queue.
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	_, err = f.s.BeginServerUpgrade()
	if !errors.As(err, &refusal) {
		t.Fatalf("live queue = %v; want a refusal", err)
	}
	if _, run, queued, _, ok := refusal.ServerUpgradeWait(); !ok || run != r.Record.ID || queued != 2 {
		t.Fatalf("live queue waits for run=%d queued=%d ok=%v; want %d with 2 queued", run, queued, ok, r.Record.ID)
	}

	// A paused batch dispatches nothing: its queue does not hold the server.
	if err := f.st.PauseAgentUpgrade(r.Record.ID, "artifact_unavailable"); err != nil {
		t.Fatal(err)
	}
	done, err := f.s.BeginServerUpgrade()
	if err != nil {
		t.Fatalf("paused queue held the server upgrade: %v", err)
	}
	done(false)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		t.Fatalf("n1 = %+v; the server upgrade must not need it verified", i)
	}
}

// A batch an older server paused because one node went unconfirmed goes on
// when this server starts; a batch paused for another reason stays paused.
func TestBatchPausedForVerificationByAnOlderServerGoesOn(t *testing.T) {
	f := newFixture(t, 3, true)
	old := f.create(t, "old", []string{"n1", "n2"}, 0)
	if err := f.st.PauseAgentUpgrade(old.Record.ID, "confirmation_required"); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	run, err := restarted.Get(context.Background(), old.Record.ID)
	if err != nil || run.Record.PauseReason != "" || run.State != "running" {
		t.Fatalf("run = %+v, %v; want it going on", run, err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if i := f.item(t, old.Record.ID, "n1"); !activePhase(i.Phase) {
		t.Fatalf("the batch did not dispatch its next node: %+v", i)
	}

	if err := f.st.PauseAgentUpgrade(old.Record.ID, "artifact_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.st, f.cfg); err != nil {
		t.Fatal(err)
	}
	if run, err := f.st.AgentUpgradeRun(old.Record.ID); err != nil || run.PauseReason != "artifact_unavailable" {
		t.Fatalf("run = %+v, %v; a batch paused for missing artifacts must stay paused", run, err)
	}
}

// With nothing queued behind it, an attempt names its node and at most how
// long it still holds the slot: until its current deadline plus the hold
// while in flight, what is left of the hold once unconfirmed.
func TestServerUpgradeRefusalNamesTheLastNodeAndHowLongItCanLast(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "one", nil, 0)
	f.tick(t)
	_, err := f.s.BeginServerUpgrade()
	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("in flight = %v; want a refusal", err)
	}
	// The push has been handed over, so the attempt waits ConfirmTimeout for
	// the agent to register, then holds the slot for the hold.
	want := int((f.s.cfg.ConfirmTimeout + f.s.cfg.UnconfirmedHold + time.Minute - 1) / time.Minute)
	if node, run, queued, minutes, ok := refusal.ServerUpgradeWait(); !ok || node != "n1" || run != r.Record.ID || queued != 0 || minutes != want {
		t.Fatalf("in flight waits for node=%q run=%d queued=%d minutes=%d ok=%v; want n1 for %d minutes", node, run, queued, minutes, ok, want)
	}
	f.tr.mu.Lock()
	delete(f.tr.connections, "n1")
	f.tr.mu.Unlock()
	f.now = f.now.Add(f.s.cfg.TransferTimeout + f.s.cfg.ConfirmTimeout)
	f.tick(t)
	_, err = f.s.BeginServerUpgrade()
	if !errors.As(err, &refusal) {
		t.Fatalf("within the hold = %v; want a refusal", err)
	}
	if node, _, queued, minutes, ok := refusal.ServerUpgradeWait(); !ok || node != "n1" || queued != 0 || minutes != 5 {
		t.Fatalf("within the hold waits for node=%q queued=%d minutes=%d ok=%v; want n1 for 5 minutes", node, queued, minutes, ok)
	}
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	done, err := f.s.BeginServerUpgrade()
	if err != nil {
		t.Fatalf("after the hold = %v; an unverified outcome must not hold the server upgrade", err)
	}
	done(false)
}

// A queue left on an interrupted batch (the build changed under it) or on a
// stopped one dispatches nothing, so neither holds a server upgrade.
func TestQueueOfAnInterruptedBatchDoesNotHoldTheServerUpgrade(t *testing.T) {
	f := newFixture(t, 2, true)
	r := f.create(t, "all", nil, 0)
	if err := f.st.InterruptAgentUpgrade(r.Record.ID, "artifact_changed"); err != nil {
		t.Fatal(err)
	}
	if items, err := f.st.AgentUpgradeItems(r.Record.ID); err != nil || len(items) != 2 || items[0].Phase != "queued" || items[1].Phase != "queued" {
		t.Fatalf("items = %+v, %v; want both still queued on the interrupted batch", items, err)
	}
	done, err := f.s.BeginServerUpgrade()
	if err != nil {
		t.Fatalf("an interrupted batch's queue held the server upgrade: %v", err)
	}
	done(false)
}

// Two nodes in a row with an unknown outcome pause the batch with its own
// reason: one silent node is that node's matter, two say something about the
// build or the network, and going on would take the fleet's agents away one
// by one. A node later seen on the target did succeed and does not count.
func TestTwoUnknownOutcomesInARowPauseTheBatch(t *testing.T) {
	silent := func(f *fixture, uuid string) {
		f.tr.mu.Lock()
		delete(f.tr.connections, uuid)
		f.tr.mu.Unlock()
	}
	t.Run("two in a row pause", func(t *testing.T) {
		f := newFixture(t, 3, true)
		r := f.create(t, "all", nil, 0)
		f.tick(t)
		silent(f, "n1")
		f.now = f.now.Add(3 * time.Minute)
		f.tick(t)
		f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
		f.tick(t)
		if i := f.item(t, r.Record.ID, "n2"); !activePhase(i.Phase) {
			t.Fatalf("the batch did not go on past one silent node: %+v", i)
		}
		silent(f, "n2")
		f.now = f.now.Add(3 * time.Minute)
		f.tick(t)
		run, err := f.s.Get(context.Background(), r.Record.ID)
		if err != nil || run.State != "paused" || run.Record.PauseReason != PauseRepeatedUnknownOutcome {
			t.Fatalf("run = %+v, %v; want paused for repeated unknown outcomes", run, err)
		}
		f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
		f.tick(t)
		if i := f.item(t, r.Record.ID, "n3"); i.Phase != "queued" {
			t.Fatalf("the paused batch went on to n3: %+v", i)
		}
		// The pause is this server's own decision, not the older rule's:
		// a restart does not undo it.
		if _, err := New(f.st, f.cfg); err != nil {
			t.Fatal(err)
		}
		if saved, err := f.st.AgentUpgradeRun(r.Record.ID); err != nil || saved.PauseReason != PauseRepeatedUnknownOutcome {
			t.Fatalf("after a restart run = %+v, %v; want still paused", saved, err)
		}
		// Continuing offers the node not yet tried and the two whose outcome is unknown.
		p, err := f.s.ContinuePreview(context.Background(), r.Record.ID)
		if err != nil || len(p.NodeUUIDs) != 3 {
			t.Fatalf("continue preview = %+v, %v; want all three nodes", p.NodeUUIDs, err)
		}
	})
	t.Run("a late success in between does not count", func(t *testing.T) {
		f := newFixture(t, 3, true)
		r := f.create(t, "all", nil, 0)
		f.tick(t)
		f.now = f.now.Add(3 * time.Minute)
		f.tr.set("n1", Connection{SessionID: "late-n1", Version: "target", Ready: true})
		f.tick(t) // n1 times out, then is seen on the target; n2 is dispatched
		if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerificationCode != "target_registered" {
			t.Fatalf("n1 = %+v", i)
		}
		silent(f, "n2")
		f.now = f.now.Add(3 * time.Minute)
		f.tick(t)
		if run, err := f.st.AgentUpgradeRun(r.Record.ID); err != nil || run.PauseReason != "" {
			t.Fatalf("run = %+v, %v; one unknown outcome after a late success must not pause", run, err)
		}
	})
}

// An unknown outcome past its hold does not keep its node, or its batch,
// shown as being upgraded; and a node that stayed connected on its old
// session and version is settled as not having taken the upgrade, so the
// attempt does not stay open for as long as the node stays up.
func TestUnknownOutcomePastItsHoldHoldsNothingAndSettlesByWhatTheNodeShows(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "one", nil, 0)
	f.tick(t)
	f.now = f.now.Add(3 * time.Minute)
	f.tick(t)
	nodes, err := f.st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := f.s.Fleet(context.Background(), nodes)
	if err != nil || fleet.RunID != r.Record.ID || fleet.Eligibility["n1"] != "busy" {
		t.Fatalf("within the hold fleet = %+v, %v; want n1 busy in its batch", fleet, err)
	}
	f.now = f.now.Add(f.s.cfg.UnconfirmedHold + time.Second)
	fleet, err = f.s.Fleet(context.Background(), nodes)
	if err != nil || fleet.RunID != 0 || fleet.Eligibility["n1"] == "busy" {
		t.Fatalf("past the hold fleet = %+v, %v; want nothing running and n1 free", fleet, err)
	}
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "unconfirmed" || i.VerifiedBy != "system" || i.VerificationCode != ReasonStillOnOldVersion {
		t.Fatalf("n1 = %+v; want settled as still on its old version", i)
	}
}
