//go:build linux && dqlite

package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// The facts read and the discovery read bind the database's members with the
// same function as a grant. Beside a member that is revoked and still in the
// database both answer for the others; with the revoked member as the
// provider, or as the requester, both refuse. The database is the store's
// SQL fixture; the member list is given.
func TestClosedClusterReadersBesideARevokedLiveMember(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	live := []cluster.Member{
		{ID: 1, Address: admissions[0].Address, Role: "voter"},
		{ID: 2, Address: admissions[1].Address, Role: "voter"},
		{ID: 3, Address: admissions[2].Address, Role: "voter"},
	}
	provider, requester, revoked := admissions[0].KeyFingerprint, admissions[1].KeyFingerprint, admissions[2].KeyFingerprint
	proof := func(leader uint64) cluster.QuorumProof {
		return cluster.QuorumProof{Term: 2, Index: 9, ConfigurationIndex: 3, LeaderID: leader, Members: live}
	}
	discover := func(providerMember cluster.Member, providerKey, requesterKey string) ([]ClusterTransportMember, error) {
		pool, err := st.db.DB()
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		_, members, err := readClosedClusterDiscovery(t.Context(), tx, id, providerMember, providerKey, requesterKey, live)
		return members, err
	}
	if _, _, got, err := readClosedClusterFactsDB(t.Context(), st.db, id, provider, requester, proof(1)); err != nil || len(got) != 3 {
		t.Fatalf("facts read with three admitted members: %d rows %v", len(got), err)
	}
	if members, err := discover(live[0], provider, requester); err != nil || len(members) != 3 {
		t.Fatalf("discovery read with three admitted members: %+v %v", members, err)
	}
	if _, err := st.RevokeClusterAdmission(t.Context(), id.ClusterID, revoked); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := readClosedClusterFactsDB(t.Context(), st.db, id, provider, requester, proof(1)); err != nil {
		t.Fatalf("facts read beside a revoked live member: %v", err)
	}
	members, err := discover(live[0], provider, requester)
	if err != nil || len(members) != 2 {
		t.Fatalf("discovery read beside a revoked live member: %+v %v", members, err)
	}
	for _, member := range members {
		if member.ID == 3 || member.KeyFingerprint == revoked {
			t.Fatalf("the revoked member is in the discovery answer: %+v", members)
		}
	}

	// The revoked member leads the database: it is not bound, so it is no
	// provider of facts, and it cannot ask either.
	if _, _, _, err := readClosedClusterFactsDB(t.Context(), st.db, id, revoked, requester, proof(3)); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("facts read with the revoked member as provider: %v", err)
	}
	if _, _, _, err := readClosedClusterFactsDB(t.Context(), st.db, id, provider, revoked, proof(1)); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("facts read for the revoked member: %v", err)
	}
	if members, err := discover(live[2], revoked, requester); err == nil || members != nil {
		t.Fatalf("discovery read with the revoked member as provider: %+v %v", members, err)
	}
	if members, err := discover(live[0], provider, revoked); err == nil || members != nil {
		t.Fatalf("discovery read for the revoked member: %+v %v", members, err)
	}
}
