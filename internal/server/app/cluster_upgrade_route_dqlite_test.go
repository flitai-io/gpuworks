//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// upgradeStepSession answers the upgrade steps the way the real session
// does for what the route adds: it checks the caller's admission, refuses a
// step it is told to refuse, and reports whether this member leads after it.
type upgradeStepSession struct {
	closedFactsRouteSession
	admitted string
	leader   bool
	refuse   error
	actions  []string
}

func (s *upgradeStepSession) ClusterUpgrade(_ context.Context, _ store.ClusterIdentity, requester, action string) (store.ClusterUpgradeStatus, bool, bool, error) {
	status := store.ClusterUpgradeStatus{Phase: store.MaintenancePhaseState{Phase: store.MaintenanceServing, Revision: 2}, Applied: 5, Known: 5}
	if requester != s.admitted {
		return status, false, false, store.ErrClusterAdmissionRejected
	}
	s.actions = append(s.actions, action)
	if s.refuse != nil {
		return status, s.leader, false, s.refuse
	}
	if action == enrollment.ClusterUpgradeLead {
		s.leader = true
	}
	return status, s.leader, false, nil
}

func upgradeStepCall(t *testing.T, handler http.Handler, peer *x509.Certificate, action string) (int, enrollment.ClusterUpgradeResponse, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, enrollment.ClusterUpgradePath, strings.NewReader(`{"action":"`+action+`"}`))
	if peer != nil {
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var response enrollment.ClusterUpgradeResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("answer %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, response, recorder.Body.String()
}

// The status step says how many agent connections the member holds, so the
// upgrade command can replace the members that hold none first. The count is
// left out when the member cannot say, never reported as zero.
func TestClusterUpgradeStatusReportsHeldAgentConnections(t *testing.T) {
	fixture := newClosedFactsTLSFixture(t)
	session := &upgradeStepSession{admitted: fixture.requesterKey}
	held, known := 0, false
	handler, err := clusterUpgradeHandler(fixture.bundle, session, newMaterialRequestTracker(), func() (int, bool) { return held, known })
	if err != nil {
		t.Fatal(err)
	}
	code, response, body := upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeStatus)
	if code != http.StatusOK || response.AgentConnections != nil || strings.Contains(body, "agent_connections") {
		t.Fatalf("a member that cannot say answered %d %s; want no count", code, body)
	}
	// The answer names the tier this process runs, not what the record
	// may say by now.
	if response.StorageTier != string(cluster.ProcessStorageTier()) || !strings.Contains(body, `"storage_tier":"`+string(cluster.ProcessStorageTier())+`"`) {
		t.Fatalf("the member answered %s; want the tier it runs, %s", body, cluster.ProcessStorageTier())
	}
	// Beside it, the tier the installation records now: a record changed
	// since the start shows as a different one, and a member that records
	// none records the standard tier.
	if response.RecordedStorageTier != "standard" {
		t.Fatalf("a member without a record answered %s; want the standard tier recorded", body)
	}
	fixture.bundle.dataDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(fixture.bundle.dataDir, "cluster-enrollment"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.bundle.dataDir, "cluster-enrollment", "config.json"), []byte(`{"storage_tier":"slow-storage@100ms"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, response, body = upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeStatus); response.RecordedStorageTier != "slow-storage@100ms" ||
		!strings.Contains(body, `"recorded_storage_tier":"slow-storage@100ms"`) || response.StorageTier != string(cluster.ProcessStorageTier()) {
		t.Fatalf("after the record changed the member answered %s", body)
	}
	known = true
	code, response, body = upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeStatus)
	if code != http.StatusOK || response.AgentConnections == nil || *response.AgentConnections != 0 || !strings.Contains(body, `"agent_connections":0`) {
		t.Fatalf("a member that holds none answered %d %s; want a literal zero", code, body)
	}
	held = 4
	if _, response, body = upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeStatus); response.AgentConnections == nil || *response.AgentConnections != 4 {
		t.Fatalf("a member that holds four answered %s", body)
	}

	// A handler without a count source (a member with no business) leaves
	// the count out.
	bare, err := clusterUpgradeHandler(fixture.bundle, session, newMaterialRequestTracker(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, response, body = upgradeStepCall(t, bare, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeStatus); code != http.StatusOK || response.AgentConnections != nil {
		t.Fatalf("a member without business answered %d %s", code, body)
	}
}

// The lead step is a member-authenticated step like the others: an admitted
// member's request makes this member lead and the answer says so; a refusal
// is in the answer; a caller without an admitted key gets nothing.
func TestClusterUpgradeLeadStepIsServedToAdmittedMembersOnly(t *testing.T) {
	fixture := newClosedFactsTLSFixture(t)
	session := &upgradeStepSession{admitted: fixture.requesterKey}
	handler, err := clusterUpgradeHandler(fixture.bundle, session, newMaterialRequestTracker(), nil)
	if err != nil {
		t.Fatal(err)
	}
	code, response, body := upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeLead)
	if code != http.StatusOK || response.Action != "lead" || !response.Leader || response.Refused != "" || response.Error != "" {
		t.Fatalf("lead by an admitted member = %d %s", code, body)
	}

	session.leader, session.refuse = false, errors.Join(store.ErrClusterUpgradeRefused, errors.New("the phase is upgrading"))
	code, response, body = upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeLead)
	if code != http.StatusOK || response.Leader || !strings.Contains(response.Refused, "the phase is upgrading") || response.Error != "" {
		t.Fatalf("a refused lead = %d %s; want the refusal in the answer", code, body)
	}
	session.refuse = errors.New("database leadership was not handed over: not leader")
	if _, response, body = upgradeStepCall(t, handler, fixture.requesterPair.Leaf, enrollment.ClusterUpgradeLead); response.Refused != "" || !strings.Contains(response.Error, "was not handed over") {
		t.Fatalf("a failed lead = %s; want the failure in the answer", body)
	}

	session.refuse, session.actions = nil, nil
	for name, peer := range map[string]*x509.Certificate{"no certificate": nil, "a member that is not admitted": fixture.otherPair.Leaf} {
		if code, _, body := upgradeStepCall(t, handler, peer, enrollment.ClusterUpgradeLead); code != http.StatusForbidden {
			t.Fatalf("%s: lead = %d %s; want 403", name, code, body)
		}
	}
	if len(session.actions) != 0 {
		t.Fatalf("steps %v ran for callers that were refused", session.actions)
	}
	if code, _, body := upgradeStepCall(t, handler, fixture.requesterPair.Leaf, "takeover"); code != http.StatusBadRequest {
		t.Fatalf("an unknown step = %d %s; want 400", code, body)
	}
}

// What a member reports as held: none while its business is closed, the
// relay's count while a generation serves, and nothing it can vouch for
// while that generation is being withdrawn.
func TestHeldAgentConnectionsFollowTheBusinessGeneration(t *testing.T) {
	member := &App{}
	if held, known := member.heldAgentConnections(); held != 0 || !known {
		t.Fatalf("a member with closed business = %d, %v; want a known zero", held, known)
	}
	generation := &closedBusinessGeneration{owner: member, business: &App{}}
	if err := member.retainClosedBusiness(generation); err != nil {
		t.Fatal(err)
	}
	if _, known := member.heldAgentConnections(); known {
		t.Fatal("a generation that has not started its sessions vouched for a count")
	}
	forward := newMemberForward(nil, nil, nil, nil, nil)
	forward.agents = &memberAgentRelay{forward: forward, relay: hub.NewAgentRelay(nil, nil, nil)}
	generation.business.ha = &memberRuntime{forward: forward}
	generation.started = true
	if held, known := member.heldAgentConnections(); held != 0 || !known {
		t.Fatalf("a serving generation without agents = %d, %v", held, known)
	}
	generation.stopping = true
	if _, known := member.heldAgentConnections(); known {
		t.Fatal("a generation being withdrawn vouched for a count")
	}
}
