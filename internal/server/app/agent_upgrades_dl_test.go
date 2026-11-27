package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentUpgradeDownloadPinOverridesMutableBinary(t *testing.T) {
	dir := t.TempDir()
	name := "gpuworks-agent-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("live-new-build"), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned := []byte("frozen-target-build")
	handler := withDL(http.StripPrefix("/dl/", http.FileServer(http.Dir(dir))), http.NotFoundHandler(), http.NotFoundHandler(), func(got string) ([]byte, bool, error) {
		if got != name {
			return nil, false, nil
		}
		return pinned, true, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/dl/"+name, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != string(pinned) || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("pin=%d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/dl/"+name, nil)
	req.Header.Set("Range", "bytes=0-5")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 206 || rec.Body.String() != "frozen" {
		t.Fatalf("range=%d %s", rec.Code, rec.Body)
	}
}

func TestAgentUpgradeDownloadPinConflictDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	name := "gpuworks-agent-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("wrong-new-target"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := withDL(http.StripPrefix("/dl/", http.FileServer(http.Dir(dir))), http.NotFoundHandler(), http.NotFoundHandler(), func(string) ([]byte, bool, error) {
		return nil, true, errors.New("private artifact path must not leak")
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/"+name, nil))
	if rec.Code != 409 || strings.Contains(rec.Body.String(), "wrong-new-target") || strings.Contains(rec.Body.String(), "private artifact") {
		t.Fatalf("conflict=%d %s", rec.Code, rec.Body)
	}
}

func TestAgentUpgradeDownloadWithoutPinUsesExistingHandler(t *testing.T) {
	dir := t.TempDir()
	name := "gpuworks-agent-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("ordinary-build"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := withDL(http.StripPrefix("/dl/", http.FileServer(http.Dir(dir))), http.NotFoundHandler(), http.NotFoundHandler(), func(string) ([]byte, bool, error) { return nil, false, nil })
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/"+name, nil))
	if rec.Code != 200 || rec.Body.String() != "ordinary-build" {
		t.Fatalf("fallback=%d %s", rec.Code, rec.Body)
	}
}
