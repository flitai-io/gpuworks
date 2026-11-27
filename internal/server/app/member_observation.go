//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What the member observation handlers read. An established member's
// session and a single member's open store both provide all three.
type (
	quorumFactsReader interface {
		ReadLocalQuorumFactsForCluster(context.Context, store.ClusterIdentity, string) (bool, store.MaintenancePhaseState, []store.ClusterAdmission, cluster.QuorumProof, error)
	}
	quorumDiscoveryReader interface {
		ReadQuorumDiscoveryForCluster(context.Context, store.ClusterIdentity, string) (store.ClusterAdmission, []store.ClusterTransportMember, error)
	}
	clusterUpgradeStepper interface {
		ClusterUpgrade(context.Context, store.ClusterIdentity, string, string) (store.ClusterUpgradeStatus, bool, bool, error)
	}
	memberObserver interface {
		quorumFactsReader
		quorumDiscoveryReader
		clusterUpgradeStepper
	}
)

var (
	_ memberObserver = (*store.LegacyJoinedRaftSession)(nil)
	_ memberObserver = (*store.SingleMemberObservation)(nil)
)

// registerMemberObservation mounts the routes other members and the
// cluster's commands ask every member: the quorum facts, the discovery of
// the members, the upgrade steps and this member's part of a diagnostics
// collection. Every listener of a member mounts them through here, so a
// route added here is served by all of them. The quorum routes are required;
// an upgrade or diagnostics route that cannot be built is left out and the
// reason logged, and the member still starts.
func registerMemberObservation(router chi.Router, bundle *enrollmentBundle, session joinedRaftSession, requests *materialRequestTracker, agents func() (int, bool), local Config, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	facts, err := closedQuorumFactsHandler(bundle, session, requests, log)
	if err != nil {
		return err
	}
	discovery, err := closedQuorumDiscoveryHandler(bundle, session, requests)
	if err != nil {
		return err
	}
	router.Post(enrollment.ClusterQuorumFactsPath, facts)
	router.Post(enrollment.ClusterQuorumDiscoveryPath, discovery)
	if upgrade, err := clusterUpgradeHandler(bundle, session, requests, agents); err != nil {
		log.Error("cluster upgrade route is not served: this member cannot be asked its upgrade status", "err", err)
	} else {
		router.Post(enrollment.ClusterUpgradePath, upgrade)
	}
	registerClusterDiagnostics(router, bundle, requests, local, log)
	return nil
}
