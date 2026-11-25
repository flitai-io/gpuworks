package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/cluster"
)

const ClusterProtocolVersion = proto.ClusterProtocolVersion
const MaxClusterInvitationTTL = time.Hour

var (
	ErrClusterNotInitialized        = errors.New("cluster identity has not been initialized")
	ErrClusterIdentityConflict      = errors.New("cluster identity does not match")
	ErrClusterInvitationRejected    = errors.New("cluster invitation is invalid, expired, or already consumed")
	ErrClusterAdmissionRejected     = errors.New("cluster member identity or address is already bound or revoked")
	ErrClusterAdministratorRequired = errors.New("cluster invitation requires an existing administrator")
)

// InitializeClusterIdentity is an explicit authorized initialization step.
// Retrying the same persisted candidate is idempotent; changing it is not.
func (s *Store) InitializeClusterIdentity(ctx context.Context, candidate ClusterIdentity) (ClusterIdentity, error) {
	if !validClusterDigest(candidate.ClusterID, 16) || !validClusterDigest(candidate.CAFingerprint, 32) || candidate.Protocol != ClusterProtocolVersion {
		return ClusterIdentity{}, errors.New("invalid cluster identity or unsupported protocol")
	}
	candidate.ID = 1
	var result ClusterIdentity
	err := s.membershipTransaction(ctx, func(tx *gorm.DB) error {
		candidate.CreatedAt = tx.NowFunc().UTC()
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
			return err
		}
		if err := tx.First(&result, 1).Error; err != nil {
			return err
		}
		if result.ClusterID != candidate.ClusterID || result.CAFingerprint != candidate.CAFingerprint || result.Protocol != candidate.Protocol {
			return ErrClusterIdentityConflict
		}
		return nil
	})
	if err != nil {
		return ClusterIdentity{}, err
	}
	return result, nil
}

func (s *Store) ClusterIdentity(ctx context.Context) (ClusterIdentity, error) {
	var identity ClusterIdentity
	err := s.db.WithContext(ctx).First(&identity, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ClusterIdentity{}, ErrClusterNotInitialized
	}
	return identity, err
}

// IssueClusterInvitation returns its secret only after a successful commit.
// A failed/unknown commit is not retried; callers must not invent success.
func (s *Store) IssueClusterInvitation(ctx context.Context, clusterID string, issuedBy uint, validFor time.Duration) (ClusterInvitation, string, error) {
	return s.IssueClusterInvitationWithOrigin(ctx, clusterID, issuedBy, validFor, "")
}

// IssueClusterInvitationWithOrigin fixes the administrator-approved public
// hostname before the bearer leaves the server. Empty means a database-host
// certificate only; a member cannot widen this binding with its CSR.
func (s *Store) IssueClusterInvitationWithOrigin(ctx context.Context, clusterID string, issuedBy uint, validFor time.Duration, origin string) (ClusterInvitation, string, error) {
	if validFor <= 0 || validFor > MaxClusterInvitationTTL {
		return ClusterInvitation{}, "", errors.New("cluster invitation lifetime must be positive and at most one hour")
	}
	if origin != "" {
		var err error
		origin, err = normalizeClusterEnrollmentOrigin(origin)
		if err != nil {
			return ClusterInvitation{}, "", err
		}
	}
	token := auth.NewToken(32)
	if token == "" {
		return ClusterInvitation{}, "", errors.New("generate cluster invitation")
	}
	var invitation ClusterInvitation
	// Issuing is an administrator's request served by a member: its
	// statements check the administrator, so the member is checked here.
	err := s.clusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		var administrator User
		err := tx.Where("id = ? AND role = ?", issuedBy, "admin").First(&administrator).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterAdministratorRequired
		}
		if err != nil {
			return err
		}
		now := tx.NowFunc().UTC()
		invitation = ClusterInvitation{TokenHash: HashToken(token), ClusterID: clusterID, IssuedBy: issuedBy, AuthorizedEnrollmentOrigin: origin, CreatedAt: now, ExpiresAt: now.Add(validFor)}
		return tx.Create(&invitation).Error
	})
	if err != nil {
		return ClusterInvitation{}, "", err
	}
	return invitation, token, nil
}

// ClaimClusterInvitation atomically consumes a bearer invitation exactly once.
// Authentication of the server and proof of possession of the submitted key
// belong to the transport layer and must precede this repository operation.
func (s *Store) ClaimClusterInvitation(ctx context.Context, clusterID, token, keyFingerprint, address string) (ClusterAdmission, error) {
	return s.ClaimClusterInvitationWithOrigin(ctx, clusterID, token, keyFingerprint, address, "")
}

// ClaimClusterInvitationWithOrigin rejects a changed public host before the
// one-use bearer is consumed. Its returned admission is not a certificate
// signing instruction; the issuer must re-read the committed invitation.
func (s *Store) ClaimClusterInvitationWithOrigin(ctx context.Context, clusterID, token, keyFingerprint, address, origin string) (ClusterAdmission, error) {
	if !validClusterDigest(token, 32) || !validClusterDigest(keyFingerprint, 32) {
		return ClusterAdmission{}, ErrClusterInvitationRejected
	}
	if origin != "" {
		var err error
		origin, err = normalizeClusterEnrollmentOrigin(origin)
		if err != nil {
			return ClusterAdmission{}, err
		}
	}
	address, err := normalizeClusterMemberAddress(address)
	if err != nil {
		return ClusterAdmission{}, err
	}
	var admission ClusterAdmission
	err = s.membershipWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		var invitation ClusterInvitation
		err := tx.Where("token_hash = ? AND cluster_id = ?", HashToken(token), clusterID).First(&invitation).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterInvitationRejected
		}
		if err != nil {
			return err
		}
		now := tx.NowFunc().UTC()
		if invitation.ConsumedAt != nil || !invitation.ExpiresAt.After(now) || invitation.AuthorizedEnrollmentOrigin != origin {
			return ErrClusterInvitationRejected
		}
		claimed := tx.Model(&ClusterInvitation{}).Where("token_hash = ? AND consumed_at IS NULL", invitation.TokenHash).Update("consumed_at", now)
		if claimed.Error != nil {
			return claimed.Error
		}
		if claimed.RowsAffected != 1 {
			return ErrClusterInvitationRejected
		}
		// Tombstoned keys cannot become members again. A new key may reuse an
		// address only after its previous admission was explicitly revoked.
		var conflicts int64
		if err := tx.Model(&ClusterAdmission{}).Where("key_fingerprint = ? OR (address = ? AND revoked_at IS NULL)", keyFingerprint, address).Count(&conflicts).Error; err != nil {
			return err
		}
		if conflicts != 0 {
			return ErrClusterAdmissionRejected
		}
		admission = ClusterAdmission{KeyFingerprint: keyFingerprint, InvitationHash: invitation.TokenHash, Address: address, AdmittedAt: now}
		if err := tx.Create(&admission).Error; err != nil {
			return err
		}
		return advanceClusterAuthorization(tx)
	})
	if err != nil {
		return ClusterAdmission{}, err
	}
	return admission, nil
}

// ClusterAdmissionForVerifiedKey reads an existing binding after the caller has
// authenticated possession of that key. A fingerprint is not a credential.
// It does not accept the invitation bearer and does not grant fresh admission.
func (s *Store) ClusterAdmissionForVerifiedKey(ctx context.Context, clusterID, keyFingerprint string) (ClusterAdmission, error) {
	return s.clusterAdmission(ctx, clusterID, keyFingerprint)
}

// ClusterAdmissionForCertificate is only for reissuing public certificates to
// a previously admitted CSR. Its caller must verify the CSR signature; replay
// of that public CSR is not live authentication and grants no access to secrets.
func (s *Store) ClusterAdmissionForCertificate(ctx context.Context, clusterID, keyFingerprint string) (ClusterAdmission, error) {
	return s.clusterAdmission(ctx, clusterID, keyFingerprint)
}

// ClusterCertificateAuthorization resolves the public hostname from the
// consumed invitation in the same snapshot as the admitted key. A missing or
// damaged invitation fails closed; the request and CSR cannot add a SAN.
func (s *Store) ClusterCertificateAuthorization(ctx context.Context, clusterID, keyFingerprint string) (ClusterAdmission, string, error) {
	if !validClusterDigest(keyFingerprint, 32) {
		return ClusterAdmission{}, "", ErrClusterAdmissionRejected
	}
	var admission ClusterAdmission
	var origin string
	err := s.membershipTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		if err := tx.Where("key_fingerprint = ? AND revoked_at IS NULL", keyFingerprint).First(&admission).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterAdmissionRejected
		} else if err != nil {
			return err
		}
		if admission.InvitationHash == "" {
			return nil // The seed has no invitation; its offline certificate is separate.
		}
		var invitation ClusterInvitation
		if err := tx.Where("token_hash = ? AND cluster_id = ?", admission.InvitationHash, clusterID).First(&invitation).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterAdmissionRejected
		} else if err != nil {
			return err
		}
		if invitation.ConsumedAt == nil {
			return ErrClusterAdmissionRejected
		}
		origin = invitation.AuthorizedEnrollmentOrigin
		if origin != "" {
			normalized, err := normalizeClusterEnrollmentOrigin(origin)
			if err != nil || normalized != origin {
				return ErrClusterEnrollmentOriginInvalid
			}
		}
		return nil
	})
	if err != nil {
		return ClusterAdmission{}, "", err
	}
	return admission, origin, nil
}

func (s *Store) clusterAdmission(ctx context.Context, clusterID, keyFingerprint string) (ClusterAdmission, error) {
	if !validClusterDigest(keyFingerprint, 32) {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// A single statement keeps identity and admission in one read snapshot
	// without entering the native writer queue. This observation grants no
	// material authority and must never increment its sequence.
	var snapshot struct {
		Identity  ClusterIdentity  `gorm:"embedded;embeddedPrefix:identity_"`
		Admission ClusterAdmission `gorm:"embedded;embeddedPrefix:admission_"`
	}
	db := s.db.WithContext(ctx)
	err := db.Raw(`SELECT i.id AS identity_id, i.cluster_id AS identity_cluster_id,
		i.ca_fingerprint AS identity_ca_fingerprint, i.protocol AS identity_protocol,
		i.created_at AS identity_created_at,
		a.key_fingerprint AS admission_key_fingerprint, a.invitation_hash AS admission_invitation_hash,
		a.address AS admission_address, a.enrollment_origin AS admission_enrollment_origin,
		a.admitted_at AS admission_admitted_at, a.revoked_at AS admission_revoked_at,
		a.material_grant_sequence AS admission_material_grant_sequence
		FROM (SELECT 1) AS singleton
		LEFT JOIN cluster_identities AS i ON i.id = 1
		LEFT JOIN cluster_admissions AS a ON a.key_fingerprint = ? AND a.revoked_at IS NULL`, keyFingerprint).Scan(&snapshot).Error
	if err != nil {
		// A damaged admission table may fail before the identity is returned.
		// Preserve the original identity-first error order; this fallback only
		// classifies failure and cannot produce an admitted snapshot.
		if identityErr := requireClusterIdentity(db, clusterID); identityErr != nil {
			return ClusterAdmission{}, identityErr
		}
		return ClusterAdmission{}, err
	}
	if snapshot.Identity.ID == 0 {
		return ClusterAdmission{}, ErrClusterNotInitialized
	}
	if snapshot.Identity.ClusterID != clusterID || snapshot.Identity.Protocol != ClusterProtocolVersion {
		return ClusterAdmission{}, ErrClusterIdentityConflict
	}
	if snapshot.Admission.KeyFingerprint == "" {
		// No admission that is not revoked: this key is refused.
		return ClusterAdmission{}, &ClusterAdmissionRefusal{Key: keyFingerprint}
	}
	return snapshot.Admission, nil
}

// clusterTransaction is a member's write like any other: the writer guard
// checks it.
func (s *Store) clusterTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	// Every admission, grant and material write keeps the five-second cap,
	// whatever longer deadline its caller carries; a shorter one still wins.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.db.WithContext(ctx).Transaction(fn)
}

// membershipTransaction and membershipWriteTransaction run what establishes
// or decides membership, where the writer guard cannot apply: the cluster
// identity and the seed admission (no admission exists yet), claiming an
// invitation and revocation (their statements check the named key), the
// membership, transport and material grants (a member asks about itself
// through them and is refused by them), the reads of admissions those grants
// rest on, and revoking the other admissions after a restore. Nothing else
// may use them; the inventory test counts their callers.
func (s *Store) membershipTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	return s.clusterTransaction(withoutWriterGuard(ctx), fn)
}

func (s *Store) membershipWriteTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	return s.clusterWriteTransaction(withoutWriterGuard(ctx), fn)
}

// clusterWriteTransaction acquires SQLite's writer before reading admission
// state, avoiding a stale read snapshot's non-retryable upgrade under concurrent
// authorization refresh. An UPDATE with no matching rows still owns the writer
// without dirtying the identity row or its indexes. This is NOT quorum proof:
// grants require their distinct sequence increment and confirmed commit; no
// transaction or unknown commit is retried here.
func (s *Store) clusterWriteTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	started := time.Now().UTC()
	var observation *cluster.TransactionBeginObservation
	if s.clusterGrantDiagnosticLogger().Enabled(ctx, slog.LevelDebug) {
		ctx, observation = cluster.ObserveTransactionBegin(ctx)
	}
	phase := "begin"
	begin, writer, body, completion := time.Duration(-1), time.Duration(-1), time.Duration(-1), time.Duration(-1)
	var completionStarted time.Time
	err := s.clusterTransaction(ctx, func(tx *gorm.DB) error {
		begin = time.Since(started)
		// Transaction completion can be COMMIT or ROLLBACK. Keep its duration
		// separate, and retain the original failure stage on callback failure.
		defer func() { completionStarted = time.Now().UTC() }()
		phase = "writer_acquisition"
		phaseStarted := time.Now().UTC()
		err := tx.Exec("UPDATE cluster_identities SET id = id WHERE 0").Error
		writer = time.Since(phaseStarted)
		if err != nil {
			return err
		}
		phase = "body"
		phaseStarted = time.Now().UTC()
		err = fn(tx)
		body = time.Since(phaseStarted)
		if err == nil {
			phase = "transaction_completion"
		}
		return err
	})
	if completionStarted.IsZero() {
		begin = time.Since(started)
	} else {
		completion = time.Since(completionStarted)
	}
	if err == nil {
		phase = "complete"
	}
	// begin_ms retains its original meaning, including admission queue, pool
	// acquisition and actual driver BEGIN. Split stages are diagnostic only.
	// Emit only after Transaction returns: logging must not hold the writer.
	timings := append(clusterBeginDiagnosticTimings(ctx, observation),
		clusterGrantTiming("begin_ms", begin),
		clusterGrantTiming("writer_acquisition_ms", writer),
		clusterGrantTiming("body_ms", body),
		clusterGrantTiming("transaction_completion_ms", completion))
	s.logClusterGrantDiagnostic(ctx, "write_transaction", phase, started, err, timings...)
	return err
}

func requireClusterIdentity(tx *gorm.DB, expected string) error {
	var identity ClusterIdentity
	if err := tx.First(&identity, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterNotInitialized
		}
		return err
	}
	if identity.ClusterID != expected || identity.Protocol != ClusterProtocolVersion {
		return ErrClusterIdentityConflict
	}
	return nil
}

func validClusterDigest(value string, bytes int) bool {
	if len(value) != bytes*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}

// NormalizeClusterMemberAddress applies the persisted admission address
// policy before an enrollment handler consumes a one-use invitation.
func NormalizeClusterMemberAddress(address string) (string, error) {
	return normalizeClusterMemberAddress(address)
}

func normalizeClusterMemberAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("member address must contain an explicit host and port: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || host == "" {
		return "", errors.New("invalid member address")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("member address must identify a reachable unicast endpoint")
		}
		host = ip.String()
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if len(host) > 253 {
			return "", errors.New("invalid member hostname")
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", errors.New("invalid member hostname")
			}
			for _, c := range label {
				if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
					return "", errors.New("invalid member hostname")
				}
			}
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}
