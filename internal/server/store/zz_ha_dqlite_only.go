//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func readClosedClusterFactsDB(ctx context.Context, db *gorm.DB, expected ClusterIdentity, providerKey, requesterKey string, first cluster.QuorumProof) (bool, MaintenancePhaseState, []ClusterAdmission, error) {
	fail := func(err error) (bool, MaintenancePhaseState, []ClusterAdmission, error) {
		return false, MaintenancePhaseState{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if !validClusterDigest(providerKey, 32) || !validClusterDigest(requesterKey, 32) {
		return fail(ErrClusterAdmissionRejected)
	}
	if first.Term == 0 || first.Index == 0 || first.ConfigurationIndex == 0 || first.ConfigurationIndex > first.Index || first.LeaderID == 0 {
		return fail(ErrClusterIdentityConflict)
	}
	class, err := classifyJoinedSchemaClass(ctx, db, expected)
	if err != nil {
		return fail(err)
	}
	legacy := class == joinedSchemaLegacy
	var phase MaintenancePhaseState
	if !legacy {
		phase, err = (&Store{db: db}).ReadMaintenancePhase(ctx)
		if err != nil {
			return fail(err)
		}
	}
	admissions, err := clusterTransportAdmissions(db)
	if err != nil {
		return fail(err)
	}
	live, err := normalizeLiveClusterMembers(first.Members)
	if err != nil {
		return fail(err)
	}
	bound, err := bindLiveClusterAdmissions(live, admissions)
	if err != nil {
		return fail(err)
	}
	provider := false
	for _, member := range bound {
		if member.ID == first.LeaderID && member.Role == "voter" && member.KeyFingerprint == providerKey {
			provider = true
		}
	}
	requester := false
	for _, admission := range admissions {
		if admission.KeyFingerprint == requesterKey && admission.RevokedAt == nil {
			requester = true
		}
	}
	if !provider || !requester {
		return fail(ErrClusterAdmissionRejected)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	return legacy, phase, admissions, nil
}

// clusterMembershipObserveTimeout bounds one live Raft observation inside a
// grant. A partitioned member must fail that observation and return 503
// instead of holding the HTTP request until the whole grant budget expires.
var clusterMembershipObserveTimeout = 1500 * time.Millisecond

// Only native construction binds the actual transport authority and live Raft
// accessor. Callers cannot substitute YAML caches or request-supplied topology.
func (s *Store) bindClusterMembership(bound ClusterIdentity, live func(context.Context) ([]cluster.Member, error)) {
	grant := func(ctx context.Context, expected ClusterIdentity, keys []string) (uint64, []ClusterTransportMember, error) {
		return s.observeAndGrantClusterMembership(ctx, bound, live, expected, keys)
	}
	s.grantClusterMembershipKeys = grant
	s.grantClusterMembership = func(ctx context.Context, expected ClusterIdentity, key string) (uint64, []ClusterTransportMember, error) {
		return grant(ctx, expected, []string{key})
	}
}

// observeAndGrantClusterMembership increments every named admission between
// one pair of live membership observations. A view that changes mid-call is
// retried inside the caller's budget; each observation is bounded so a
// partitioned member fails instead of holding the request.
func (s *Store) observeAndGrantClusterMembership(ctx context.Context, bound ClusterIdentity, live func(context.Context) ([]cluster.Member, error), expected ClusterIdentity, keys []string) (revision uint64, members []ClusterTransportMember, resultErr error) {
	started := time.Now().UTC()
	phase := "validation"
	liveBefore, transportGrant, liveAfter := time.Duration(-1), time.Duration(-1), time.Duration(-1)
	defer func() {
		s.logClusterGrantDiagnostic(ctx, "membership", phase, started, resultErr,
			clusterGrantTiming("live_before_ms", liveBefore),
			clusterGrantTiming("transport_grant_ms", transportGrant),
			clusterGrantTiming("live_after_ms", liveAfter))
	}()
	if live == nil {
		return 0, nil, errors.New("live native cluster membership is unavailable")
	}
	if expected.ClusterID != bound.ClusterID || expected.CAFingerprint != bound.CAFingerprint || expected.Protocol != bound.Protocol {
		return 0, nil, ErrClusterIdentityConflict
	}
	if err := rejectClusterTransportGrantKeys(keys); err != nil {
		return 0, nil, err
	}
	var lastErr error
	observeLimit := func() time.Duration { return clusterMembershipObserveTimeout }
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return 0, nil, lastErr
			}
			return 0, nil, err
		}
		revision, members, err := s.attemptClusterMembershipGrant(ctx, bound, live, keys, observeLimit, &phase, &liveBefore, &transportGrant, &liveAfter)
		if err == nil {
			phase = "complete"
			return revision, members, nil
		}
		// Only an interrupted database-leader attempt is repeated, and always
		// as a complete observe-grant-observe sequence: nothing from a partial
		// or unknown-commit attempt is returned. The admission sequence only
		// increases, so a grant that did commit before its interruption is
		// harmless. An interrupted observation is repeated only inside the
		// bounded window, which keeps a partitioned member's refusal short.
		if !GrantInterrupted.Unanswered(err) || ctx.Err() != nil {
			return 0, nil, err
		}
		if (phase == "live_before" || phase == "live_after") && !errors.Is(err, errClusterMembershipChanged) {
			window := clusterMembershipRetryWindow()
			remaining := window - time.Since(started)
			if remaining <= 0 {
				return 0, nil, err
			}
			// From here on an observation also ends with the window, so a
			// member that never finds a leader refuses at the window and not
			// a whole observation later.
			observeLimit = func() time.Duration {
				return max(min(clusterMembershipObserveTimeout, window-time.Since(started)), time.Millisecond)
			}
		}
		lastErr = err
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil, lastErr
		case <-timer.C:
		}
	}
}

// errClusterMembershipChanged reports two live observations around one grant
// that disagree, as while a member is being added, removed or promoted.
var errClusterMembershipChanged = errors.New("live cluster membership changed during authorization")

// attemptClusterMembershipGrant runs one observe-grant-observe sequence and
// returns membership only when both observations agree.
func (s *Store) attemptClusterMembershipGrant(ctx context.Context, bound ClusterIdentity, live func(context.Context) ([]cluster.Member, error), keys []string, observeLimit func() time.Duration, phase *string, liveBefore, transportGrant, liveAfter *time.Duration) (uint64, []ClusterTransportMember, error) {
	*phase = "live_before"
	phaseStarted := time.Now().UTC()
	before, err := observeLiveClusterMembers(ctx, live, observeLimit())
	*liveBefore = time.Since(phaseStarted)
	if err != nil {
		return 0, nil, err
	}
	before, err = normalizeLiveClusterMembers(before)
	if err != nil {
		return 0, nil, err
	}
	*phase = "transport_grant"
	phaseStarted = time.Now().UTC()
	granted, admissions, err := s.grantClusterTransportAdmissionKeys(ctx, bound, keys)
	*transportGrant = time.Since(phaseStarted)
	if err != nil {
		return 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	*phase = "live_after"
	phaseStarted = time.Now().UTC()
	after, err := observeLiveClusterMembers(ctx, live, observeLimit())
	*liveAfter = time.Since(phaseStarted)
	if err != nil {
		return 0, nil, err
	}
	after, err = normalizeLiveClusterMembers(after)
	if err != nil {
		return 0, nil, err
	}
	if !slices.Equal(before, after) {
		return 0, nil, errClusterMembershipChanged
	}
	*phase = "binding"
	members, err := bindLiveClusterAdmissions(after, admissions)
	if err != nil {
		return 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	return granted, members, nil
}

// ErrClusterMembershipObservationTimeout marks a live observation that used
// up its own bound while the grant's budget had not expired. It says the
// leader could not be read in time, nothing about the membership itself.
var ErrClusterMembershipObservationTimeout = errors.New("live cluster membership observation timed out")

func observeLiveClusterMembers(ctx context.Context, live func(context.Context) ([]cluster.Member, error), bound time.Duration) ([]cluster.Member, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= bound {
		return live(ctx)
	}
	observeCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	members, err := live(observeCtx)
	if err != nil && observeCtx.Err() != nil && ctx.Err() == nil {
		err = errors.Join(err, ErrClusterMembershipObservationTimeout)
	}
	return members, err
}

// sessionDialAddress is the session listener for a member address: the same
// host and the next port. Members derive it from the address they already
// share, so the listener does not take the database port.
func sessionDialAddress(memberAddress string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(memberAddress))
	if err != nil || host == "" {
		return ""
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number >= 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(number+1))
}

func sessionRouteFromMembers(id uint64, address string, members []cluster.Member) ClusterSessionRoute {
	peers := make([]ClusterSessionPeer, 0, len(members))
	for _, member := range members {
		if member.ID == 0 || member.ID == id {
			continue
		}
		dial := sessionDialAddress(member.Address)
		if dial == "" {
			continue
		}
		peers = append(peers, ClusterSessionPeer{ID: member.ID, Address: dial})
	}
	return ClusterSessionRoute{ID: id, Secure: true, Listen: sessionDialAddress(address), Peers: peers}
}

func membersOrEmpty(ctx context.Context, db interface {
	Members(context.Context) ([]cluster.Member, error)
}) []cluster.Member {
	if db == nil {
		return nil
	}
	members, err := db.Members(ctx)
	if err != nil {
		return nil
	}
	return members
}

// checkLegacyExpandedCompatibility observes one database snapshot. It accepts
// only the exact schema immediately before the maintenance phase migration;
// any phase object without its marker, or marker without its phase object, is
// a partial upgrade requiring explicit recovery rather than a legacy runtime.
func (s *Store) checkLegacyExpandedCompatibility(ctx context.Context, expected ClusterIdentity) error {
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return ErrClusterIdentityConflict
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkLegacyExpandedSchema(tx); err != nil {
			return err
		}
		actual, err := (&Store{db: tx}).ClusterIdentity(ctx)
		if err != nil {
			return err
		}
		if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
			return ErrClusterIdentityConflict
		}
		return nil
	})
}

// grantTransportAdmissionsThroughInterruption is the periodic refresh's grant.
// A grant that a database leadership change interrupted is repeated as a
// complete grant, inside the cycle's own deadline and only within the same
// window that bounds a repeated membership observation. An interruption that
// outlasts the window is returned while the cycle is still live, exactly as
// a single failed attempt was, so the caller's verdict on it is unchanged.
// The admission sequence only increases, so a grant that committed before
// its interruption is harmless; no snapshot from a failed attempt is used.
func (s *Store) grantTransportAdmissionsThroughInterruption(ctx context.Context, expected ClusterIdentity, key string) (uint64, []ClusterAdmission, error) {
	started, window := time.Now().UTC(), clusterMembershipRetryWindow()
	for attempt := 1; ; attempt++ {
		revision, admissions, err := s.GrantClusterTransportAdmissions(ctx, expected, key)
		if err == nil || !GrantInterrupted.Unanswered(err) || ctx.Err() != nil ||
			time.Since(started) >= window {
			return revision, admissions, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil, err
		case <-timer.C:
		}
		s.clusterGrantDiagnosticLogger().DebugContext(ctx, "transport authorization refresh repeated after an interrupted grant",
			"err_class", clusterGrantErrorClass(err), "attempt", attempt+1)
	}
}

// Only native construction binds this callback. Public callers cannot replace
// the authority or member key with a request-supplied identity.
func (s *Store) bindTransportAuthorization(expected ClusterIdentity, key string, apply func(identity.TransportPolicy) (identity.TransportPolicy, error), active func() (identity.TransportPolicy, error)) {
	if s.writerGuard != nil && validClusterDigest(key, 32) {
		s.writerGuard.key.Store(&key)
	}
	s.refreshTransportAuthorization = func(ctx context.Context) error {
		revision, admissions, err := s.grantTransportAdmissionsThroughInterruption(ctx, expected, key)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot := transportAuthorizationSnapshot(expected, revision, admissions)
		_, err = apply(snapshot)
		return err
	}
	s.confirmTransportAuthorization = func(ctx context.Context, requesterKey, expectedLocalKey string) (identity.TransportPolicy, error) {
		if active == nil || !validClusterDigest(key, 32) || expectedLocalKey != key {
			return identity.TransportPolicy{}, errors.New("runtime transport identity does not match the requested member")
		}
		revision, admissions, err := s.GrantClusterTransportAdmissions(ctx, expected, requesterKey)
		if err != nil {
			return identity.TransportPolicy{}, err
		}
		if err := ctx.Err(); err != nil {
			return identity.TransportPolicy{}, err
		}
		applied, err := active()
		if err != nil {
			return identity.TransportPolicy{}, err
		}
		fresh := transportAuthorizationSnapshot(expected, revision, admissions)
		if applied.ClusterID != fresh.ClusterID || applied.CAFingerprint != fresh.CAFingerprint || applied.SourceRevision != fresh.SourceRevision || !slices.Equal(applied.AllowedKeys, fresh.AllowedKeys) || !slices.Equal(applied.RevokedKeys, fresh.RevokedKeys) {
			return identity.TransportPolicy{}, errors.New("runtime transport has not applied the current authorization snapshot")
		}
		authorize, err := applied.Authorizer(expected.ClusterID, expected.CAFingerprint)
		if err != nil {
			return identity.TransportPolicy{}, err
		}
		if err := authorize(key); err != nil {
			return identity.TransportPolicy{}, err
		}
		if err := authorize(requesterKey); err != nil {
			return identity.TransportPolicy{}, err
		}
		if err := ctx.Err(); err != nil {
			return identity.TransportPolicy{}, err
		}
		applied.AllowedKeys = slices.Clone(applied.AllowedKeys)
		applied.RevokedKeys = slices.Clone(applied.RevokedKeys)
		return applied, nil
	}

}

func transportAuthorizationSnapshot(expected ClusterIdentity, revision uint64, admissions []ClusterAdmission) identity.TransportPolicy {
	snapshot := identity.TransportPolicy{ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, SourceRevision: revision, AllowedKeys: []string{}, RevokedKeys: []string{}}
	for _, admission := range admissions {
		if admission.RevokedAt == nil {
			snapshot.AllowedKeys = append(snapshot.AllowedKeys, admission.KeyFingerprint)
		} else {
			snapshot.RevokedKeys = append(snapshot.RevokedKeys, admission.KeyFingerprint)
		}
	}
	return snapshot
}
