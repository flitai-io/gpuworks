//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

// This fixture uses actual authorized native voters and production pinned mTLS
// handlers. Complete persisted enrollment material lets the real worker reload
// its identity; no admission, receipt, publication, or install is stubbed.
func newReplicationInvalidationFixture(t *testing.T, ctx context.Context, intercept func(int, http.ResponseWriter, *http.Request) bool) *candidateGrantFixture {
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
		serverTLS, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, tls: serverTLS, trust: trust, ca: ca, caKey: caKey, issuer: authority, dataDir: filepath.Dir(configs[i].Dir)}
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
			if intercept != nil && intercept(i, w, r) {
				return
			}
			handler.ServeHTTP(w, r)
		}))
		server.TLS = serverTLS.Clone()
		server.StartTLS()
		t.Cleanup(server.Close)
		bundle.info.PublicURL = server.URL
		bundle.info.ListenAddress = server.Listener.Addr().String()
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
		bundle := f.bundles[i]
		raw, err := json.Marshal(bundle.info)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(dataDir, enrollmentDirectory)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{"config.json": raw, "ca.pem": ca, "ca.key": caKey, "server.pem": certificates[i], "server.key": privateKeys[i], "master.binding": enrollmentMasterBinding(caKey, master)} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := readEnrollmentBundle(dataDir, false); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
