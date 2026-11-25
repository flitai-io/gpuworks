//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// The seed records the tier once; a member that joins is told it with its
// certificate, keeps it with its join materials and never chooses its own.
func TestRuntimeDqliteStorageTierIsRecordedBySeedAndAdoptedByJoiner(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	info, err := InitializeEnrollmentWithTier(t.Context(), cfg, address, "https://"+address, storagetier.SlowStorage)
	if err != nil || info.StorageTier != "slow-storage" {
		t.Fatalf("initialized info %+v err %v, want the slow-storage tier recorded", info, err)
	}
	config := enrollmentNativeFiles(t, cfg)["config.json"]
	var recorded map[string]any
	if err := json.Unmarshal([]byte(config), &recorded); err != nil || recorded["storage_tier"] != "slow-storage" {
		t.Fatalf("config.json = %s (%v)", config, err)
	}
	if tier, err := RecordedStorageTier(cfg.DataDir); err != nil || tier != storagetier.SlowStorage {
		t.Fatalf("recorded tier %q err %v", tier, err)
	}
	if again, err := InitializeEnrollmentWithTier(t.Context(), cfg, address, "https://"+address, storagetier.SlowStorage); err != nil || again != info {
		t.Fatalf("repeated initialization: %+v %v", again, err)
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err == nil || !strings.Contains(err.Error(), "existing enrollment configuration differs") {
		t.Fatalf("initialization with another tier: %v, want the recorded tier kept", err)
	}
	if enrollmentNativeFiles(t, cfg)["config.json"] != config {
		t.Fatal("a refused initialization rewrote the enrollment configuration")
	}

	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	startEnrollmentNativeApp(t, a)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(enrollmentNativeFiles(t, cfg)["ca.pem"])) {
		t.Fatal("CA unavailable")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	secure := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	status, data := enrollmentNativeRequest(t, &http.Client{Timeout: 5 * time.Second}, "http://"+a.Addr()+"/api/v1/auth/login", "", map[string]string{"username": "admin", "password": cfg.AdminPassword})
	var login struct {
		Token string `json:"token"`
	}
	if status != 200 || json.Unmarshal(data, &login) != nil || login.Token == "" {
		t.Fatalf("login=%d %s", status, data)
	}
	status, data = enrollmentNativeRequest(t, secure, "https://"+a.EnrollmentAddr()+"/api/v1/cluster/invitations", login.Token, map[string]any{})
	var invite dto.ClusterInvitationResponse
	if status != 201 || json.Unmarshal(data, &invite) != nil {
		t.Fatalf("invitation=%d %s", status, data)
	}

	joining := Config{DataDir: t.TempDir()}
	target := ClusterJoinTarget{ClusterID: invite.ClusterID, CAFingerprint: invite.CAFingerprint, Protocol: invite.Protocol, EnrollmentURL: invite.EnrollmentURL, Address: "127.0.0.1:9012"}
	if _, err := ReceiveClusterMaterials(t.Context(), joining, target, invite.Token); err != nil {
		t.Fatalf("receive materials: %v", err)
	}
	var adopted string
	err = withClusterJoinMaterials(t.Context(), joining, target, func(_ string, _ ClusterJoinInfo, _ []byte, snapshot joinMaterialSnapshot) error {
		adopted = snapshot.StorageTier
		if got := joinedStorageTier(snapshot); got != storagetier.SlowStorage {
			t.Errorf("joining member would start its node on %q, want slow-storage", got)
		}
		return nil
	})
	if err != nil || adopted != "slow-storage" {
		t.Fatalf("join materials record tier %q err %v, want the cluster's slow-storage", adopted, err)
	}

	// The join itself tells its process the cluster's tier before it starts
	// the node, whatever the process was told before; reopening the joined
	// identity later leaves the process on the tier it was told.
	previous := cluster.ProcessStorageTier()
	t.Cleanup(func() { cluster.UseStorageTier(previous) })
	cluster.UseStorageTier(storagetier.Standard)
	started, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, startErr := StartClusterRaft(started, joining, target)
	if got := cluster.ProcessStorageTier(); got != storagetier.SlowStorage {
		t.Fatalf("the join left its process on %s (start: %v), want the cluster's slow-storage", got, startErr)
	}
	if session != nil {
		_ = session.Close()
	}
	cluster.UseStorageTier("slow-storage@100ms")
	reopened, reopenErr := startClusterRaft(started, joining, target, false)
	if reopened != nil {
		t.Cleanup(func() { _ = reopened.Close() })
	}
	if got := cluster.ProcessStorageTier(); got != "slow-storage@100ms" {
		t.Fatalf("reopening the joined identity moved the process to %s (start: %v), want the tier it was told", got, reopenErr)
	}
}

// A recorded tier this build does not know stops every reader of the
// enrollment configuration instead of running the cluster on other timing.
func TestRuntimeDqliteUnknownRecordedStorageTierIsRefused(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	if _, err := InitializeEnrollmentWithTier(t.Context(), cfg, address, "https://"+address, storagetier.SlowStorage); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, enrollmentDirectory, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"storage_tier": "glacial"`, `"storage_tier": "standard"`} {
		changed := strings.Replace(string(raw), `"storage_tier": "slow-storage"`, bad, 1)
		if changed == string(raw) {
			t.Fatalf("config.json has no tier to replace: %s", raw)
		}
		if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readEnrollmentBundle(cfg.DataDir, false); err == nil || err.Error() != "unsupported enrollment storage tier" {
			t.Fatalf("%s: bundle read error %v", bad, err)
		}
		if _, _, err := readStatusEnrollment(cfg.DataDir); err == nil {
			t.Fatalf("%s: status read accepted the configuration", bad)
		}
		if a, err := New(cfg, enrollmentNativeLog()); err == nil {
			a.Close()
			t.Fatalf("%s: the server started", bad)
		}
	}
	if _, err := RecordedStorageTier(cfg.DataDir); err != nil {
		t.Fatalf("recorded tier reader refused the explicit standard spelling: %v", err)
	}
}
