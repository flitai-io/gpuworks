package store

import (
	"context"
	"errors"
	"slices"

	"gorm.io/gorm"
)

// One transaction increments every key, then reads the admission snapshot once.
// Each key keeps the recorded check before that snapshot and the final phase
// check after it. A rejected key rolls the whole set back. No caller can supply
// a phase or opt out of either final check.
func rejectClusterTransportGrantKeys(keys []string) error {
	if len(keys) == 0 || len(keys) > 512 {
		return ErrClusterAdmissionRejected
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if !validClusterDigest(key, 32) {
			return ErrClusterAdmissionRejected
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrClusterAdmissionRejected
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (s *Store) grantClusterTransportAdmissionKeys(ctx context.Context, expected ClusterIdentity, keys []string) (uint64, []ClusterAdmission, error) {
	if err := rejectClusterTransportGrantKeys(keys); err != nil {
		return 0, nil, err
	}
	ordered := slices.Clone(keys)
	slices.Sort(ordered)
	ctx, endWork := s.bindMaintenanceWork(ctx, MaintenanceWorkTransaction)
	defer endWork()
	var revision int64
	var admissions []ClusterAdmission
	err := s.membershipWriteTransaction(context.WithValue(ctx, clusterGrantPurposeKey{}, "transport_admissions"), func(tx *gorm.DB) error {
		var phase MaintenancePhaseState
		granted := make([]ClusterAdmission, 0, len(ordered))
		for i, key := range ordered {
			observed, actual, current, err := readClusterMaterialGrantInput(tx, key)
			if err != nil {
				return err
			}
			if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol || actual.Protocol != ClusterProtocolVersion {
				return ErrClusterIdentityConflict
			}
			if i == 0 {
				phase = observed
			} else if observed != phase {
				return ErrMaintenancePhaseChanged
			}
			next, err := grantClusterMaterials(tx, current)
			if err != nil {
				// A missing or revoked admission is refused here, and
				// the caller must be able to tell whose it was.
				return refusedKey(err, key)
			}
			granted = append(granted, next)
		}
		var err error
		revision, admissions, err = readClusterTransportGrantSnapshot(tx)
		if err != nil {
			return err
		}
		for _, row := range granted {
			if err := requireClusterMaterialGrantRecordedAndPhase(tx, ctx, row, phase); err != nil {
				return refusedKey(err, row.KeyFingerprint)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return uint64(revision), admissions, nil
}

const clusterTransportGrantSnapshotSQL = `SELECT
	r.id IS NOT NULL AS revision_present, r.revision AS authorization_revision,
	a.admission_present IS NOT NULL AS admission_present,
	a.key_fingerprint AS admission_key_fingerprint, a.invitation_hash AS admission_invitation_hash,
	a.address AS admission_address, a.enrollment_origin AS admission_enrollment_origin,
	a.admitted_at AS admission_admitted_at, a.revoked_at AS admission_revoked_at,
	a.material_grant_sequence AS admission_material_grant_sequence
	FROM (SELECT 1) AS singleton
	LEFT JOIN (SELECT id, revision FROM cluster_authorizations
		WHERE id = 1 AND typeof(revision) = 'integer' AND revision > 0) AS r ON 1 = 1
	LEFT JOIN (SELECT *, 1 AS admission_present FROM cluster_admissions) AS a ON 1 = 1
	ORDER BY a.key_fingerprint LIMIT 513`

func readClusterTransportGrantSnapshot(tx *gorm.DB) (int64, []ClusterAdmission, error) {
	var snapshot []struct {
		Admission             ClusterAdmission `gorm:"embedded;embeddedPrefix:admission_"`
		AuthorizationRevision int64
		RevisionPresent       bool
		AdmissionPresent      bool
	}
	if err := tx.Raw(clusterTransportGrantSnapshotSQL).Find(&snapshot).Error; err != nil {
		// A combined query can fail on the admission table before revealing
		// an invalid revision. Retain the original two-read error order in
		// this transaction; never repeat its grant or hide the failed read.
		var revision int64
		result := tx.Raw(`SELECT revision FROM cluster_authorizations WHERE id = 1 AND typeof(revision) = 'integer' AND revision > 0`).Scan(&revision)
		if result.Error != nil {
			return 0, nil, result.Error
		}
		if result.RowsAffected != 1 {
			return 0, nil, errors.New("cluster authorization revision is missing or invalid")
		}
		if _, readErr := clusterTransportAdmissions(tx); readErr != nil {
			return 0, nil, readErr
		}
		return 0, nil, err
	}
	if len(snapshot) == 0 || !snapshot[0].RevisionPresent {
		return 0, nil, errors.New("cluster authorization revision is missing or invalid")
	}
	admissions := make([]ClusterAdmission, 0, len(snapshot))
	for _, row := range snapshot {
		if row.AdmissionPresent {
			admissions = append(admissions, row.Admission)
		}
	}
	if err := validateClusterTransportAdmissions(admissions); err != nil {
		return 0, nil, err
	}
	return snapshot[0].AuthorizationRevision, admissions, nil
}

func requireClusterMaterialGrantRecordedAndPhase(tx *gorm.DB, ctx context.Context, expected ClusterAdmission, phase MaintenancePhaseState) error {
	var final struct {
		Admission        ClusterAdmission      `gorm:"embedded;embeddedPrefix:admission_"`
		Phase            MaintenancePhaseState `gorm:"embedded;embeddedPrefix:phase_"`
		AdmissionPresent bool
	}
	err := tx.Raw(`SELECT a.key_fingerprint IS NOT NULL AS admission_present,
		a.key_fingerprint AS admission_key_fingerprint, a.invitation_hash AS admission_invitation_hash,
		a.address AS admission_address, a.enrollment_origin AS admission_enrollment_origin,
		a.admitted_at AS admission_admitted_at, a.revoked_at AS admission_revoked_at,
		a.material_grant_sequence AS admission_material_grant_sequence,
		p.phase AS phase_phase, p.revision AS phase_revision
		FROM (SELECT 1) AS singleton
		LEFT JOIN cluster_admissions AS a ON a.key_fingerprint = ?
		LEFT JOIN maintenance_phase AS p ON p.id = 1`, expected.KeyFingerprint).Scan(&final).Error
	if err != nil {
		return err
	}
	// Unlike the initial read, include revoked rows so their full tuple fails
	// the recorded comparison rather than being mistaken for a missing row.
	if !final.AdmissionPresent {
		return gorm.ErrRecordNotFound
	}
	if err := requireClusterMaterialGrantValue(expected, final.Admission); err != nil {
		return err
	}
	if !validMaintenancePhase(final.Phase.Phase) || final.Phase.Revision <= 0 {
		return ErrMaintenancePhaseUnavailable
	}
	if err := requireServingClusterPhase(final.Phase); err != nil {
		return err
	}
	if final.Phase != phase {
		return ErrMaintenancePhaseChanged
	}
	return ctx.Err()
}
