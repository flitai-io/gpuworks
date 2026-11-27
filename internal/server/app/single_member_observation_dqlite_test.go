//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

// A server that opened the cluster's database as a single member answers
// the questions every member is asked: the quorum facts, the discovery of
// the members, the upgrade status and its part of a diagnostics collection.
// A step that changes the cluster is refused there with a code of its own.
// A cluster of one member stays in this state across restarts.
func TestASingleMemberAnswersTheMemberQuestions(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	// One extension module with one migration: the status must count it,
	// as an established member's does.
	cfg.Extensions = Extensions{Modules: []Module{observedExtension{}}}
	info, err := InitializeEnrollmentWithTier(t.Context(), cfg, address, "https://"+address, storagetier.SlowStorage)
	if err != nil {
		t.Fatal(err)
	}
	// As an installation does: the database exists, then it is given the
	// member's address and certificate, then the server starts.
	if err := os.Chmod(cfg.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	created, err := store.OpenRuntimeWith(t.Context(), filepath.Join(cfg.DataDir, "gpuworks.db"), bus.New(), store.OpenOptions{Backend: store.ReplicatedBackend(), Extensions: MigrationSets(cfg.Extensions)})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	database := testport.Address(t)
	if _, err := ActivateDatabaseAddress(t.Context(), cfg, database); err != nil {
		t.Fatal(err)
	}
	files := enrollmentNativeFiles(t, cfg)
	cert, key := []byte(files["server.pem"]), []byte(files["server.key"])
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	for _, start := range []string{"first start", "after a restart"} {
		a, err := New(cfg, enrollmentNativeLog())
		if err != nil {
			t.Fatalf("%s: %v", start, err)
		}
		stop := startEnrollmentNativeApp(t, a)
		client, err := enrollment.NewClient("https://"+a.EnrollmentAddr()+enrollmentCertificatePath, info.ClusterID, info.CAFingerprint, info.Protocol)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := client.FetchObservedQuorumFacts(ctx, cert, key)
		if err != nil || facts.ClusterID != info.ClusterID || facts.LeaderID == 0 || len(facts.Members) != 1 {
			t.Fatalf("%s: quorum facts: %+v, %v; want this cluster with its one member leading", start, facts, err)
		}
		self := facts.Members[0]
		discovery, _, err := client.FetchClusterQuorumDiscovery(ctx, cert, key, []string{self.KeyFingerprint})
		if err != nil || len(discovery.Members) != 1 || discovery.Members[0].ID != self.ID {
			t.Fatalf("%s: quorum discovery: %+v, %v; want the one member", start, discovery, err)
		}
		status, err := client.ClusterUpgrade(ctx, cert, key, enrollment.ClusterUpgradeStatus)
		if err != nil || !status.Leader || status.Refused != "" || status.Error != "" || status.Known == 0 || status.Applied != status.Known {
			t.Fatalf("%s: upgrade status: %+v, %v; want the schema this member runs and no refusal", start, status, err)
		}
		if status.KnownExtensions != 1 || status.AppliedExtensions != 1 || status.AppliedByExtension["observed"] != 1 {
			t.Fatalf("%s: upgrade status counts %d known and %d applied extension migrations (%v), want the module's one", start, status.KnownExtensions, status.AppliedExtensions, status.AppliedByExtension)
		}
		// The agent connections are this server's own hub's, known and
		// counted, not a member's closed business that holds none.
		if status.AgentConnections == nil || *status.AgentConnections != a.hub.SessionCount() {
			t.Fatalf("%s: upgrade status reports agent connections %v, the hub holds %d", start, status.AgentConnections, a.hub.SessionCount())
		}
		// With an agent connected the status counts it, and stops
		// counting it when the agent leaves: the number orders an upgrade.
		disconnect := connectObservedAgent(t, ctx, a, start)
		if counted, err := client.ClusterUpgrade(ctx, cert, key, enrollment.ClusterUpgradeStatus); err != nil || counted.AgentConnections == nil || *counted.AgentConnections != 1 {
			t.Fatalf("%s: upgrade status with one agent connected: %+v, %v; want agent_connections 1", start, counted, err)
		}
		disconnect()
		if counted, err := client.ClusterUpgrade(ctx, cert, key, enrollment.ClusterUpgradeStatus); err != nil || counted.AgentConnections == nil || *counted.AgentConnections != 0 {
			t.Fatalf("%s: upgrade status after the agent left: %+v, %v; want agent_connections 0", start, counted, err)
		}
		refusedWithoutAnAdmittedKey(t, a.EnrollmentAddr(), cfg, files)
		// The prepared routes still answer behind the observation routes.
		if policy, err := client.FetchTransportPolicy(ctx, cert, key); err != nil || len(policy.AllowedKeys) == 0 {
			t.Fatalf("%s: transport policy behind the observation routes: %+v, %v", start, policy, err)
		}
		now := time.Now()
		part, err := client.OpenClusterDiagnostics(ctx, cert, key, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatalf("%s: diagnostics part: %v", start, err)
		}
		// The part names this member (an identity that could not be read
		// is ID 0 with no address) and carries its status report.
		files := partArchive(t, part.Body)
		if err := part.Body.Close(); err != nil {
			t.Fatal(err)
		}
		var named diagnostics.MemberFacts
		if err := json.Unmarshal([]byte(files[diagnostics.MemberFactsFile]), &named); err != nil || named.ID == 0 || named.Address != database {
			t.Fatalf("%s: diagnostics part names member %d at %q (%v), want this member at %s; files %d", start, named.ID, named.Address, err, database, len(files))
		}
		if !strings.Contains(files[diagnostics.MemberStatusFile], database) {
			t.Fatalf("%s: the part's status report does not show this member:\n%s", start, files[diagnostics.MemberStatusFile])
		}
		// A collection given up after it began frees the member's part for
		// the next one: the asker is not left with "busy".
		if abandoned, err := client.OpenClusterDiagnostics(ctx, cert, key, now.Add(-time.Hour), now); err != nil {
			t.Fatalf("%s: second diagnostics part: %v", start, err)
		} else if err := abandoned.Body.Close(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			next, err := client.OpenClusterDiagnostics(ctx, cert, key, now.Add(-time.Hour), now)
			if err == nil {
				_, _ = io.Copy(io.Discard, next.Body)
				_ = next.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the part abandoned by its asker was not freed within 15 s: %v", start, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		// Every step that changes the cluster is refused, with the reason
		// that says what to do and the status unchanged.
		for _, action := range []string{enrollment.ClusterUpgradeBegin, enrollment.ClusterUpgradeMigrate, enrollment.ClusterUpgradeFinish, enrollment.ClusterUpgradeAbort, enrollment.ClusterUpgradeLead} {
			answer, err := client.ClusterUpgrade(ctx, cert, key, action)
			if err != nil || answer.Error != "" ||
				!strings.Contains(answer.Refused, "this member runs as a single member") || !strings.Contains(answer.Refused, "restart it once") {
				t.Fatalf("%s: %s: %+v, %v; want the single member's refusal", start, action, answer, err)
			}
			if answer.Phase != status.Phase || answer.PhaseRevision != status.PhaseRevision || answer.Applied != status.Applied {
				t.Fatalf("%s: %s changed the cluster: phase %q revision %d applied %d, was %q %d %d", start, action, answer.Phase, answer.PhaseRevision, answer.Applied, status.Phase, status.PhaseRevision, status.Applied)
			}
		}
		client.Close()
		stop()
		if err := a.Close(); err != nil {
			t.Fatalf("%s: close: %v", start, err)
		}
	}
}

// observedExtension is a module with one migration of its own.
type observedExtension struct{}

func (observedExtension) Name() string { return "observed" }
func (observedExtension) Migrations() store.MigrationSet {
	return store.MigrationSet{Namespace: "observed", TablePrefix: "ee_observed_", Migrations: []store.Migration{{Version: 1, Name: "create_notes", Apply: func(tx *gorm.DB) error {
		return tx.Exec("CREATE TABLE ee_observed_notes (id INTEGER PRIMARY KEY)").Error
	}}}}
}

// refusedWithoutAnAdmittedKey asks each observation route without a member
// certificate and with a certificate of the cluster's authority for a key
// the cluster does not admit; neither is answered.
func refusedWithoutAnAdmittedKey(t *testing.T, listen string, cfg Config, files map[string]string) {
	t.Helper()
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
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
	now := time.Now().UTC()
	certificate, err := bundle.issuer.IssueMember(csr, fingerprint, "127.0.0.1:9", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	unadmitted, err := tls.X509KeyPair(append(append([]byte{}, certificate...), []byte(files["ca.pem"])...), key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(files["ca.pem"])) {
		t.Fatal("CA unavailable")
	}
	for name, certificates := range map[string][]tls.Certificate{"no certificate": nil, "a key the cluster does not admit": {unadmitted}} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, Certificates: certificates}}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		for path, body := range map[string]string{
			enrollment.ClusterQuorumFactsPath:     `{"nonce":"` + strings.Repeat("a", 64) + `"}`,
			enrollment.ClusterQuorumDiscoveryPath: `{"nonce":"` + strings.Repeat("b", 64) + `"}`,
			enrollment.ClusterUpgradePath:         `{"action":"status"}`,
			enrollment.ClusterDiagnosticsPath:     `{}`,
		} {
			response, err := client.Post("https://"+listen+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("%s, %s: %v", name, path, err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Errorf("%s, %s: HTTP %d, want 403", name, path, response.StatusCode)
			}
		}
		transport.CloseIdleConnections()
	}
}

// connectObservedAgent connects a real agent connection to the server's own
// hub and returns once the hub holds its session; the returned function
// ends the connection and returns once the hub has let the session go.
func connectObservedAgent(t *testing.T, ctx context.Context, a *App, start string) func() {
	t.Helper()
	_, token, err := a.store.CreateNodePending("observed " + start)
	if err != nil {
		t.Fatal(err)
	}
	agentCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- agentconn.New(agentconn.Credentials{ServerURL: "http://" + a.Addr(), NodeToken: token}, nil, nil, gpu.NoAccelerator{}, enrollmentNativeLog()).Run(agentCtx)
	}()
	ended := false
	sessions := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for a.hub.SessionCount() != want {
			select {
			case err := <-done:
				if ended {
					continue
				}
				t.Fatalf("%s: the agent connection ended while the hub held %d sessions, want %d: %v", start, a.hub.SessionCount(), want, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the hub holds %d agent sessions, want %d", start, a.hub.SessionCount(), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	sessions(1)
	return func() {
		t.Helper()
		cancel()
		<-done
		ended = true
		sessions(0)
	}
}
