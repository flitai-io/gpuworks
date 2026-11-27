//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/filelock"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

func enrollmentNativeConfig(t *testing.T) (Config, string) {
	t.Helper()
	t.Setenv("GPUWORKS_MASTER_SECRET", "")
	cfg := Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0", AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg, testport.Address(t)
}
func enrollmentNativeLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func enrollmentNativeFiles(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"config.json", "ca.pem", "ca.key", "server.pem", "server.key", "master.binding"} {
		data, err := os.ReadFile(filepath.Join(cfg.DataDir, enrollmentDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(data)
	}
	return out
}
func TestRuntimeDqliteEnrollmentInitializationPreservesIdentity(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	online, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); !errors.Is(err, filelock.ErrBusy) {
		online.Close()
		t.Fatalf("online init must reject runtime owner: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, enrollmentDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("online init wrote material")
	}
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address)
	if err != nil {
		t.Fatal(err)
	}
	files := enrollmentNativeFiles(t, cfg)
	again, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address)
	if err != nil || again != info {
		t.Fatalf("repeat init changed identity: %+v %v", again, err)
	}
	if !reflect.DeepEqual(files, enrollmentNativeFiles(t, cfg)) {
		t.Fatal("repeat init replaced key material")
	}
	marker := filepath.Join(cfg.DataDir, enrollmentDirectory, ".pending")
	if err := os.WriteFile(marker, []byte("interrupted before completion"), 0600); err != nil {
		t.Fatal(err)
	}
	if a, err := New(cfg, enrollmentNativeLog()); err == nil {
		a.Close()
		t.Fatal("pending initialization started App")
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful retry left pending marker")
	}
	if !reflect.DeepEqual(files, enrollmentNativeFiles(t, cfg)) {
		t.Fatal("pending retry replaced existing CA or member key")
	}
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := a.store.ClusterIdentity(t.Context())
	a.Close()
	if err != nil || stored.ClusterID != info.ClusterID || stored.CAFingerprint != info.CAFingerprint {
		t.Fatalf("persisted identity mismatch: %v", err)
	}
}

func TestRuntimeDqliteEnrollmentMissingMaterialNeverRegenerates(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err != nil {
		t.Fatal(err)
	}
	files := enrollmentNativeFiles(t, cfg)
	key := filepath.Join(cfg.DataDir, enrollmentDirectory, "ca.key")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err == nil {
		t.Fatal("missing signing key regenerated")
	}
	if a, err := New(cfg, enrollmentNativeLog()); err == nil {
		a.Close()
		t.Fatal("incomplete material started App")
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing signing key replaced")
	}
	if err := os.WriteFile(key, []byte(files["ca.key"]), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataDir, enrollmentDirectory)
	retained := filepath.Join(cfg.DataDir, "saved-enrollment")
	if err := os.Rename(dir, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err == nil {
		t.Fatal("existing database identity acquired replacement materials")
	}
	if a, err := New(cfg, enrollmentNativeLog()); err == nil {
		a.Close()
		t.Fatal("missing identity materials started App")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing enrollment directory recreated")
	}
	if err := os.Rename(retained, dir); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatalf("restored originals cannot start: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func startEnrollmentNativeApp(t *testing.T, a *App) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("App Run failed to stop")
			}
		})
	}
	t.Cleanup(stop)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if address := a.Addr(); address != "" && !strings.HasSuffix(address, ":0") {
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get("http://" + address + "/api/v1/healthz")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 401 && a.EnrollmentAddr() != "" {
					return stop
				}
			}
		}
		select {
		case <-timer.C:
			t.Fatal("dual listener App did not become ready")
		case <-tick.C:
		}
	}
}
func enrollmentNativeRequest(t *testing.T, client *http.Client, url, token string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}
func TestRuntimeDqliteEnrollmentActualHTTPSAndRestart(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	info, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address)
	if err != nil {
		t.Fatal(err)
	}
	original := enrollmentNativeFiles(t, cfg)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(original["ca.pem"])) {
		t.Fatal("CA unavailable")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	secure := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	plain := &http.Client{Timeout: 5 * time.Second}
	var jwt string
	var lastGCSequence int64
	for restart := 0; restart < 2; restart++ {
		a, err := New(cfg, enrollmentNativeLog())
		if err != nil {
			t.Fatal(err)
		}
		var gcSequence int64
		if err := a.store.DB().Raw("SELECT sequence FROM catalog_gc_barriers WHERE id = 1").Scan(&gcSequence).Error; err != nil || gcSequence <= lastGCSequence {
			a.Close()
			t.Fatalf("native startup did not confirm a fresh GC barrier: sequence=%d previous=%d err=%v", gcSequence, lastGCSequence, err)
		}
		lastGCSequence = gcSequence
		t.Cleanup(func() { _ = a.Close() })
		stop := startEnrollmentNativeApp(t, a)
		if restart == 0 {
			status, data := enrollmentNativeRequest(t, plain, "http://"+a.Addr()+"/api/v1/auth/login", "", map[string]string{"username": "admin", "password": cfg.AdminPassword})
			if status != 200 {
				t.Fatalf("login=%d %s", status, data)
			}
			var login struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(data, &login); err != nil {
				t.Fatal(err)
			}
			jwt = login.Token
			if jwt == "" {
				t.Fatal("login omitted JWT")
			}
		}
		status, data := enrollmentNativeRequest(t, secure, "https://"+a.EnrollmentAddr()+"/api/v1/cluster/invitations", jwt, map[string]any{})
		if status != 201 {
			t.Fatalf("invitation on restart %d=%d %s", restart, status, data)
		}
		var invite dto.ClusterInvitationResponse
		if err := json.Unmarshal(data, &invite); err != nil {
			t.Fatal(err)
		}
		if invite.ClusterID != info.ClusterID || invite.CAFingerprint != info.CAFingerprint {
			t.Fatal("restart replaced public identity")
		}
		memberAddress := []string{"127.0.0.1:9010", "127.0.0.1:9011"}[restart]
		joining := Config{DataDir: t.TempDir()}
		target := ClusterJoinTarget{ClusterID: invite.ClusterID, CAFingerprint: invite.CAFingerprint, Protocol: invite.Protocol, EnrollmentURL: invite.EnrollmentURL, Address: memberAddress}
		prepared, err := PrepareClusterJoin(t.Context(), joining, target)
		if err != nil {
			t.Fatal(err)
		}
		key, err := identity.LoadMemberKey(filepath.Join(joining.DataDir, "gpuworks.db.join", "member.key"))
		if err != nil {
			t.Fatal(err)
		}
		client, err := enrollment.NewClient(invite.EnrollmentURL, invite.ClusterID, invite.CAFingerprint, invite.Protocol)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		if received, err := ReceiveClusterMaterials(t.Context(), joining, target, invite.Token); err != nil || received != prepared {
			t.Fatalf("invitation-backed material receipt failed: %v", err)
		}
		certificate, err := client.Recover(t.Context(), key, memberAddress)
		if err != nil {
			t.Fatal(err)
		}
		if certificate.KeyFingerprint != prepared.KeyFingerprint {
			t.Fatal("exchange changed prepared member identity")
		}
		materials, err := client.FetchSecrets(t.Context(), []byte(certificate.CertificatePEM), key)
		if err != nil {
			t.Fatal(err)
		}
		expectedMaster, err := enrollmentMasterSecret(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		if materials.CACertificatePEM != original["ca.pem"] || materials.CAKeyPEM != original["ca.key"] || string(materials.MasterSecret) != string(expectedMaster) {
			t.Fatal("material response differs from persisted cluster identity")
		}
		if received, err := ReceiveClusterMaterials(t.Context(), joining, target, ""); err != nil || received != prepared {
			t.Fatalf("material receipt changed join identity: %v", err)
		}
		rawMaterials, err := readJoinMaterialFile(filepath.Join(joining.DataDir, "gpuworks.db.join", "materials", "bundle.json"), 2<<20)
		if err != nil {
			t.Fatal(err)
		}
		var persisted joinMaterialSnapshot
		if err := json.Unmarshal(rawMaterials, &persisted); err != nil || !reflect.DeepEqual(persisted.Secrets, materials) {
			t.Fatal("persisted material differs from authenticated source")
		}
		index, err := client.FetchCatalogIndex(t.Context(), []byte(certificate.CertificatePEM), key)
		if err != nil || len(index.Digests) == 0 {
			t.Fatalf("native App must export its seeded catalog: %v", err)
		}
		for range 2 {
			if err := ReceiveClusterCatalog(t.Context(), joining, target); err != nil {
				t.Fatalf("catalog receive/retry: %v", err)
			}
		}
		cache, err := packages.New(filepath.Join(joining.DataDir, "gpuworks.db.join", "catalog-packages"))
		if err != nil {
			t.Fatal(err)
		}
		for _, digest := range index.Digests {
			if _, err := cache.Load(digest); err != nil {
				t.Fatalf("received reference %s missing or corrupt: %v", digest, err)
			}
		}
		if unexpected, err := New(joining, enrollmentNativeLog()); err == nil {
			unexpected.Close()
			t.Fatal("catalog copy incorrectly released join startup guard")
		}
		if again, err := PrepareClusterJoin(t.Context(), joining, target); err != nil || again != prepared {
			t.Fatalf("retry after invitation consumption changed identity: %v", err)
		}
		if recovered, err := client.Recover(t.Context(), key, memberAddress); err != nil || recovered.KeyFingerprint != prepared.KeyFingerprint {
			t.Fatalf("prepared identity cannot recover its admission: %v", err)
		}
		pair, err := tls.X509KeyPair([]byte(certificate.CertificatePEM), key)
		if err != nil {
			t.Fatal(err)
		}
		trust, err := identity.NewTrust([]byte(original["ca.pem"]), info.ClusterID, info.CAFingerprint, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if fp, err := trust.VerifyMember(pair.Leaf, time.Now()); err != nil || fp != certificate.KeyFingerprint {
			t.Fatalf("issued certificate verification: %v", err)
		}
		stop()
		if err := ReceiveClusterCatalog(t.Context(), joining, target); err == nil {
			t.Fatal("offline cache reuse bypassed current source authorization")
		}
		for _, digest := range index.Digests {
			if _, err := cache.Load(digest); err != nil {
				t.Fatal("failed retry damaged received package", err)
			}
		}
		if received, err := ReceiveClusterMaterials(t.Context(), joining, target, ""); err != nil || received != prepared {
			t.Fatalf("offline retry of committed materials failed: %v", err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(original, enrollmentNativeFiles(t, cfg)) {
		t.Fatal("App restart changed persisted private material")
	}
}

func TestRuntimeDqliteEnrollmentBindFailureReleasesMainListener(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg.Addr = testport.Address(t)
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// The synchronous bind error must occur before the listener is served or
	// any Run-owned background loops are started.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected immediate enrollment bind error, got %v", err)
	}
	rebound, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		t.Fatalf("main listener leaked after enrollment bind failure: %v", err)
	}
	rebound.Close()
	if a.EnrollmentAddr() != address {
		t.Fatalf("unexpected enrollment listener state: %q", a.EnrollmentAddr())
	}
}

func TestRuntimeDqliteEnrollmentMasterSecretBinding(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err != nil {
		t.Fatal(err)
	}
	original := enrollmentNativeFiles(t, cfg)
	path := filepath.Join(cfg.DataDir, "master.key")
	secret, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing file", "changed file", "changed environment"} {
		t.Run(scenario, func(t *testing.T) {
			switch scenario {
			case "missing file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "changed file":
				if err := os.WriteFile(path, []byte("wrong-master-secret"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed environment":
				t.Setenv("GPUWORKS_MASTER_SECRET", "wrong-environment-secret")
			}
			if a, err := New(cfg, enrollmentNativeLog()); err == nil {
				a.Close()
				t.Fatal("App accepted lost or changed master secret")
			}
			if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err == nil {
				t.Fatal("init accepted changed binding")
			}
			if !reflect.DeepEqual(original, enrollmentNativeFiles(t, cfg)) {
				t.Fatal("rejection changed cluster private material")
			}
			if scenario == "missing file" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("App generated replacement master key")
				}
			}
			if err := os.WriteFile(path, secret, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDqliteEnrollmentMissingDatabaseIdentityRequiresPending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "completed bundle"
		if pending {
			name = "pending candidate"
		}
		t.Run(name, func(t *testing.T) {
			cfg, address := enrollmentNativeConfig(t)
			info, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address)
			if err != nil {
				t.Fatal(err)
			}
			before := enrollmentNativeFiles(t, cfg)
			a, err := New(cfg, enrollmentNativeLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := a.store.DB().Exec("DELETE FROM cluster_identities").Error; err != nil {
				a.Close()
				t.Fatal(err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(cfg.DataDir, enrollmentDirectory, ".pending")
			if pending {
				if err := os.WriteFile(marker, []byte("interrupted before database identity commit"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			restored, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address)
			if pending {
				if err != nil || restored != info {
					t.Fatalf("candidate failed to resume same identity: %+v %v", restored, err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("resumed candidate left pending marker")
				}
				reopened, err := New(cfg, enrollmentNativeLog())
				if err != nil {
					t.Fatal(err)
				}
				identity, err := reopened.store.ClusterIdentity(t.Context())
				reopened.Close()
				if err != nil || identity.ClusterID != info.ClusterID || identity.CAFingerprint != info.CAFingerprint {
					t.Fatalf("candidate restored wrong DB identity: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("completed bundle silently recreated missing database identity")
				}
				if reopened, err := New(cfg, enrollmentNativeLog()); err == nil {
					reopened.Close()
					t.Fatal("incoherent completed bundle started App")
				}
			}
			if !reflect.DeepEqual(before, enrollmentNativeFiles(t, cfg)) {
				t.Fatal("identity recovery replaced original key material")
			}
		})
	}
}

func TestRuntimeDqliteEnrollmentEmptyNativeDirectoryRejected(t *testing.T) {
	cfg := Config{DataDir: t.TempDir()}
	dir := filepath.Join(cfg.DataDir, "gpuworks.db.dqlite")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "master.key"), []byte("existing-master"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeEnrollment(t.Context(), cfg, "127.0.0.1:19010", "https://127.0.0.1:19010"); err == nil {
		t.Fatal("empty native directory initialized a replacement database")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty source acquired native state: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, enrollmentDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid source generated cluster authority")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "gpuworks.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid source generated SQLite database")
	}
}

func TestRuntimeDqliteEnrollmentLoadedBundleKeepsVerifiedSecret(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	if _, err := InitializeEnrollment(t.Context(), cfg, address, "https://"+address); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, "master.key")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := enrollmentAppSecret(cfg, bundle); err != nil || got != string(original) {
		t.Fatal("loaded bundle re-read missing key")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("loaded bundle generated replacement key")
	}
	if err := os.WriteFile(path, []byte("replacement-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_MASTER_SECRET", "replacement-environment")
	if got, err := enrollmentAppSecret(cfg, bundle); err != nil || got != string(original) {
		t.Fatal("loaded bundle accepted file or environment replacement")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "replacement-secret" {
		t.Fatal("secret accessor rewrote material")
	}
}
