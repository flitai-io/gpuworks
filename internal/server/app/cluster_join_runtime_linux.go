//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// StartPersistedClusterRaft reopens an existing joined identity for a later
// process. It starts Raft under the same runtime lease as the Join command and
// leaves application traffic closed. The locked start validates
// the persisted target again before starting the database. It leaves the
// process on the tier it was told (its installation's record), not the one
// the member was given when it joined.
func StartPersistedClusterRaft(ctx context.Context, cfg Config) (*ClusterRaftSession, error) {
	target, err := persistedClusterJoinTarget(cfg)
	if err != nil {
		return nil, err
	}
	return startClusterRaft(ctx, cfg, target, false)
}

// OpenPersistedJoinedStore hands a resumed Raft member and its runtime lease
// to Store.Close. The caller must keep the application closed until material,
// protocol, session-route and target-version readiness are continuously gated.
func OpenPersistedJoinedStore(ctx context.Context, cfg Config, b *bus.Bus) (_ *store.Store, resultErr error) {
	session, err := StartPersistedClusterRaft(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, session.Close())
		}
	}()
	if err := session.WaitReady(ctx); err != nil {
		return nil, err
	}
	st, err := store.OpenOwnedJoinedMemberStore(ctx, session.Database(), b, session.Close)
	if err != nil {
		return nil, err
	}
	if err := st.RefreshTransportAuthorization(ctx); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

func persistedClusterJoinTarget(cfg Config) (ClusterJoinTarget, error) {
	if cfg.DataDir == "" {
		return ClusterJoinTarget{}, errors.New("joined runtime requires an explicit data directory")
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return ClusterJoinTarget{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ClusterJoinTarget{}, err
	}
	dir := filepath.Join(root, "gpuworks.db.join")
	stat, err := os.Lstat(dir)
	if err != nil {
		return ClusterJoinTarget{}, err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 || stat.Mode()&os.ModeSymlink != 0 {
		return ClusterJoinTarget{}, errors.New("persisted join identity must be a private plain directory")
	}
	raw, err := readJoinMaterialFile(filepath.Join(dir, "config.json"), 128<<10)
	if err != nil {
		return ClusterJoinTarget{}, err
	}
	var info ClusterJoinInfo
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&info); err != nil {
		return ClusterJoinTarget{}, errors.New("invalid persisted join configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ClusterJoinTarget{}, errors.New("trailing persisted join configuration")
	}
	if _, err := loadClusterJoin(dir, info.ClusterJoinTarget); err != nil {
		return ClusterJoinTarget{}, err
	}
	return info.ClusterJoinTarget, nil
}

func verifyPersistedJoinedEnrollment(cfg Config, bundle *enrollmentBundle) error {
	target, err := persistedClusterJoinTarget(cfg)
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	info, err := loadClusterJoin(filepath.Join(root, "gpuworks.db.join"), target)
	if err != nil {
		return err
	}
	want := EnrollmentInfo{
		ClusterID: info.ClusterID, CAFingerprint: info.CAFingerprint,
		Protocol: info.Protocol, ListenAddress: info.EnrollmentListen,
		PublicURL: info.EnrollmentOrigin,
	}
	if bundle != nil {
		// The tier is not part of the member's identity: the join target
		// does not name it, the cluster does.
		want.StorageTier = bundle.info.StorageTier
	}
	if bundle == nil || bundle.info != want || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil {
		return errors.New("joined enrollment identity differs from persisted member")
	}
	key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil || key != info.KeyFingerprint {
		return errors.New("joined enrollment certificate differs from persisted member")
	}
	return nil
}
