//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

func TestRuntimeDqliteAddressActivationRecoversPublication(t *testing.T) {
	cfg, enrollmentAddress := enrollmentNativeConfig(t)
	// testing.TempDir creates its numbered child with 0777 masked by umask;
	// use the installed server's non-group-writable data-root permissions.
	if err := os.Chmod(cfg.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := InitializeEnrollment(ctx, cfg, enrollmentAddress, "https://"+enrollmentAddress); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	seedKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, "gpuworks.db")
	if _, err := ActivateDatabaseAddress(ctx, cfg, enrollmentAddress); err == nil {
		t.Fatal("database activated on enrollment listener port")
	}
	if _, err := os.Lstat(path + ".maintenance"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected listener conflict published maintenance: %v", err)
	}
	st, err := store.OpenRuntime(ctx, path, bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("before-address-activation")
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	address, wrongAddress := testport.Address(t), testport.Address(t)
	cut := errors.New("injected publication stop")
	var expected cluster.Member
	var intent addressActivationIntent
	var ready addressActivationReady
	for _, phase := range []string{"intent", "candidate", "ready", "exchange", "complete", "unblock"} {
		if !t.Run(phase, func(t *testing.T) {
			reached := false
			_, err := activateDatabaseAddress(ctx, cfg, address, func(point string) error {
				if point == phase {
					reached = true
					return cut
				}
				return nil
			})
			if !reached || !errors.Is(err, cut) {
				t.Fatalf("checkpoint %s reached=%v err=%v", phase, reached, err)
			}
			_, release, err := store.LockRuntimeDatabase(path)
			if err != nil {
				t.Fatalf("failed operation retained runtime lock: %v", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if phase == "unblock" {
				if _, err := os.Lstat(path + ".maintenance"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unblock retained startup barrier: %v", err)
				}
				return
			}
			if err := readAddressActivation(path+".maintenance", &intent); err != nil {
				t.Fatal(err)
			}
			if runtime, err := store.OpenRuntime(ctx, path, bus.New(), store.ReplicatedBackend()); err == nil {
				_ = runtime.Close()
				t.Fatal("maintenance admitted normal runtime")
			}
			if _, err := ActivateDatabaseAddress(ctx, cfg, wrongAddress); err == nil {
				t.Fatal("resume accepted a different destination")
			}
			if phase == "ready" || phase == "exchange" || phase == "complete" {
				if err := readAddressActivation(filepath.Join(intent.Operation, "ready.json"), &ready); err != nil {
					t.Fatal(err)
				}
				expected = ready.Member
			}
			if phase == "ready" {
				tamper := filepath.Join(ready.Candidate, "unexpected-resource")
				if err := os.WriteFile(tamper, []byte("must not overwrite or ignore"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := ActivateDatabaseAddress(ctx, cfg, address); err == nil {
					t.Fatal("changed candidate activated")
				}
				digest, err := addressActivationDigest(ctx, path+".dqlite")
				if err != nil || digest != intent.SourceDigest {
					t.Fatalf("rejected candidate changed source: %s %v", digest, err)
				}
				if _, err := os.Stat(tamper); err != nil {
					t.Fatalf("rejection removed unknown state: %v", err)
				}
				if err := os.Remove(tamper); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "exchange" || phase == "complete" {
				preserved, err := addressActivationDigest(ctx, ready.Candidate)
				if err != nil || preserved != ready.SourceDigest {
					t.Fatalf("exchange did not preserve original: %s %v", preserved, err)
				}
				active, err := addressActivationDigest(ctx, path+".dqlite")
				if err != nil || active != ready.CandidateDigest {
					t.Fatalf("exchange active generation: %s %v", active, err)
				}
			}
		}) {
			return
		}
	}
	member, err := ActivateDatabaseAddress(ctx, cfg, address)
	if err != nil || member != expected {
		t.Fatalf("completion retry: %#v expected %#v: %v", member, expected, err)
	}
	var admittedAt time.Time
	assertSeed := func(t *testing.T, st *store.Store) store.ClusterAdmission {
		t.Helper()
		rows, err := st.ClusterTransportAdmissions(ctx, bundle.info.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected exactly one seed admission, got %+v", rows)
		}
		row := rows[0]
		if row.InvitationHash != "" || row.KeyFingerprint != seedKey || row.Address != address || row.RevokedAt != nil || row.AdmittedAt.IsZero() {
			t.Fatalf("incorrect seed admission: %+v", row)
		}
		if !admittedAt.IsZero() && !row.AdmittedAt.Equal(admittedAt) {
			t.Fatalf("seed retry changed admission time: %v -> %v", admittedAt, row.AdmittedAt)
		}
		admittedAt = row.AdmittedAt
		return row
	}
	withStore := func(t *testing.T, check func(*store.Store)) {
		t.Helper()
		st, err := store.OpenRuntime(ctx, path, bus.New(), store.ReplicatedBackend())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := st.Close(); err != nil {
				t.Error(err)
			}
		}()
		check(st)
	}
	var afterUUID string
	for restart := 0; restart < 2; restart++ {
		a, err := New(cfg, enrollmentNativeLog())
		if err != nil {
			t.Fatalf("App restart %d: %v", restart, err)
		}
		t.Cleanup(func() { _ = a.Close() })
		stop := startEnrollmentNativeApp(t, a)
		assertSeed(t, a.store)
		status, data := enrollmentNativeRequest(t, &http.Client{Timeout: 5 * time.Second}, "http://"+a.Addr()+"/api/v1/auth/login", "", map[string]string{"username": cfg.AdminUser, "password": cfg.AdminPassword})
		if status != http.StatusOK {
			stop()
			_ = a.Close()
			t.Fatalf("login %d: %d %s", restart, status, data)
		}
		if found, err := a.store.NodeByUUID(node.UUID); err != nil || found.Name != node.Name {
			stop()
			_ = a.Close()
			t.Fatalf("activation lost original resource: %v", err)
		}
		if restart == 0 {
			after, _, err := a.store.CreateNodePending("after-address-activation")
			if err != nil {
				stop()
				_ = a.Close()
				t.Fatal(err)
			}
			afterUUID = after.UUID
		} else if found, err := a.store.NodeByUUID(afterUUID); err != nil || found.UUID != afterUUID {
			stop()
			_ = a.Close()
			t.Fatalf("restart lost new resource: %v", err)
		}
		stop()
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		// SQL writes and native restarts change the active tree. Completed retries
		// must validate live member/authority instead of the pre-start cold hash.
		member, err := ActivateDatabaseAddress(ctx, cfg, address)
		if err != nil || member != expected {
			t.Fatalf("retry after business writes: %#v %v", member, err)
		}
	}
	// Older activated installations have transport material but no SQL seed.
	// Opening the runtime cannot silently grant this missing permission.
	withStore(t, func(st *store.Store) {
		assertSeed(t, st)
		result := st.DB().Where("invitation_hash = ?", "").Delete(&store.ClusterAdmission{})
		if result.Error != nil || result.RowsAffected != 1 {
			t.Fatalf("remove legacy seed: rows=%d err=%v", result.RowsAffected, result.Error)
		}
	})
	withStore(t, func(st *store.Store) {
		var count int64
		if err := st.DB().Model(&store.ClusterAdmission{}).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("normal runtime recreated seed authorization")
		}
		if _, err := st.ClusterTransportAdmissions(ctx, bundle.info.ClusterID); err == nil {
			t.Fatal("snapshot accepted a missing seed")
		}
	})
	previousAdmission := admittedAt
	admittedAt = time.Time{}
	for retry := 0; retry < 2; retry++ {
		member, err := ActivateDatabaseAddress(ctx, cfg, address)
		if err != nil || member != expected {
			t.Fatalf("explicit legacy seed recovery: %#v %v", member, err)
		}
		withStore(t, func(st *store.Store) { assertSeed(t, st) })
	}
	if !admittedAt.After(previousAdmission) {
		t.Fatal("legacy seed recovery did not create a fresh admission")
	}
	// An explicit retry cannot overwrite a conflicting seed or remove revocation.
	for _, kind := range []string{"conflicting_address", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			revokedAt := time.Now().UTC().Truncate(time.Microsecond)
			withStore(t, func(st *store.Store) {
				update := map[string]any{"address": wrongAddress}
				if kind == "revoked" {
					update = map[string]any{"revoked_at": revokedAt}
				}
				if err := st.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", seedKey).Updates(update).Error; err != nil {
					t.Fatal(err)
				}
			})
			if _, err := ActivateDatabaseAddress(ctx, cfg, address); !errors.Is(err, store.ErrClusterAdmissionRejected) {
				t.Fatalf("%s seed accepted: %v", kind, err)
			}
			withStore(t, func(st *store.Store) {
				var row store.ClusterAdmission
				if err := st.DB().Where("key_fingerprint = ?", seedKey).First(&row).Error; err != nil {
					t.Fatal(err)
				}
				if !row.AdmittedAt.Equal(admittedAt) {
					t.Fatal("rejected retry changed admission time")
				}
				if kind == "revoked" {
					if row.RevokedAt == nil || !row.RevokedAt.Equal(revokedAt) {
						t.Fatal("rejected retry cleared revocation")
					}
				} else if row.Address != wrongAddress {
					t.Fatal("rejected retry replaced conflicting address")
				}
			})
			// Put the seed back for the next case. The member's own Store
			// cannot: a member whose admission is revoked commits nothing, and
			// so cannot clear its own revocation. A Store on the same database
			// that is not bound to the member's key stands for a repair made
			// outside the member.
			func() {
				db, err := cluster.OpenConfiguredDatabase(ctx, path+".dqlite", filepath.Base(path))
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				}()
				repair, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
				if err != nil {
					t.Fatal(err)
				}
				if err := repair.DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", seedKey).Updates(map[string]any{"address": address, "revoked_at": nil}).Error; err != nil {
					t.Fatal(err)
				}
			}()
			withStore(t, func(st *store.Store) { assertSeed(t, st) })
		})
	}
	// Even under the same CA, the SQL seed must identify the actual database
	// member certificate rather than a different enrollment server key.
	replacementKey, err := identity.NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	replacementCSR, err := identity.NewMemberCSR(replacementKey)
	if err != nil {
		t.Fatal(err)
	}
	replacementFingerprint, err := identity.CSRKeyFingerprint(replacementCSR)
	if err != nil {
		t.Fatal(err)
	}
	replacementCertificate, err := bundle.issuer.IssueMember(replacementCSR, replacementFingerprint, address, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	replaceAddressActivationTransport(t, cfg, address, bundle.info.ClusterID, bundle.info.CAFingerprint, bundle.ca, replacementCertificate, replacementKey, replacementFingerprint)
	if _, err := ActivateDatabaseAddress(ctx, cfg, address); !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("same-authority different transport member accepted: %v", err)
	}
	withStore(t, func(st *store.Store) { assertSeed(t, st) })
	// Cryptographically coherent transport material from another authority must
	// not turn a completed retry into success for the original SQL/enrollment.
	replaceAddressActivationTransportWithForeignAuthority(t, cfg, address)
	if _, err := ActivateDatabaseAddress(ctx, cfg, address); !errors.Is(err, store.ErrClusterIdentityConflict) {
		t.Fatalf("foreign transport accepted on completed retry: %v", err)
	}
}

func replaceAddressActivationTransportWithForeignAuthority(t *testing.T, cfg Config, address string) {
	t.Helper()
	now := time.Now().UTC()
	foreignID := strings.Repeat("f", 32)
	ca, caKey, err := identity.NewAuthority(foreignID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, foreignID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	replaceAddressActivationTransport(t, cfg, address, foreignID, pin, ca, cert, key, fingerprint)
}

func replaceAddressActivationTransport(t *testing.T, cfg Config, address, clusterID, pin string, ca, cert, key []byte, fingerprint string) {
	t.Helper()
	path := filepath.Join(cfg.DataDir, "gpuworks.db")
	_, release, err := store.LockRuntimeDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	saved := t.TempDir()
	for _, name := range []string{".transport", ".transport-required"} {
		if err := os.Rename(filepath.Join(path+".dqlite", name), filepath.Join(saved, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := cluster.PrepareDatabaseTransport(path+".dqlite", address, clusterID, pin, ca, cert, key, identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1, AllowedKeys: []string{fingerprint}}); err != nil {
		t.Fatal(err)
	}
}
