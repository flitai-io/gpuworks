//go:build linux && dqlite

package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// checkJoinedMaterialReadiness proves the current consumer files on this
// member and the current canonical bytes on a majority of live members.
// A database pointer or an earlier transfer receipt is never sufficient.
func checkJoinedMaterialReadiness(ctx context.Context, session *ClusterRaftSession, cfg Config) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if session == nil || session.db == nil || session.info.EnrollmentOrigin == "" || session.materialServer == nil {
		return false, nil
	}
	st, err := store.OpenJoinedMemberStore(ctx, session.db, bus.New())
	if err != nil {
		return materialReadinessFailure(ctx, err)
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil || bundle == nil || bundle.info.ClusterID != session.info.ClusterID || bundle.info.CAFingerprint != session.info.CAFingerprint || bundle.info.Protocol != session.info.Protocol || bundle.info.PublicURL != session.info.EnrollmentOrigin {
		return materialReadinessFailure(ctx, err)
	}
	localKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil || localKey != session.info.KeyFingerprint {
		return materialReadinessFailure(ctx, err)
	}
	return checkJoinedStoreMaterialReadiness(ctx, st, bundle, session.db.ID, cfg)
}

// checkJoinedStoreMaterialReadiness proves material bytes using a Store that
// already owns or borrows the joined member runtime. The caller validates the
// local enrollment bundle against its persisted join identity before using
// this proof to open business traffic.
func checkJoinedStoreMaterialReadiness(ctx context.Context, st *store.Store, bundle *enrollmentBundle, memberID uint64, cfg Config, loggers ...*slog.Logger) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if st == nil || bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil || memberID == 0 {
		return false, nil
	}
	localKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return materialReadinessFailure(ctx, err)
	}
	ready, err := proveJoinedStoreMaterial(ctx, st, bundle, memberID, localKey, cfg, loggers...)
	return thisMembersVerdict(localKey, ready, err)
}

// thisMembersVerdict is the one exit of a member's proof. The worker closes
// admission at once on a refusal, so the only refusal a proof may return is
// the database's refusal of the proving member itself. Any other, such as the
// refusal of another member's key that the same grant returns when that
// member is removed, leaves here as "not ready".
func thisMembersVerdict(localKey string, ready bool, err error) (bool, error) {
	if store.Causes(err, store.CauseMemberRefused) != 0 && !store.RefusesMember(err, localKey) {
		return false, nil
	}
	return ready, err
}

func proveJoinedStoreMaterial(ctx context.Context, st *store.Store, bundle *enrollmentBundle, memberID uint64, localKey string, cfg Config, loggers ...*slog.Logger) (bool, error) {
	log := slog.Default()
	if len(loggers) != 0 && loggers[0] != nil {
		log = loggers[0]
	}
	// From here on a failure may be the database's refusal of this member.
	fail := func(cause ...error) (bool, error) { return materialProofFailure(ctx, localKey, cause...) }
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	receiver, err := newMaterialReceiver(filepath.Join(root, "material-replicas"), memberID)
	if err != nil {
		return fail(err)
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	manifests, err := st.CountMaterialManifests(ctx)
	if err != nil || manifests == 0 {
		return fail(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var members []store.ClusterTransportMember
	var grantKeys []string
	for {
		grantKeys, err = materialProofGrantKeys(ctx, st, expected, localKey)
		if err == nil {
			_, members, err = grantMaterialProofMembershipKeys(ctx, st, expected, grantKeys)
		}
		if err != nil {
			// One failed grant already committed its cost; the next regular
			// proof re-reads keys instead of queuing more writer commits here.
			return fail(err)
		}
		if len(members) >= 2 {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	self := false
	for _, member := range members {
		if member.ID == memberID {
			self = member.KeyFingerprint == localKey && member.EnrollmentOrigin == bundle.info.PublicURL
		}
	}
	if !self {
		return false, nil
	}
	pointers, err := joinedMaterialPointers(ctx, st, expected, members, memberID)
	if err != nil || len(pointers) == 0 {
		return fail(err)
	}
	// The single opening grant above brackets every source byte check below.
	// A pointer first published after its key read is outside that bracket:
	// fail this round and let the next regular proof re-read the key set.
	covered := make(map[string]struct{}, len(grantKeys))
	for _, key := range grantKeys {
		covered[key] = struct{}{}
	}
	manifestsBySource := make(map[uint64]materialManifest, len(pointers))
	sources := make([]store.ClusterTransportMember, 0, len(pointers))
	for _, source := range members {
		pointer, published := pointers[source.ID]
		if !published {
			continue
		}
		if _, ok := covered[source.KeyFingerprint]; !ok {
			return fail()
		}
		manifest, err := verifyCandidateReceivedMaterialManifestBytes(ctx, st, receiver, expected, pointer)
		if err != nil {
			return fail(err)
		}
		consumerStarted := candidateProofStart(ctx)
		consumerErr := checkCandidateMaterialConsumerReferences(ctx, st, receiver, manifest)
		candidateProofObserve(ctx, proofConsumerReferences, consumerStarted, 0)
		if consumerErr != nil {
			return fail(consumerErr)
		}
		manifestsBySource[source.ID] = manifest
		sources = append(sources, source)
	}
	installedStarted := candidateProofStart(ctx)
	installedErr := checkCandidateInstalledMaterialSets(ctx, receiver, manifestsBySource)
	candidateProofObserve(ctx, proofInstalledPaths, installedStarted, 0)
	if installedErr != nil {
		return fail(installedErr)
	}
	peers := make([]store.ClusterTransportMember, 0, len(members)-1)
	for _, member := range members {
		if member.ID != memberID {
			peers = append(peers, member)
		}
	}
	// Ring distance from this member spreads every proof's primary read around
	// the live membership instead of concentrating it on the lowest ID.
	// Wrapping subtraction is the ring distance; member IDs are unique.
	slices.SortFunc(peers, func(a, b store.ClusterTransportMember) int {
		return cmp.Compare(a.ID-memberID, b.ID-memberID)
	})
	peers = orderMaterialProofPeers(peers, time.Now().UTC())
	if err := runCandidateProgressHedgedPeerQuorum(ctx, len(peers), len(members)/2, candidateRemoteConcurrency, materialReproofInterval, func(workCtx context.Context, index int, started, progress func()) (bool, error) {
		peer := peers[index]
		err := withCandidateRemoteSlot(workCtx, func(requestCtx context.Context) error {
			started()
			client, err := memberMaterialClient(bundle, peer.EnrollmentOrigin, peer.KeyFingerprint)
			if err != nil {
				return err
			}
			defer client.CloseIdleConnections()
			return verifyCandidateRemoteMaterialSetsWithProgress(requestCtx, client, peer.EnrollmentOrigin, pointers, manifestsBySource, peer.ID, progress)
		})
		recordMaterialPeerOutcome(peer.EnrollmentOrigin, workCtx, err)
		if err != nil {
			log.DebugContext(workCtx, "candidate remote material verification failed",
				"stage", "remote_material_set", "peer_id", peer.ID, "peer_address", peer.Address,
				"peer_key_fingerprint", peer.KeyFingerprint, "err_class", closedMemberErrorClass(err),
				"caller_context_class", closedMemberErrorClass(workCtx.Err()), "error", err)
		}
		return materialPeerVote(err)
	}); err != nil {
		recordCandidateProofFailure(ctx, "remote_quorum", err)
		return fail(err)
	}
	for _, source := range sources {
		pointer := pointers[source.ID]
		manifest, err := verifyCandidateReceivedMaterialManifestBytes(ctx, st, receiver, expected, pointer)
		if err != nil {
			return fail(err)
		}
		if err := checkCandidateMaterialConsumerReferences(ctx, st, receiver, manifest); err != nil {
			return fail(err)
		}
	}
	// The closing grant re-increments the same key set in one transaction and
	// re-observes live membership before the final pointer recheck.
	_, current, err := grantMaterialProofMembershipKeys(ctx, st, expected, grantKeys)
	if store.RefusesMember(err, localKey) {
		// Before the interruption check, which answers with the context's
		// error once the context has ended.
		return false, err
	}
	if interrupted, interruptErr := materialMembershipInterrupted(ctx, err, members, current); interrupted || err != nil || !slices.Equal(members, current) {
		if interrupted {
			return false, interruptErr
		}
		return fail(err)
	}
	latest, err := joinedMaterialPointers(ctx, st, expected, current, memberID)
	if err != nil || !maps.Equal(pointers, latest) {
		return fail(err)
	}
	return true, nil
}

// materialProofGrantKeys enumerates the local key and every published source
// key with a live admission so one replicated grant can bracket the whole
// proof. A revoked or never-admitted source key would be rejected by the
// grant and poison every round, so only currently admitted keys feed it. The
// read itself grants nothing; the grant it feeds re-observes live membership
// on both sides of one transaction, and a pointer published after this read
// fails the coverage check before any byte is trusted.
func materialProofGrantKeys(ctx context.Context, st *store.Store, expected store.ClusterIdentity, localKey string) ([]string, error) {
	sources, err := st.AdmittedMaterialManifestSourceKeys(ctx, expected.ClusterID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 1, len(sources)+1)
	seen := map[string]bool{localKey: true}
	keys[0] = localKey
	for _, source := range sources {
		if seen[source] {
			continue
		}
		seen[source] = true
		keys = append(keys, source)
	}
	return keys, nil
}

func joinedMaterialPointers(ctx context.Context, st *store.Store, expected store.ClusterIdentity, members []store.ClusterTransportMember, targetID uint64) (map[uint64]store.MaterialManifestRecord, error) {
	pointers := make(map[uint64]store.MaterialManifestRecord, len(members)-1)
	for _, member := range members {
		if member.ID == targetID {
			continue
		}
		if member.EnrollmentOrigin == "" || member.KeyFingerprint == "" {
			return nil, errors.New("live material member has no authenticated HTTPS endpoint")
		}
		pointerStarted := candidateProofStart(ctx)
		pointer, err := st.MaterialManifestBySource(ctx, int64(member.ID))
		candidateProofObserve(ctx, proofPointerCheck, pointerStarted, 0)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if pointer.ClusterID != expected.ClusterID || pointer.SourceKey != member.KeyFingerprint || !validMaterialDigest(pointer.Digest) || pointer.Bytes <= 0 || pointer.Bytes > maxMaterialManifestBytes {
			return nil, errors.New("material manifest pointer differs from live source")
		}
		pointers[member.ID] = pointer
	}
	return pointers, nil
}

// materialPeerVote counts one remote member. A reset, a 503, or a database
// connection loss does not vote and does not cancel the other member. A
// metadata or digest mismatch remains fatal for this proof.
func materialPeerVote(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if peerUnreachable(err) || (proofClusterLost(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, errCandidateMaterialNoMajority)) {
		return false, nil
	}
	return false, err
}

// materialMembershipInterrupted reports a lost leader or a grant that only
// dropped members already observed. A new member, a changed key, or any other
// definitive mismatch stays a negative proof.
func materialMembershipInterrupted(ctx context.Context, err error, previous, current []store.ClusterTransportMember) (bool, error) {
	if err == nil && slices.Equal(previous, current) {
		return false, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return true, cerr
	}
	if proofClusterLost(err) || (err == nil && observedMembersOnlyShrink(previous, current)) {
		return true, context.DeadlineExceeded
	}
	return false, nil
}

// materialProofFailure is how a stage of the proof fails: the database's
// refusal of this member as it is, also when the context has ended, and
// everything else as materialReadinessFailure has it.
func materialProofFailure(ctx context.Context, localKey string, cause ...error) (bool, error) {
	for _, err := range cause {
		if store.RefusesMember(err, localKey) {
			return false, err
		}
	}
	return materialReadinessFailure(ctx, cause...)
}

// A live parent used to erase a lost leader into a definitive false result.
// DeadlineExceeded is the signal an unexpired ordinary proof already keeps.
// A changed manifest still returns false, nil. Fewer than two live members
// stays on the caller's definitive path so a lost majority can close.
func materialReadinessFailure(ctx context.Context, cause ...error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, err := range cause {
		if proofClusterLost(err) || store.ProofReadUnanswered.Unanswered(err) {
			return false, context.DeadlineExceeded
		}
	}
	return false, nil
}
