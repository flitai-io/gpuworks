//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// statusProofPort is one member's database port as the status read sees it:
// mutual TLS under the cluster authority, then one answer to the quorum proof
// request. A nil answer is the failure a member that is not the leader gives.
type statusProofPort struct {
	address string
	// stop closes the port, as a member that is stopped.
	stop   func()
	answer func() []byte
	served atomic.Int32
}

type statusProofFixture struct {
	root                   string
	self, follower, leader *statusProofPort
	selfKey, leaderKey     string
	strangerCert           []byte
	strangerKey            []byte
}

func statusProofBody(proof cluster.QuorumProof) []byte {
	word := func(body []byte, value uint64) []byte { return binary.LittleEndian.AppendUint64(body, value) }
	body := word(nil, 1)
	for _, value := range []uint64{proof.Term, proof.Index, proof.ConfigurationIndex, proof.LeaderID, uint64(len(proof.Members))} {
		body = word(body, value)
	}
	for _, member := range proof.Members {
		body = word(body, member.ID)
		text := append([]byte(member.Address), 0)
		for len(text)%8 != 0 {
			text = append(text, 0)
		}
		body = append(body, text...)
		body = word(body, map[string]uint64{"voter": 0, "stand-by": 1, "spare": 2}[member.Role])
	}
	header := binary.LittleEndian.AppendUint32(nil, uint32(len(body)/8))
	return append(append(header, 3, 2, 0, 0), body...)
}

// The failure response: code 1 and a short message, type 0.
func statusProofRefusal() []byte {
	body := binary.LittleEndian.AppendUint64(nil, 1)
	body = append(body, []byte("not leader\x00\x00\x00\x00\x00\x00")...)
	header := binary.LittleEndian.AppendUint32(nil, uint32(len(body)/8))
	return append(append(header, 0, 0, 0, 0), body...)
}

func newStatusProofFixture(t *testing.T) *statusProofFixture {
	t.Helper()
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
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	issue := func(address string) ([]byte, []byte, string) {
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
		return certificate, key, fingerprint
	}
	f := &statusProofFixture{}
	start := func() (*statusProofPort, []byte, []byte, string) {
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := &statusProofPort{address: raw.Addr().String()}
		certificate, key, fingerprint := issue(port.address)
		pair, err := tls.X509KeyPair(append(append([]byte{}, certificate...), ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		listener := tls.NewListener(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
		port.stop = func() { _ = listener.Close() }
		t.Cleanup(port.stop)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					request := make([]byte, 24)
					if _, err := io.ReadFull(conn, request); err != nil {
						return
					}
					port.served.Add(1)
					answer := statusProofRefusal()
					if port.answer != nil {
						answer = port.answer()
					}
					_, _ = conn.Write(answer)
				}()
			}
		}()
		return port, certificate, key, fingerprint
	}
	var selfCert, selfPrivate []byte
	f.self, selfCert, selfPrivate, f.selfKey = start()
	var followerKey string
	f.follower, _, _, followerKey = start()
	f.leader, _, _, f.leaderKey = start()
	f.strangerCert, f.strangerKey, _ = issue("127.0.0.1:1")

	f.root = writeStatusRaft(t, statusIdentity(1, f.self.address, 0), statusMembers(
		statusIdentity(1, f.self.address, 0),
		statusIdentity(2, f.follower.address, 0),
		statusIdentity(3, f.leader.address, 0),
	), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
	// t.TempDir may sit behind a symlink, which the policy refuses.
	if f.root, err = filepath.EvalSymlinks(f.root); err != nil {
		t.Fatal(err)
	}
	materials := filepath.Join(f.root, "cluster-enrollment")
	if err := os.Mkdir(materials, 0o700); err != nil {
		t.Fatal(err)
	}
	// No enrollment listener is reachable: the routes give nothing.
	closed := closedStatusPort(t)
	config := `{"cluster_id":"` + clusterID + `","ca_fingerprint":"` + pin + `","protocol":1,"listen_address":"` + closed + `","public_url":"https://` + closed + `"}`
	for name, data := range map[string][]byte{"config.json": []byte(config), "ca.pem": ca, "server.pem": selfCert, "server.key": selfPrivate} {
		if err := os.WriteFile(filepath.Join(materials, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policyDir := filepath.Join(f.root, "gpuworks.db.dqlite", ".transport")
	if err := os.Mkdir(policyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	allowed := []string{f.selfKey, followerKey, f.leaderKey}
	sort.Strings(allowed)
	policy := identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1, AllowedKeys: allowed, RevokedKeys: []string{}}
	if err := identity.PublishTransportPolicy(filepath.Join(policyDir, "policy.json"), 0, policy); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *statusProofFixture) proof(term uint64) cluster.QuorumProof {
	return cluster.QuorumProof{Term: term, Index: 77, ConfigurationIndex: 12, LeaderID: 3, Members: []cluster.Member{
		{ID: 1, Address: f.self.address, Role: "voter"},
		{ID: 2, Address: f.follower.address, Role: "voter"},
		{ID: 3, Address: f.leader.address, Role: "voter"},
	}}
}

// A serving cluster whose enrollment listeners give no quorum facts still
// reports its leader: the status command reads the proof from the database
// ports, passing the members that are not the leader.
func TestStatusReadsTheLeaderFromTheDatabasePortsWhenNoRouteAnswers(t *testing.T) {
	f := newStatusProofFixture(t)
	f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: f.root}, "", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"Role: voter (live raft)", "Voters: 3", "Fault tolerant: true", "Sync: observed",
		"Leader: 3 " + f.leader.address + "\n", "Term: 5", "Commit index: 77", "Configuration index: 12",
		"  2 " + f.follower.address + " voter (live raft)",
		// The proof names no enrollment origin.
		"  3 " + f.leader.address + " database: open enrollment: not observed material route: not observed",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
	if strings.Contains(text, "(self)") {
		t.Fatalf("a follower reported itself as the leader\n%s", text)
	}
	if f.self.served.Load() != 1 || f.follower.served.Load() != 1 || f.leader.served.Load() != 1 {
		t.Fatalf("proof requests: self=%d follower=%d leader=%d", f.self.served.Load(), f.follower.served.Load(), f.leader.served.Load())
	}
}

func TestStatusDatabaseProofIsUsedOnlyAfterTheRoutesGaveNothing(t *testing.T) {
	f := newStatusProofFixture(t)
	f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
	previous := statusQuorumFetch
	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{LeaderID: 1, Term: 9, Index: 80, ConfigurationIndex: 12, Members: []enrollment.ClusterMember{{ID: 1, Address: f.self.address, Role: "voter"}}}, true
	}
	t.Cleanup(func() { statusQuorumFetch = previous })
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: f.root}, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Term: 9") || f.self.served.Load()+f.follower.served.Load()+f.leader.served.Load() != 0 {
		t.Fatalf("the database ports were read although a route answered\n%s", text)
	}
}

func TestStatusDatabaseProofRefusesWhatIsNotTheLeadersOwnAnswer(t *testing.T) {
	observe := func(t *testing.T, f *statusProofFixture) string {
		t.Helper()
		text, err := ObserveClusterStatus(context.Background(), Config{DataDir: f.root}, "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	unobserved := func(t *testing.T, text string) {
		t.Helper()
		for _, line := range []string{"Leader: not observed", "Term: not observed", "Commit index: not observed", "Role: voter (cluster.yaml cache)"} {
			if !strings.Contains(text, line) {
				t.Fatalf("missing %q\n%s", line, text)
			}
		}
	}
	t.Run("no leader answers", func(t *testing.T) {
		unobserved(t, observe(t, newStatusProofFixture(t)))
	})
	t.Run("a member answers for another leader", func(t *testing.T) {
		f := newStatusProofFixture(t)
		// The follower returns a proof that names member 3 as the leader.
		f.follower.answer = func() []byte { return statusProofBody(f.proof(5)) }
		unobserved(t, observe(t, f))
	})
	t.Run("the policy does not authorize the leader", func(t *testing.T) {
		f := newStatusProofFixture(t)
		f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
		path := filepath.Join(f.root, "gpuworks.db.dqlite", ".transport", "policy.json")
		policy, err := identity.LoadTransportPolicy(path)
		if err != nil {
			t.Fatal(err)
		}
		next := policy
		next.Revision, next.AllowedKeys, next.RevokedKeys = 2, nil, []string{f.leaderKey}
		for _, key := range policy.AllowedKeys {
			if key != f.leaderKey {
				next.AllowedKeys = append(next.AllowedKeys, key)
			}
		}
		if err := identity.PublishTransportPolicy(path, policy.Revision, next); err != nil {
			t.Fatal(err)
		}
		unobserved(t, observe(t, f))
		if f.leader.served.Load() != 0 {
			t.Fatal("a proof request was sent to a member the policy revoked")
		}
	})
	t.Run("the local certificate is not this cluster's member", func(t *testing.T) {
		f := newStatusProofFixture(t)
		f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
		materials := filepath.Join(f.root, "cluster-enrollment")
		if err := os.WriteFile(filepath.Join(materials, "server.pem"), f.strangerCert, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(materials, "server.key"), f.strangerKey, 0o600); err != nil {
			t.Fatal(err)
		}
		unobserved(t, observe(t, f))
		if f.self.served.Load()+f.follower.served.Load()+f.leader.served.Load() != 0 {
			t.Fatal("a proof request was sent with a certificate the policy does not authorize")
		}
	})
	t.Run("without the policy file", func(t *testing.T) {
		f := newStatusProofFixture(t)
		f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
		if err := os.Remove(filepath.Join(f.root, "gpuworks.db.dqlite", ".transport", "policy.json")); err != nil {
			t.Fatal(err)
		}
		unobserved(t, observe(t, f))
	})
}

func TestStatusDatabaseProofStopsAtItsDeadline(t *testing.T) {
	f := newStatusProofFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.self.answer = func() []byte { <-release; return statusProofRefusal() }
	f.leader.answer = func() []byte { return statusProofBody(f.proof(5)) }
	started := time.Now()
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: f.root}, "", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// The routes and the proof each have one timeout; a held first member
	// uses up the proof's, and the leader is not asked afterwards.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("status outlived its bounds: %s", elapsed)
	}
	if !strings.Contains(text, "Leader: not observed") || f.leader.served.Load() != 0 {
		t.Fatalf("a held member did not use up the proof's deadline\n%s", text)
	}
}
