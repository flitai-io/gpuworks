package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommunityBrowseUsesStoredRegistryLogin(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "reader" || p != "fixture-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"tags":["v1.0.0"]}`)
	}))
	defer server.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	transport.Proxy = nil
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	application, _ := newTestApp(t, "gpuworks-test-password-2026")
	t.Cleanup(func() { _ = application.Store().Close() })
	host := strings.TrimPrefix(server.URL, "https://")
	browser := outsideSources(application.Store(), func() []string { return []string{"127.0.0.1"} }).Community
	if _, err := browser.Browse(context.Background(), host+"/team/runner", nil); err == nil {
		t.Fatal("private repository allowed anonymous browse")
	}
	credential, err := application.Store().PutSourceCredential(0, host, "reader", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	row, err := browser.Browse(context.Background(), host+"/team/runner", nil)
	if err != nil || len(row.Tags) != 1 {
		t.Fatalf("stored login browse: error=%v tags=%d", err, len(row.Tags))
	}
	if err := application.Store().DeleteSourceCredential(credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.Browse(context.Background(), host+"/team/runner", nil); err == nil {
		t.Fatal("deleted credential reused cached private tags")
	}
}
