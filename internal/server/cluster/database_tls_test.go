//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

// testTransport is one cluster authority with a policy file. The database
// process authorizes every handshake from that file, so fixtures publish the
// keys they issue instead of installing callbacks on a tls.Config.
type testTransport struct {
	authority  *identity.Authority
	trust      *identity.Trust
	caPEM      []byte
	policyPath string
	policy     identity.TransportPolicy
}

type testTransportMember struct {
	listen, dial    *tls.Config
	certPEM, keyPEM []byte
	fingerprint     string
	transport       *testTransport
}

var testTransportMembers sync.Map

func newTestTransport(t *testing.T) *testTransport {
	t.Helper()
	now := time.Now().UTC()
	clusterID := "c4178b0a97cb456cb65709a4d4126d56"
	ca, key, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, key, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return &testTransport{authority: authority, trust: trust, caPEM: ca, policyPath: filepath.Join(dir, "transport.json"),
		policy: identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin}}
}

// publish replaces the allowed and revoked keys in the policy file.
func (tt *testTransport) publish(t *testing.T, allowed, revoked []string) {
	t.Helper()
	next := tt.policy
	next.Revision++
	next.AllowedKeys = slices.Clone(allowed)
	next.RevokedKeys = slices.Clone(revoked)
	sort.Strings(next.AllowedKeys)
	sort.Strings(next.RevokedKeys)
	if err := identity.PublishTransportPolicy(tt.policyPath, tt.policy.Revision, next); err != nil {
		t.Fatal(err)
	}
	tt.policy = next
}

// issue creates a member key and certificate for host without admitting it.
func (tt *testTransport) issue(t *testing.T, host string) *testTransportMember {
	t.Helper()
	now := time.Now().UTC()
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
	certificate, err := tt.authority.IssueMember(csr, fingerprint, net.JoinHostPort(host, "1"), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// This process dials with the same rule the database process applies.
	listen, dial, err := tt.trust.TLSConfigs(certificate, key, now, func(peer string) error {
		policy, err := identity.LoadTransportPolicy(tt.policyPath)
		if err != nil {
			return err
		}
		authorize, err := policy.Authorizer(tt.policy.ClusterID, tt.policy.CAFingerprint)
		if err != nil {
			return err
		}
		return authorize(peer)
	})
	if err != nil {
		t.Fatal(err)
	}
	member := &testTransportMember{listen: listen, dial: dial, certPEM: certificate, keyPEM: key, fingerprint: fingerprint, transport: tt}
	testTransportMembers.Store(string(listen.Certificates[0].Certificate[0]), member)
	return member
}

// admit issues a member and adds its key to the policy file.
func (tt *testTransport) admit(t *testing.T, host string) *testTransportMember {
	t.Helper()
	member := tt.issue(t, host)
	tt.publish(t, append(slices.Clone(tt.policy.AllowedKeys), member.fingerprint), tt.policy.RevokedKeys)
	return member
}

func (m *testTransportMember) apply(cfg *DatabaseConfig) {
	cfg.ListenTLS, cfg.DialTLS = m.listen, m.dial
	cfg.TLSCertPEM, cfg.TLSKeyPEM, cfg.TLSCACertPEM = m.certPEM, m.keyPEM, m.transport.caPEM
	cfg.TransportPolicyPath = m.transport.policyPath
}

// databaseTestTLS returns the configurations of one admitted member of a new
// authority. Two calls are two unrelated clusters.
func databaseTestTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	member := newTestTransport(t).admit(t, "127.0.0.1")
	return member.listen, member.dial
}

func databaseTLSConfig(t *testing.T, server, client *tls.Config) DatabaseConfig {
	t.Helper()
	address := testport.Address(t)
	cfg := DatabaseConfig{Dir: t.TempDir(), Address: address, Name: "gpuworks.db", ListenTLS: server, DialTLS: client}
	if server != nil && len(server.Certificates) != 0 {
		if member, ok := testTransportMembers.Load(string(server.Certificates[0].Certificate[0])); ok {
			member.(*testTransportMember).apply(&cfg)
			cfg.ListenTLS, cfg.DialTLS = server, client
		}
	}
	return cfg
}

func TestReplicatedDatabaseMutualTLS(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serverTLS, clientTLS := databaseTestTLS(t)
	a, err := OpenDatabase(ctx, databaseTLSConfig(t, serverTLS, clientTLS))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := a.Scheduling.ExecContext(ctx, "CREATE TABLE tls_fixture (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SQL.ExecContext(ctx, "INSERT INTO tls_fixture VALUES (1, 'verified')"); err != nil {
		t.Fatal(err)
	}
	otherServer, otherClient := databaseTestTLS(t)
	bad := databaseTLSConfig(t, otherServer, otherClient)
	bad.Seeds = []string{a.Address}
	rejectCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	unauthorized, err := OpenDatabase(rejectCtx, bad)
	stop()
	if err == nil {
		_ = unauthorized.Close()
		t.Fatal("member with wrong cluster certificates joined")
	}
	members, err := a.Members(ctx)
	if err != nil || len(members) != 1 {
		t.Fatalf("unauthorized member changed membership: %+v %v", members, err)
	}
	good := databaseTLSConfig(t, serverTLS, clientTLS)
	good.Seeds = []string{a.Address}
	b, err := OpenDatabase(ctx, good)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	var value string
	if err := b.SQL.QueryRowContext(ctx, "SELECT value FROM tls_fixture WHERE id=1").Scan(&value); err != nil || value != "verified" {
		t.Fatalf("authenticated member cannot read acknowledged value: %q %v", value, err)
	}
}
