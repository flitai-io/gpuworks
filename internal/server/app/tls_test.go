package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func tlsTestFiles(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gpuworks test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.3"), net.ParseIP("8.8.8.8")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return certPath, keyPath, pool
}

func TestNativeTLSProtectsPublicListenersAndPreservesLocalMaintenance(t *testing.T) {
	cert, key, roots := tlsTestFiles(t)
	port := freePortFor(t, "127.0.0.2")
	app, err := New(Config{Listen: serverconfig.Listen{Console: []string{"127.0.0.2:" + port}, Gateway: []string{"127.0.0.3:" + port}}, DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026", TLSCertFile: cert, TLSKeyFile: key}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.proxy.Close(); app.obs.Close(); _ = app.store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("TLS server did not stop")
		}
	})
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("https://127.0.0.2:" + port + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("HTTPS health status %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTPS listener unavailable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.Post("https://127.0.0.2:"+port+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTPS login status %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 1 || !resp.Cookies()[0].Secure {
		t.Fatal("HTTPS login did not set Secure cookie")
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatal("HTTPS did not set HSTS")
	}
	resp, err = client.Get("https://127.0.0.3:" + port + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("TLS lost listener function boundary: %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatal("listener-level rejection omitted security headers")
	}
	resp, err = client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("local maintenance status %d", resp.StatusCode)
	}
	origin, _, err := expose.ResolveOrigin(expose.NewStore(app.cfg.DataDir), "")
	if err != nil || origin != "http://127.0.0.1:"+port {
		t.Fatalf("connector did not select the HTTP maintenance listener: origin=%s error=%v", origin, err)
	}
	resp, err = client.Get("http://127.0.0.2:" + port + "/api/v1/auth/me")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("public plaintext accepted: %d", resp.StatusCode)
		}
	}
}

func TestNativeTLSRejectsLegacyAddrWithoutMaintenanceBinding(t *testing.T) {
	cert, key, _ := tlsTestFiles(t)
	cfg := Config{Addr: "127.0.0.2:8800", TLSCertFile: cert, TLSKeyFile: key}
	if _, err := cfg.listenerTLS(); err == nil || !strings.Contains(err.Error(), "listen.console") {
		t.Fatalf("native TLS accepted a legacy address without a maintenance socket: %v", err)
	}
	// The legacy plain-HTTP address remains available to existing embedders.
	cfg.TLSCertFile, cfg.TLSKeyFile = "", ""
	if tlsConfig, err := cfg.listenerTLS(); err != nil || tlsConfig != nil {
		t.Fatalf("legacy HTTP configuration changed: %v", err)
	}
}

func TestNativeTLSFailsBeforeBindingOnInvalidConfiguration(t *testing.T) {
	for _, cfg := range []Config{{Addr: ":8800", TLSCertFile: "missing", TLSKeyFile: "missing"}, {Addr: "127.0.0.2:0", TLSCertFile: "missing", TLSKeyFile: "missing"}, {Addr: "127.0.0.2:0", TLSCertFile: "missing"}, {Addr: "127.0.0.1:0", TLSCertFile: "missing", TLSKeyFile: "missing"}} {
		if _, err := cfg.listenerTLS(); err == nil {
			t.Fatalf("invalid native TLS configuration accepted: %+v", cfg)
		}
	}
}

func TestNativeTLSListenUpdateCannotSaveAnUnstartableWildcard(t *testing.T) {
	cert, key, _ := tlsTestFiles(t)
	configPath := filepath.Join(t.TempDir(), serverconfig.FileName)
	before := []byte("listen:\n  console: [127.0.0.2:8800]\n")
	if err := os.WriteFile(configPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newServerSettings(Config{ConfigPath: configPath, TLSCertFile: cert, TLSKeyFile: key}, nil, nil)
	if _, err := s.UpdateListen(dto.ServerListenUpdate{Console: []string{"0.0.0.0:8800"}}, "127.0.0.1:8800"); err == nil {
		t.Fatal("TLS listener update accepted wildcard")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected TLS config was saved: %s %v", after, err)
	}
}
