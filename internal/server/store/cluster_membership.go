package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

type ClusterTransportMember struct {
	ID               uint64
	Address          string
	Role             string
	KeyFingerprint   string
	EnrollmentOrigin string
}

type clusterMembershipGrant func(context.Context, ClusterIdentity, string) (uint64, []ClusterTransportMember, error)

const ClusterMembershipGrantTimeout = 5 * time.Second

// GrantClusterMembership observes the live configuration before and after a
// majority-confirmed authorization grant. Matching observations are not an
// atomic configuration freeze or a readiness guarantee for pending members.
func (s *Store) GrantClusterMembership(ctx context.Context, expected ClusterIdentity, requesterKey string) (uint64, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ClusterMembershipGrantTimeout)
	defer cancel()
	if s.grantClusterMembership == nil {
		return 0, nil, errors.New("live native cluster membership is unavailable")
	}
	revision, members, err := s.grantClusterMembership(ctx, expected, requesterKey)
	s.rememberConfirmedMembership(members, err)
	return revision, members, err
}

// GrantClusterMembershipKeys increments every named admission in one replicated
// transaction, between one pair of live membership observations. Callers use it
// to bracket a multi-source proof without a commit per source.
func (s *Store) GrantClusterMembershipKeys(ctx context.Context, expected ClusterIdentity, keys []string) (uint64, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ClusterMembershipGrantTimeout)
	defer cancel()
	if s.grantClusterMembershipKeys == nil {
		return 0, nil, errors.New("live native cluster membership is unavailable")
	}
	revision, members, err := s.grantClusterMembershipKeys(ctx, expected, keys)
	s.rememberConfirmedMembership(members, err)
	return revision, members, err
}

// confirmedClusterMembership is the member list of the latest successful
// grant, kept in memory only.
type confirmedClusterMembership struct {
	members []ClusterTransportMember
	at      time.Time
}

func (s *Store) rememberConfirmedMembership(members []ClusterTransportMember, err error) {
	if err != nil || len(members) == 0 {
		return
	}
	s.confirmedMembership.Store(&confirmedClusterMembership{members: slices.Clone(members), at: elapsedClock()})
}

// ForgetConfirmedClusterMembership drops the list of the latest successful
// grant, so the next caller takes a fresh one. It runs when this member's own
// admission closes and when this member commits a revocation.
func (s *Store) ForgetConfirmedClusterMembership() {
	s.confirmedMembership.Store(nil)
}

// ConfirmedClusterMembership returns the member list the latest successful
// grant of this process confirmed, when that grant is no older than maxAge.
// It reads memory only. Callers that must not wait for a replicated write use
// it between grants: a member removed since then stays in the list, and one
// added since then is missing, for at most maxAge.
func (s *Store) ConfirmedClusterMembership(maxAge time.Duration) ([]ClusterTransportMember, bool) {
	confirmed := s.confirmedMembership.Load()
	if confirmed == nil || maxAge <= 0 || time.Since(confirmed.at) > maxAge {
		return nil, false
	}
	return slices.Clone(confirmed.members), true
}

func normalizeLiveClusterMembers(input []cluster.Member) ([]cluster.Member, error) {
	if len(input) == 0 || len(input) > 512 {
		return nil, errors.New("live cluster membership is empty or exceeds capacity")
	}
	members := slices.Clone(input)
	ids := make(map[uint64]bool, len(input))
	addresses := make(map[string]bool, len(input))
	for i := range members {
		member := &members[i]
		if member.ID == 0 || ids[member.ID] {
			return nil, errors.New("live cluster member has invalid or duplicate identity")
		}
		switch member.Role {
		case "voter", "stand-by", "spare":
		default:
			return nil, errors.New("live cluster member has invalid role")
		}
		address, err := normalizeClusterMemberAddress(member.Address)
		if err != nil {
			return nil, err
		}
		if addresses[address] {
			return nil, errors.New("live cluster member has duplicate address")
		}
		member.Address = address
		ids[member.ID] = true
		addresses[address] = true
	}
	slices.SortFunc(members, func(a, b cluster.Member) int { return cmp.Compare(a.ID, b.ID) })
	return members, nil
}

// BindClusterMembers is the one definition of which members of the database
// are members of the cluster, for a caller that holds the database's member
// list and the admission rows itself. See bindLiveClusterAdmissions.
func BindClusterMembers(live []cluster.Member, admissions []ClusterAdmission) ([]ClusterTransportMember, error) {
	members, err := normalizeLiveClusterMembers(live)
	if err != nil {
		return nil, err
	}
	return bindLiveClusterAdmissions(members, admissions)
}

// bindLiveClusterAdmissions gives each member of the database its admission
// and returns the members that hold one.
//
// A member of the database is left out when its address has a revoked
// admission and no admission that is not revoked. A member whose address has
// no admission row at all fails the whole binding.
//
// The rule is keyed on the address, and revoked rows are never deleted: a
// later member of the database at an address that once held a revoked
// admission, and that was never admitted itself, is left out as well, where
// a member at a fresh address fails the binding.
//
// The returned list can be shorter than the database's member list. A caller
// that counts over it counts the members returned.
func bindLiveClusterAdmissions(live []cluster.Member, admissions []ClusterAdmission) ([]ClusterTransportMember, error) {
	active := make(map[string]ClusterAdmission, len(admissions))
	keys := make(map[string]bool, len(admissions))
	revoked := make(map[string]bool)
	for _, admission := range admissions {
		if admission.RevokedAt != nil {
			// Matched as stored: a live address is normalized, so a
			// revoked row with any other spelling matches nothing.
			revoked[admission.Address] = true
			continue
		}
		address, err := normalizeClusterMemberAddress(admission.Address)
		if err != nil || address != admission.Address || !validClusterDigest(admission.KeyFingerprint, 32) {
			return nil, ErrClusterAdmissionRejected
		}
		if admission.EnrollmentOrigin != "" {
			origin, err := normalizeClusterEnrollmentOrigin(admission.EnrollmentOrigin)
			if err != nil || origin != admission.EnrollmentOrigin {
				return nil, ErrClusterEnrollmentOriginInvalid
			}
		}
		if _, exists := active[address]; exists || keys[admission.KeyFingerprint] {
			return nil, errors.New("active cluster admissions have duplicate key or address")
		}
		active[address] = admission
		keys[admission.KeyFingerprint] = true
	}
	result := make([]ClusterTransportMember, 0, len(live))
	boundKeys := make(map[string]bool, len(live))
	for _, member := range live {
		admission, exists := active[member.Address]
		if !exists && revoked[member.Address] {
			continue
		}
		if !exists || boundKeys[admission.KeyFingerprint] {
			return nil, errors.New("live cluster member has no unique active admission")
		}
		boundKeys[admission.KeyFingerprint] = true
		result = append(result, ClusterTransportMember{ID: member.ID, Address: member.Address, Role: member.Role, KeyFingerprint: admission.KeyFingerprint, EnrollmentOrigin: admission.EnrollmentOrigin})
	}
	return result, nil
}
