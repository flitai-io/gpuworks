//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

type candidateGrantFixture struct {
	authority    *identity.Authority
	authorityKey []byte
	stores       []*store.Store
	databases    []*cluster.Database
	bundles      []*enrollmentBundle
	receivers    []*materialReceiver
	pointers     []store.MaterialManifestRecord
	keys         []string
	pairs        []tls.Certificate
	requests     atomic.Int64
	// asked counts every request each member's material endpoint received;
	// down makes a member's endpoint drop its connections unanswered.
	asked  [3]atomic.Int64
	down   [3]atomic.Bool
	mu     sync.Mutex
	before func()
}

// The fixture owns actual authorized native voters and production mTLS batch
// handlers. Its immutable inventory has no catalog or benchmark consumers and
// needs only the installed shared master and one agent override.
func newCandidateGrantFixture(t *testing.T, ctx context.Context) *candidateGrantFixture {
	t.Helper()
	return newCandidateGrantFixtureWithSourceID(t, ctx, 0)
}

func newCandidateGrantFixtureWithSourceID(t *testing.T, ctx context.Context, sourceID uint64) *candidateGrantFixture {
	t.Helper()
	f := &candidateGrantFixture{}
	now := time.Now().UTC()
	clusterID := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	f.authority = authority
	f.authorityKey = caKey
	expected := store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}
	var configs []cluster.DatabaseConfig
	var certificates, privateKeys [][]byte
	policy := identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1}
	for range 3 {
		address := testport.Address(t)
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
		certificate, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		if err := os.Chmod(dataDir, 0700); err != nil {
			t.Fatal(err)
		}
		configs = append(configs, cluster.DatabaseConfig{Dir: filepath.Join(dataDir, "database"), Address: address, Name: "gpuworks.db"})
		certificates, privateKeys = append(certificates, certificate), append(privateKeys, key)
		f.keys = append(f.keys, fingerprint)
		policy.AllowedKeys = append(policy.AllowedKeys, fingerprint)
	}
	slices.Sort(policy.AllowedKeys)
	for i, cfg := range configs {
		policyPath := filepath.Join(filepath.Dir(cfg.Dir), "transport.json")
		if err := identity.PublishTransportPolicy(policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			cfg.Seeds = []string{configs[0].Address}
		}
		if i == 1 && sourceID != 0 {
			if _, err := cluster.PrepareJoinIdentity(ctx, cfg.Dir, cfg.Address, cfg.Seeds); err != nil {
				t.Fatal(err)
			}
			// Use a real persisted joining ID with the high uint64 bit set. The
			// native SDK loads this ID unchanged; SQL stores its int64 bit pattern.
			info := fmt.Sprintf("ID: %d\nAddress: %q\nRole: 0\n", sourceID, cfg.Address)
			if err := os.WriteFile(filepath.Join(cfg.Dir, "info.yaml"), []byte(info), 0600); err != nil {
				t.Fatal(err)
			}
		}
		db, err := cluster.OpenAuthorizedDatabase(ctx, cfg, policyPath, trust, certificates[i], privateKeys[i])
		if err != nil {
			t.Fatal(err)
		}
		f.databases = append(f.databases, db)
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		if i == 0 {
			st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
			if err != nil {
				t.Fatal(err)
			}
			if err := st.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := st.InitializeClusterIdentity(ctx, expected); err != nil {
				t.Fatal(err)
			}
			if _, err := st.InitializeClusterSeedAdmission(ctx, clusterID, f.keys[0], configs[0].Address); err != nil {
				t.Fatal(err)
			}
			admin := store.User{Username: "grant-proof-admin", Role: "admin"}
			if err := st.DB().Create(&admin).Error; err != nil {
				t.Fatal(err)
			}
			for index := 1; index < 3; index++ {
				_, token, err := st.IssueClusterInvitation(ctx, clusterID, admin.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.ClaimClusterInvitation(ctx, clusterID, token, f.keys[index], configs[index].Address); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for {
		members, err := f.databases[0].Members(ctx)
		if err == nil && len(members) == 3 && slices.ContainsFunc(members, func(m cluster.Member) bool { return m.Role == "voter" }) {
			voters := 0
			for _, m := range members {
				if m.Role == "voter" {
					voters++
				}
			}
			if voters == 3 {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i, db := range f.databases {
		st, err := store.OpenJoinedMemberStore(ctx, db, bus.New())
		if err != nil {
			t.Fatal(err)
		}
		f.stores = append(f.stores, st)
		pair, err := tls.X509KeyPair(append(bytes.Clone(certificates[i]), ca...), privateKeys[i])
		if err != nil {
			t.Fatal(err)
		}
		f.pairs = append(f.pairs, pair)
		serverTLS, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, tls: serverTLS, trust: trust, ca: ca}
		f.bundles = append(f.bundles, bundle)
		receiverRoot := filepath.Join(filepath.Dir(configs[i].Dir), "material-replicas")
		if err := os.MkdirAll(receiverRoot, 0700); err != nil {
			t.Fatal(err)
		}
		receiver, err := newMaterialReceiver(receiverRoot, db.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.receivers = append(f.receivers, receiver)
		handler := enrollmentMaterialHandlerWithReceiver(bundle, st, http.NotFoundHandler(), nil, receiver)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.asked[i].Add(1)
			if f.down[i].Load() {
				if hijacker, ok := w.(http.Hijacker); ok {
					if conn, _, err := hijacker.Hijack(); err == nil {
						_ = conn.Close()
						return
					}
				}
			}
			if r.Method == http.MethodPost && r.URL.Path == materialBatchPath {
				f.requests.Add(1)
				f.mu.Lock()
				before := f.before
				f.mu.Unlock()
				if before != nil {
					before()
				}
			}
			handler.ServeHTTP(w, r)
		}))
		server.TLS = serverTLS.Clone()
		server.StartTLS()
		t.Cleanup(server.Close)
		bundle.info.PublicURL = server.URL
		if err := st.SetClusterEnrollmentOriginForVerifiedKey(ctx, expected, f.keys[i], server.URL); err != nil {
			t.Fatal(err)
		}
	}
	master, override := []byte("shared tiny master"), []byte("tiny agent override")
	for i, receiver := range f.receivers {
		dataDir := filepath.Dir(receiver.root)
		if err := os.MkdirAll(filepath.Join(dataDir, "dl"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "master.key"), master, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "dl", "tiny-agent"), override, 0600); err != nil {
			t.Fatal(err)
		}
		for _, body := range [][]byte{master, override} {
			f.put(t, ctx, i, body)
		}
	}
	for source := 1; source < 3; source++ {
		f.pointers = append(f.pointers, f.publish(t, ctx, source))
	}
	return f
}

// publish records a member as a source of the fixture's two material files:
// its manifest is put on every member and its pointer written to the
// database. The fixture publishes the second and third member; a test that
// needs the first as a source as well calls it.
func (f *candidateGrantFixture) publish(t *testing.T, ctx context.Context, source int) store.MaterialManifestRecord {
	t.Helper()
	master, override := []byte("shared tiny master"), []byte("tiny agent override")
	expected := f.expected()
	manifest := materialManifest{Version: materialManifestVersion, ClusterID: expected.ClusterID, SourceMemberID: f.databases[source].ID, SourceKey: f.keys[source], Classes: slices.Clone(requiredMaterialClasses), Files: []materialManifestFile{
		{Class: MaterialAgentOverride, Path: "dl/tiny-agent", SHA256: materialTestDigest(override), Bytes: int64(len(override))},
		{Class: MaterialMasterSecret, Path: "master.key", SHA256: materialTestDigest(master), Bytes: int64(len(master))},
	}}
	slices.SortFunc(manifest.Files, func(a, b materialManifestFile) int { return strings.Compare(string(a.Class), string(b.Class)) })
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseMaterialManifest(raw, expected, manifest.SourceMemberID, manifest.SourceKey); err != nil {
		t.Fatal(err)
	}
	for i := range f.receivers {
		f.put(t, ctx, i, raw)
	}
	pointer := store.MaterialManifestRecord{SourceMemberID: int64(manifest.SourceMemberID), ClusterID: expected.ClusterID, SourceKey: f.keys[source], Digest: materialTestDigest(raw), Bytes: int64(len(raw)), RecordedAt: time.Now().UTC()}
	if err := f.stores[0].DB().WithContext(ctx).Create(&pointer).Error; err != nil {
		t.Fatal(err)
	}
	return pointer
}

func (f *candidateGrantFixture) put(t *testing.T, ctx context.Context, index int, body []byte) {
	t.Helper()
	receiver := f.receivers[index]
	digest := materialTestDigest(body)
	if _, err := receiver.receive(ctx, digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := f.stores[index].RecordLocalMaterialReplicaReference(ctx, digest, receiver.memberID, filepath.Join(receiver.root, digest[:2], digest)); err != nil {
		t.Fatal(err)
	}
}

func (f *candidateGrantFixture) prove(ctx context.Context) (bool, CandidateProofMetrics, error) {
	recorder := &candidateProofRecorder{}
	ctx = context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
	ready, err := checkJoinedStoreMaterialReadiness(ctx, f.stores[0], f.bundles[0], f.databases[0].ID, Config{DataDir: filepath.Dir(f.receivers[0].root)})
	return ready, recorder.snapshot(), err
}
