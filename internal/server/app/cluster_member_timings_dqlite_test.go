//go:build linux && dqlite

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// The other members' timing is read from each member itself. A member that
// cannot be asked carries the reason and no tier, and without the cluster's
// membership nothing is returned: the caller must refuse, not guess.
func TestClusterMemberTimingsNeverGuessAMembersTier(t *testing.T) {
	f := newStatusProofFixture(t)
	previous := statusQuorumFetch
	t.Cleanup(func() { statusQuorumFetch = previous })

	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{}, false
	}
	if timings, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, time.Second); err == nil || timings != nil ||
		!strings.Contains(err.Error(), "membership could not be read") {
		t.Fatalf("without the membership: %v, %v", timings, err)
	}

	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{LeaderID: 3, Members: []enrollment.ClusterMember{
			{ID: 1, Address: f.self.address, Role: "voter", EnrollmentOrigin: "https://127.0.0.1:1"},
			{ID: 2, Address: f.follower.address, Role: "voter"},
			// Nothing listens on port 1.
			{ID: 3, Address: f.leader.address, Role: "voter", EnrollmentOrigin: "https://127.0.0.1:1"},
		}}, true
	}
	timings, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, time.Second)
	if err != nil || len(timings) != 2 {
		t.Fatalf("timings %+v, %v; want the two other members", timings, err)
	}
	if timings[0].ID != 2 || timings[0].Address != f.follower.address || timings[0].Tier != "" || !strings.Contains(timings[0].Unread, "names no enrollment listener") {
		t.Fatalf("a member without a listener: %+v", timings[0])
	}
	if timings[1].ID != 3 || timings[1].Tier != "" || !strings.Contains(timings[1].Unread, "it did not answer") {
		t.Fatalf("a member that does not answer: %+v", timings[1])
	}
	if _, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, 0); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
	if _, err := ClusterMemberTimings(context.Background(), Config{DataDir: t.TempDir()}, time.Second); err == nil {
		t.Fatal("an installation without a member identity was asked for its peers")
	}
}

// Which members are running is read from their database ports, with no
// leader's help: a member that listens is listed although nobody leads, and
// one that is stopped is not.
func TestClusterMembersListeningNeedsNoLeader(t *testing.T) {
	f := newStatusProofFixture(t)
	previous := statusQuorumFetch
	t.Cleanup(func() { statusQuorumFetch = previous })
	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		t.Error("the leader's membership was asked for")
		return enrollment.ClusterQuorumFacts{}, false
	}
	listening, known, err := ClusterMembersListening(context.Background(), Config{DataDir: f.root}, time.Second)
	if err != nil || known != 2 || len(listening) != 2 {
		t.Fatalf("listening %v of %d known, %v; want the two other members", listening, known, err)
	}
	for _, address := range listening {
		if address != f.follower.address && address != f.leader.address {
			t.Fatalf("listed %s, which is neither of the other members", address)
		}
	}
	// One of the two stops: the other is still found, alone.
	f.follower.stop()
	listening, known, err = ClusterMembersListening(context.Background(), Config{DataDir: f.root}, time.Second)
	if err != nil || known != 2 || len(listening) != 1 || listening[0] != f.leader.address {
		t.Fatalf("with one member stopped: listening %v of %d known, %v; want only %s", listening, known, err, f.leader.address)
	}
	f.leader.stop()
	if listening, known, err = ClusterMembersListening(context.Background(), Config{DataDir: f.root}, time.Second); err != nil || known != 2 || len(listening) != 0 {
		t.Fatalf("with both stopped: listening %v of %d known, %v", listening, known, err)
	}
	if _, _, err := ClusterMembersListening(context.Background(), Config{DataDir: f.root}, 0); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}
