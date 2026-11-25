//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// ClusterRaftSession is a database that has started joining. WaitReady failure
// leaves it running. Close is the only way to stop it and drop the runtime lease.
// Readiness is not voter, catch-up, or application readiness.
type ClusterRaftSession struct {
	db                  *cluster.Database
	info                ClusterJoinInfo
	key                 []byte
	snapshot            joinMaterialSnapshot
	materialServer      *enrollmentServer
	release             func() error
	once                sync.Once
	err                 error
	cancelAuthorization context.CancelFunc
	authorizationDone   chan struct{}
}

// ID is the persisted Raft identity. An interrupted start reuses it.
func (s *ClusterRaftSession) ID() uint64 {
	if s == nil || s.db == nil {
		return 0
	}
	return s.db.ID
}

// Database returns the running node. It remains usable after WaitReady fails.
func (s *ClusterRaftSession) Database() *cluster.Database {
	if s == nil {
		return nil
	}
	return s.db
}

// WaitReady waits for ordinary SQL reachability. Failure does not close the node.
func (s *ClusterRaftSession) WaitReady(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("joining database is not running")
	}
	return s.db.WaitReady(ctx)
}

// Close stops the node and releases the runtime lease.
func (s *ClusterRaftSession) Close() error {
	if s == nil {
		return nil
	}
	if s.materialServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
		err := s.materialServer.Close(ctx)
		cancel()
		if err != nil {
			// An active receiver may still be writing after a forced HTTP close.
			// Keep SQL and the runtime lease until a later Close can prove drain.
			return err
		}
	}
	s.once.Do(func() {
		var err error
		if s.cancelAuthorization != nil {
			s.cancelAuthorization()
			<-s.authorizationDone
		}
		if s.db != nil {
			err = s.db.Close()
		}
		if s.release != nil {
			err = errors.Join(err, s.release())
		}
		s.err = err
	})
	return s.err
}

func (s *ClusterRaftSession) startTransportAuthorization(refresh func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelAuthorization = cancel
	s.authorizationDone = make(chan struct{})
	go func() {
		defer close(s.authorizationDone)
		runTransportAuthorization(ctx, refresh, slog.Default(), time.Second, 5*time.Second)
	}()
}

// PrepareClusterRaftIdentity publishes the stable Raft identity before the
// node listens. It does not start the database or open the application.
func PrepareClusterRaftIdentity(ctx context.Context, cfg Config, target ClusterJoinTarget) (id uint64, resultErr error) {
	hold, err := lockClusterJoinMaterials(ctx, cfg, target)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, hold.release()) }()
	seeds, err := hold.remoteSeeds(ctx)
	if err != nil {
		return 0, err
	}
	return cluster.PrepareJoinIdentity(ctx, hold.path+".dqlite", hold.info.Address, seeds)
}

// ReconcileClusterRaftMarker deletes an empty join marker only when the live
// enrollment membership already contains this exact identity. The database
// must not be open. A missing membership match or a non-empty marker is kept.
func ReconcileClusterRaftMarker(ctx context.Context, cfg Config, target ClusterJoinTarget) (resultErr error) {
	hold, err := lockClusterJoinMaterials(ctx, cfg, target)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, hold.release()) }()
	members, err := hold.liveMembers(ctx)
	if err != nil {
		return err
	}
	return cluster.ReconcileCommittedJoinMarker(ctx, hold.path+".dqlite", members)
}

// StartClusterRaft starts the authorized database under the runtime lock and
// the existing join gate. It does not migrate, seed, recover, or accept
// business traffic. A leftover join marker is removed only after live
// membership is observed to contain this exact identity. If that observation
// fails, the marker stays and no database is started. WaitReady is left to
// the caller so a failed wait can be retried on the same running node.
func StartClusterRaft(ctx context.Context, cfg Config, target ClusterJoinTarget) (*ClusterRaftSession, error) {
	return startClusterRaft(ctx, cfg, target, true)
}

// startClusterRaft starts the member's node. A join tells its process the
// cluster's tier first (joining), as a server is told its installation's:
// the node, the membership grants and the connections this process dials
// then follow one timing. Reopening a joined identity does not: the tier
// the member was given when it joined may since have been changed in its
// installation's record, which is what the process was told.
func startClusterRaft(ctx context.Context, cfg Config, target ClusterJoinTarget, joining bool) (_ *ClusterRaftSession, resultErr error) {
	hold, err := lockClusterJoinMaterials(ctx, cfg, target)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, hold.release())
		}
	}()
	if joining {
		cluster.UseStorageTier(joinedStorageTier(hold.snapshot))
	}
	raftDir := hold.path + ".dqlite"
	seeds, fetchErr := hold.remoteSeeds(ctx)
	if fetchErr != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		_, seeds, err = cluster.PublishedJoinSeeds(ctx, raftDir)
		if err != nil {
			return nil, fetchErr
		}
	}
	if err := hold.reconcileExactMembership(ctx, raftDir); err != nil {
		return nil, err
	}
	if _, err := cluster.PrepareJoinIdentity(ctx, raftDir, hold.info.Address, seeds); err != nil {
		return nil, err
	}
	trust, err := identity.NewTrust([]byte(hold.snapshot.Secrets.CACertificatePEM), hold.info.ClusterID, hold.info.CAFingerprint, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	db, err := cluster.StartAuthorizedDatabase(ctx, cluster.DatabaseConfig{
		Dir: raftDir, Address: hold.info.Address, Seeds: seeds, Name: filepath.Base(hold.path),
	}, filepath.Join(hold.dir, "transport-policy.json"), trust, []byte(hold.snapshot.CertificatePEM), hold.key)
	if err != nil {
		return nil, err
	}
	return &ClusterRaftSession{db: db, info: hold.info, key: hold.key, snapshot: hold.snapshot, release: hold.release}, nil
}

func (h *clusterJoinHold) liveMembers(ctx context.Context) ([]cluster.Member, error) {
	client, err := enrollment.NewClient(h.info.EnrollmentURL, h.info.ClusterID, h.info.CAFingerprint, h.info.Protocol)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	snapshot, err := client.FetchClusterMembership(ctx, []byte(h.snapshot.CertificatePEM), h.key)
	if err != nil {
		return nil, err
	}
	members := make([]cluster.Member, 0, len(snapshot.Members))
	for _, member := range snapshot.Members {
		members = append(members, cluster.Member{ID: member.ID, Address: member.Address, Role: member.Role})
	}
	return members, nil
}

func (h *clusterJoinHold) remoteSeeds(ctx context.Context) ([]string, error) {
	members, err := h.liveMembers(ctx)
	if err != nil {
		return nil, err
	}
	seeds := make([]string, 0, len(members))
	for _, member := range members {
		if member.Address != h.info.Address {
			seeds = append(seeds, member.Address)
		}
	}
	if len(seeds) == 0 {
		return nil, errors.New("joining database requires a remote cluster member")
	}
	return seeds, nil
}

func (h *clusterJoinHold) reconcileExactMembership(ctx context.Context, raftDir string) error {
	if _, err := os.Lstat(filepath.Join(raftDir, "join")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	members, err := h.liveMembers(ctx)
	if err != nil {
		// An unobserved marker must not be treated as a fresh join. Upstream
		// would repeat Add and never become ready after the member already exists.
		return err
	}
	err = cluster.ReconcileCommittedJoinMarker(ctx, raftDir, members)
	if errors.Is(err, cluster.ErrJoinIdentityNotLive) {
		return nil
	}
	return err
}

// joinedStorageTier is the tier a joining member runs: the one the cluster
// told it with its certificate.
func joinedStorageTier(snapshot joinMaterialSnapshot) storagetier.Tier {
	tier, err := storagetier.Parse(snapshot.StorageTier)
	if err != nil {
		return storagetier.Standard
	}
	return tier
}
