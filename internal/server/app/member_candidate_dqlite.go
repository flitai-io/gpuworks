//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// newCandidateMaterialEligibility is the closed member's request-path
// material gate input. The business assembly owner must run it with the App
// lifetime and call Invalidate on observed membership or material changes.
// Constructing it does not open any business route by itself.
func newCandidateMaterialEligibility(a *App) *materialEligibility {
	log := slog.Default()
	if a != nil && a.log != nil {
		log = a.log
	}
	return newJournalledMaterialEligibility(a, log, time.Now, a.CandidateMemberReadyMeasured)
}

// newJournalledMaterialEligibility builds the proof worker around measure and
// has the member's journal told what each attempt ended on and what the
// worker made of it.
func newJournalledMaterialEligibility(a *App, log *slog.Logger, now func() time.Time, measure func(context.Context) (bool, CandidateProofMetrics, error)) *materialEligibility {
	prove := func(ctx context.Context) (bool, error) {
		ready, metrics, err := measure(ctx)
		log.DebugContext(ctx, "candidate material proof completed", "ready", ready, "err_class", closedMemberErrorClass(err),
			"failure_stage", metrics.FailureStage, "failure_class", metrics.FailureClass,
			"total_ms", float64(metrics.Total.Duration)/float64(time.Millisecond), "metrics", metrics)
		if a != nil {
			a.admissionJournal.attempted(ready && err == nil, closedMemberErrorClass(err), metrics.FailureStage, metrics.FailureClass, metrics.Total.Duration)
		}
		return ready, err
	}
	eligibility := newMaterialEligibility(now, prove, materialReproofInterval)
	eligibility.setIdleRest(materialIdleReproofRest)
	if a != nil {
		eligibility.setOnOutcome(func(proved bool) { a.admissionJournal.outcome(log, proved) })
	}
	return eligibility
}

// CandidateMemberReady proves that this closed App is currently admitted and
// has the canonical material bytes needed by the live cluster. This is an
// on-demand, potentially expensive network and disk proof; callers must bound
// their context and must not invoke it on every business request.
func (a *App) CandidateMemberReady(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a == nil || !a.joinedClosed || a.legacyClosed || a.store == nil {
		return false, nil
	}
	bundle, memberID, key, cause := a.candidateEnrollmentWithCause(ctx)
	if bundle == nil {
		return ownEnrollmentVerdict(ctx, key, cause)
	}
	// Membership is established by the material checker against a fresh live
	// snapshot. The route supplies only the local member ID, never authority.
	ready, err := checkJoinedStoreMaterialReadiness(ctx, a.store, bundle, memberID, a.cfg, a.log)
	if err != nil || !ready {
		return ready, err
	}
	current, currentID, currentKey, cause := a.candidateEnrollmentWithCause(ctx)
	if current == nil {
		return ownEnrollmentVerdict(ctx, currentKey, cause)
	}
	if currentID != memberID || currentKey != key || current.info != bundle.info {
		return materialReadinessFailure(ctx)
	}
	return true, nil
}

// seedAdmissionFailure reads the answer to a seed member's direct read of
// its own admission. The database's refusal of this member comes back with
// the member's key. Any other error is the cause without a key, so that a
// read the database did not answer is classified as no answer and not as
// "not ready", which the worker latches.
func seedAdmissionFailure(admission store.ClusterAdmission, err error, key string) (refusedKey string, cause error, failed bool) {
	switch {
	case store.RefusesMember(err, key):
		return key, err, true
	case err != nil:
		return "", err, true
	case admission.InvitationHash != "":
		// Admitted by an invitation that is not yet claimed as a member.
		return "", nil, true
	}
	return "", nil, false
}

// ownEnrollmentVerdict is how the proof fails on the member's own enrollment
// check. It passes the same filter as the proof's body; here that changes
// nothing, since the refusal was already matched to this key.
func ownEnrollmentVerdict(ctx context.Context, key string, cause error) (bool, error) {
	ready, err := materialProofFailure(ctx, key, cause)
	return thisMembersVerdict(key, ready, err)
}

// CandidateMemberReadyMeasured runs the same on-demand proof and returns its
// aggregate cost even when readiness fails, without retaining metrics.
func (a *App) CandidateMemberReadyMeasured(ctx context.Context) (bool, CandidateProofMetrics, error) {
	recorder := &candidateProofRecorder{}
	ctx = context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
	started := time.Now()
	ready, err := a.CandidateMemberReady(ctx)
	metrics := recorder.snapshot()
	metrics.Total = CandidateProofStage{Calls: 1, Duration: time.Since(started)}
	return ready, metrics, err
}

// candidateEnrollment re-reads the local identity and its seed or persisted
// joined provenance. No cached route or earlier startup proof is authority.
func (a *App) candidateEnrollment(ctx context.Context) (*enrollmentBundle, uint64, string) {
	bundle, id, key, _ := a.candidateEnrollmentWithCause(ctx)
	return bundle, id, key
}

// candidateEnrollmentWithCause also returns a cluster identity read failure.
// One that fails while the database is between leaders says nothing about
// this member; the proof reports it as an interruption, not a negative. When
// the database refuses this member's own admission, the cause is that
// refusal and the key is the member's.
func (a *App) candidateEnrollmentWithCause(ctx context.Context) (*enrollmentBundle, uint64, string, error) {
	started := candidateProofStart(ctx)
	defer candidateProofObserve(ctx, proofEnrollment, started, 0)
	begin, ok := a.store.ClusterSessionRoute()
	if !ok || begin.ID == 0 || ctx.Err() != nil {
		return nil, 0, "", nil
	}
	bundle, err := readEnrollmentBundle(a.cfg.DataDir, false)
	if err != nil || bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil {
		return nil, 0, "", nil
	}
	if err := verifyEnrollmentState(ctx, a.store, bundle); err != nil {
		// Never the database's refusal: this check compares the files on
		// this member's disk with an identity row that is written once, so
		// a difference is a finding about the local files. It carries no
		// key and leaves the proof as "not ready".
		return nil, 0, "", err
	}
	key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return nil, 0, "", nil
	}
	root, err := filepath.EvalSymlinks(a.cfg.DataDir)
	if err != nil {
		return nil, 0, "", nil
	}
	_, err = os.Lstat(filepath.Join(root, "gpuworks.db.join"))
	switch {
	case err == nil:
		if err := verifyPersistedJoinedEnrollment(a.cfg, bundle); err != nil {
			return nil, 0, "", nil
		}
	case os.IsNotExist(err):
		admission, err := a.store.ClusterAdmissionForVerifiedKey(ctx, bundle.info.ClusterID, key)
		if refusedKey, cause, failed := seedAdmissionFailure(admission, err, key); failed {
			return nil, 0, refusedKey, cause
		}
	default:
		return nil, 0, "", nil
	}
	end, ok := a.store.ClusterSessionRoute()
	if !ok || end.ID != begin.ID {
		return nil, 0, "", nil
	}
	return bundle, begin.ID, key, nil
}
