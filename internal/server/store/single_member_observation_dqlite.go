//go:build linux && dqlite

package store

import (
	"context"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// A store that opened a cluster's database as a single member serves
// without a session: the first member's process until its next start after
// others joined, and every process of a cluster that still has one member.
// Other members and the cluster's commands ask each member the same
// observation questions, so such a store answers them too.
// singleMemberDatabases holds its database handle.
var singleMemberDatabases sync.Map

type singleMemberDatabase struct {
	db      *cluster.Database
	raftDir string
}

// ErrClusterUpgradeSingleMember refuses an upgrade step that changes the
// cluster on a member that runs as a single member: such a step belongs to
// an established member's session, which owns the migration.
var ErrClusterUpgradeSingleMember = refuseClusterUpgrade("this member runs as a single member: with more members in the cluster, restart it once before it takes part in an upgrade; a cluster of one member applies its migrations when its new program starts and needs no upgrade step")

// SingleMemberObservation answers the observation questions an established
// member's session answers (the quorum facts, the discovery of the members,
// the upgrade status) for a store that opened the cluster's database as a
// single member. It reads through the same functions and changes nothing.
type SingleMemberObservation struct {
	st      *Store
	db      *cluster.Database
	raftDir string
	sets    []MigrationSet
}

// SingleMemberObservation is the store's observation when the store opened
// a database with a cluster identity itself. sets are the executable's
// extension migration sets, for the upgrade status.
func (s *Store) SingleMemberObservation(sets []MigrationSet) (*SingleMemberObservation, bool) {
	value, ok := singleMemberDatabases.Load(s)
	if !ok {
		return nil, false
	}
	single := value.(singleMemberDatabase)
	return &SingleMemberObservation{st: s, db: single.db, raftDir: single.raftDir, sets: append([]MigrationSet(nil), sets...)}, true
}

// Close satisfies the session's shape; the store owns the database.
func (o *SingleMemberObservation) Close() error { return nil }

// ReadLocalQuorumFactsForCluster observes the cluster for a member that
// names this database's cluster.
func (o *SingleMemberObservation) ReadLocalQuorumFactsForCluster(ctx context.Context, expected ClusterIdentity, requesterKey string) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
	if err := observedClusterIdentity(o.db, expected); err != nil {
		return false, MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
	}
	return readLocalQuorumFacts(ctx, o.db, o.db.Address, o.raftDir, requesterKey)
}

// ReadQuorumDiscoveryForCluster reads the members' public dial hints.
func (o *SingleMemberObservation) ReadQuorumDiscoveryForCluster(ctx context.Context, expected ClusterIdentity, requesterKey string) (ClusterAdmission, []ClusterTransportMember, error) {
	return readQuorumDiscovery(ctx, o.db, expected, requesterKey)
}

// ClusterUpgrade answers the upgrade status for an admitted member and
// refuses every step that changes the cluster with
// ErrClusterUpgradeSingleMember, returning the status with the refusal.
func (o *SingleMemberObservation) ClusterUpgrade(ctx context.Context, expected ClusterIdentity, requesterKey, action string) (status ClusterUpgradeStatus, leader, migrating bool, err error) {
	current, err := o.db.Leader(ctx)
	if err != nil {
		return status, false, false, err
	}
	leader = current.ID == o.db.ID
	if err := admitClusterUpgradeCaller(ctx, o.st, expected, requesterKey); err != nil {
		return status, leader, false, err
	}
	status, err = o.st.ClusterUpgradeFor(o.sets).Status(ctx)
	if err == nil && action != "status" {
		err = ErrClusterUpgradeSingleMember
	}
	return status, leader, false, err
}

// bindSingleMemberDatabase records the handle of a database this store
// opened with a cluster identity, until the store closes it.
func bindSingleMemberDatabase(st *Store, db *cluster.Database, raftDir string) {
	singleMemberDatabases.Store(st, singleMemberDatabase{db: db, raftDir: raftDir})
	closeDatabase := st.closeBackend
	st.closeBackend = func() error {
		singleMemberDatabases.Delete(st)
		return closeDatabase()
	}
}
