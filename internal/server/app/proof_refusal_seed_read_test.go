//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A seed's direct read of its own admission that the database did not answer
// is no answer: the seed keeps serving on its last confirmation past the
// proof's window, up to the unconfirmed limit. Answered as "not ready" it
// would be a verdict, and the seed would close when its window ends.
//
// The read's failure is given, not produced: a database that answers the
// identity read just before and then does not answer this one cannot be
// arranged on real processes. The worker is the real one.
func TestSeedAdmissionReadWithoutAnAnswerKeepsTheSeedOpen(t *testing.T) {
	const key = "seed-key"
	noLeader := fmt.Errorf("read admission: %w", dqlitedriver.ErrNoAvailableLeader)
	for _, tc := range []struct {
		name       string
		admission  store.ClusterAdmission
		err        error
		refusedKey string
		failed     bool
		noAnswer   bool
	}{
		{"admitted", store.ClusterAdmission{KeyFingerprint: key}, nil, "", false, false},
		{"no leader", store.ClusterAdmission{}, noLeader, "", true, true},
		{"the read's own time limit", store.ClusterAdmission{}, context.DeadlineExceeded, "", true, true},
		{"refused: this member's key", store.ClusterAdmission{}, &store.ClusterAdmissionRefusal{Key: key}, key, true, false},
		{"refused: identity conflict", store.ClusterAdmission{}, store.ErrClusterIdentityConflict, key, true, false},
		{"a refusal of another key", store.ClusterAdmission{}, &store.ClusterAdmissionRefusal{Key: "other"}, "", true, false},
		{"an error nothing classifies", store.ClusterAdmission{}, errors.New("no such table"), "", true, false},
		{"still an invitation", store.ClusterAdmission{KeyFingerprint: key, InvitationHash: "h"}, nil, "", true, false},
		{"end of stream, a finding like at every proof stage", store.ClusterAdmission{}, io.EOF, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedKey, cause, failed := seedAdmissionFailure(tc.admission, tc.err, key)
			if refusedKey != tc.refusedKey || failed != tc.failed || (tc.err != nil && cause != tc.err) {
				t.Fatalf("key=%q cause=%v failed=%t", refusedKey, cause, failed)
			}
			if !failed {
				return
			}
			ready, err := ownEnrollmentVerdict(t.Context(), refusedKey, cause)
			if ready || errors.Is(err, context.DeadlineExceeded) != tc.noAnswer {
				t.Fatalf("the proof's answer: ready=%t err=%v, want no answer=%t", ready, err, tc.noAnswer)
			}
			if refused := store.RefusesMember(err, key); refused != (tc.refusedKey != "") {
				t.Fatalf("the proof's answer refuses the member=%t: %v", refused, err)
			}
		})
	}

	// The code a closed member answers with says which refusal it was, and
	// a successful proof clears it.
	for _, tc := range []struct {
		refusal error
		code    string
	}{
		{&store.ClusterAdmissionRefusal{Key: key}, refusalMemberRevoked},
		{fmt.Errorf("grant: %w", store.ErrClusterIdentityConflict), refusalIdentityConflict},
	} {
		var answer error
		eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return answer == nil, answer }, time.Second)
		t.Cleanup(eligibility.Stop)
		if _, reason, _ := eligibility.eligibleForResult(0); reason != "proof_missing" {
			t.Fatalf("a member without a proof answers %q", reason)
		}
		answer = tc.refusal
		eligibility.Reprove(t.Context())
		if open, reason, _ := eligibility.eligibleForResult(0); open || reason != tc.code || refusalCodeOf(reason) != tc.code {
			t.Fatalf("after %v the member answers open=%t %q, want %q", tc.refusal, open, reason, tc.code)
		}
		answer = nil
		if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil || !eligibility.Eligible() {
			t.Fatalf("a proof after the refusal: %t %v", ready, err)
		}
		eligibility.Invalidate(closeMaterialPass)
		if _, reason, _ := eligibility.eligibleForResult(0); reason != "proof_missing" {
			t.Fatalf("after a successful proof and an invalidation the member answers %q", reason)
		}
	}

	// Through the worker: a proof that starts while the last confirmation is
	// valid and ends after it ran out, on the seed's read.
	for _, mode := range []string{"no answer", "not ready"} {
		clock := &materialTestClock{now: time.Unix(100, 0)}
		failing := false
		eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
			if !failing {
				return true, nil
			}
			clock.Advance(2 * time.Second)
			if mode == "not ready" {
				return false, nil
			}
			refusedKey, cause, _ := seedAdmissionFailure(store.ClusterAdmission{}, noLeader, key)
			return ownEnrollmentVerdict(ctx, refusedKey, cause)
		}, time.Second)
		t.Cleanup(eligibility.Stop)
		if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
			t.Fatalf("%s: first proof: %t %v", mode, ready, err)
		}
		failing = true
		clock.Advance(materialEligibilityWindow - time.Second)
		eligibility.Reprove(t.Context())
		if open := eligibility.Eligible(); open != (mode == "no answer") {
			t.Fatalf("%s: past the last confirmation's window the seed admits requests=%t", mode, open)
		}
	}
}
