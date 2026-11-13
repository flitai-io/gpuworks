package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

func saveRuntimePrivateHosts(t *testing.T, path string, hosts []string) {
	t.Helper()
	encoded, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	// JSON arrays are YAML sequences. Replace the file as an editor's safe
	// save does, keeping other startup-only settings unchanged.
	content := "usage_report: false\nauto_check_updates: false\noutbound_restrict_private_hosts: true\noutbound_allow_private_hosts: " + string(encoded) + "\n"
	temporary := path + ".new"
	if err := os.WriteFile(temporary, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func startOutboundRuntimeApp(t *testing.T, path string) (*App, string) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Addr, cfg.ConfigPath = "127.0.0.1:0", path
	cfg.DataDir = t.TempDir()
	cfg.AdminUser, cfg.AdminPassword = "admin", "gpuworks-test-password-2026"
	cfg.UsageReportOff, cfg.ManualUpdateChecks, cfg.RecoveryEnabled = true, true, false
	// A stale startup value must never authorize a target after the file
	// has removed its permission.
	cfg.OutboundAllowPrivateHosts = []string{"127.0.0.1"}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("outbound runtime app did not stop")
			return
		}
		a.obs.close()
		if err := a.store.Close(); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		a.lnMu.RLock()
		ready := len(a.lns) > 0
		a.lnMu.RUnlock()
		if ready {
			return a, "http://" + a.Addr()
		}
		select {
		case err := <-done:
			t.Fatalf("outbound runtime app stopped before binding: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("outbound runtime app did not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunningAppAppliesPrivateHostFileChangesWithoutRestart(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{
		RepoTags: []string{"example/runtime:v1"}, LayerBytes: 256 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, archiveServer := ocifixture.StartArchiveServer(archive)
	t.Cleanup(archiveServer.Close)
	var archiveHits, tagHits atomic.Int64
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tags" {
			tagHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"next":null,"results":[{"name":"v1.0.0"}]}`)
			return
		}
		archiveHits.Add(1)
		archiveServer.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	// Community upstream_api accepts HTTPS only. Trust this fixture's real
	// certificate before App.New assembles its long-lived HTTP clients.
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	previousTransport := http.DefaultTransport
	transport := previousTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previousTransport
		transport.CloseIdleConnections()
	})
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	saveRuntimePrivateHosts(t, path, []string{})
	a, base := startOutboundRuntimeApp(t, path)
	token := login(t, base)
	// Distinct repository cache keys force actual outbound requests through
	// the same long-lived community browser assembled by App.New.
	var repositories []map[string]string
	for _, name := range []string{"denied", "allowed", "revoked"} {
		repositories = append(repositories, map[string]string{
			"repo": "example/runtime-" + name, "engine": "vllm", "registry": "docker-hub",
			"upstream_api": origin.URL + "/tags",
		})
	}
	catalog, err := json.Marshal(map[string]any{"repos": repositories})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetSetting(store.SettingImageCommunityCatalog, string(catalog)); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		name  string
		hosts []string
		want  int
	}{
		{"denied", []string{}, http.StatusForbidden},
		{"allowed", []string{"127.0.0.1"}, http.StatusOK},
		{"revoked", []string{}, http.StatusForbidden},
	} {
		t.Run(stage.name, func(t *testing.T) {
			saveRuntimePrivateHosts(t, path, stage.hosts)
			beforeArchive, beforeTags := archiveHits.Load(), tagHits.Load()
			code, body := status(t, http.MethodPost, base+"/api/v1/images/preview-archive-url", token,
				[]byte(fmt.Sprintf(`{"url":%q}`, origin.URL+"/image.tar")))
			if code != stage.want {
				t.Fatalf("preview=%d want=%d body=%s", code, stage.want, body)
			}
			if code == http.StatusOK {
				if !strings.Contains(string(body), `"repo_tags":["example/runtime:v1"]`) || archiveHits.Load() <= beforeArchive {
					t.Fatalf("allowed preview did not read the real archive: %s", body)
				}
			} else if archiveHits.Load() != beforeArchive || !strings.Contains(string(body), "IMAGE_ARCHIVE_URL_POLICY_DENIED") {
				t.Fatalf("denied preview reached origin or lost its policy error: %s", body)
			}
			code, body = status(t, http.MethodGet, base+"/api/v1/images/community/browse?repo=example/runtime-"+stage.name, token, nil)
			if stage.want == http.StatusOK {
				if code != http.StatusOK || !strings.Contains(string(body), "v1.0.0") || tagHits.Load() <= beforeTags {
					t.Fatalf("allowed community browse=%d body=%s", code, body)
				}
			} else if code == http.StatusOK || tagHits.Load() != beforeTags || !strings.Contains(string(body), "outbound URL is not permitted") {
				t.Fatalf("revoked or unapproved community browser reached origin: status=%d body=%s", code, body)
			}
		})
	}
}

func TestInvalidRuntimePrivateHostConfigurationDoesNotLeakParserValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	saveRuntimePrivateHosts(t, path, []string{"127.0.0.1"})
	var logs bytes.Buffer
	readHosts := privateHostPermissions(Config{ConfigPath: path}, slog.New(slog.NewTextHandler(&logs, nil)))
	if hosts := readHosts(); len(hosts) != 1 || hosts[0] != "127.0.0.1" {
		t.Fatalf("initial runtime permission=%v", hosts)
	}
	// KnownFields errors can quote an unrecognized YAML key. Such a key may
	// itself be a pasted URL carrying a password or query token.
	secretURL := "https://operator:private-password@source.invalid?token=private-token"
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%q: true\n", secretURL)), 0o600); err != nil {
		t.Fatal(err)
	}
	if hosts := readHosts(); len(hosts) != 0 {
		t.Fatalf("invalid configuration retained private permission: %v", hosts)
	}
	output := logs.String()
	if !strings.Contains(output, "private sources denied") || !strings.Contains(output, "invalid_or_changed") {
		t.Fatalf("invalid configuration has no useful diagnostic: %s", output)
	}
	for _, secret := range []string{secretURL, "private-password", "private-token", "source.invalid"} {
		if strings.Contains(output, secret) {
			t.Fatalf("runtime permission diagnostic leaked YAML content: %s", output)
		}
	}
}

func TestRunningAppDefaultsToPrivateAccessAndHotTogglesStrictMode(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{
		RepoTags: []string{"example/default-private:v1"}, LayerBytes: 256 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, origin := ocifixture.StartArchiveServer(archive)
	t.Cleanup(origin.Close)
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	save := func(policy string) {
		t.Helper()
		content := "usage_report: false\nauto_check_updates: false\n" + policy
		if err := os.WriteFile(path+".new", []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	save("")
	_, base := startOutboundRuntimeApp(t, path)
	token := login(t, base)
	for _, stage := range []struct {
		name, policy string
		want         int
	}{
		{"default", "", 200},
		{"strict", "outbound_restrict_private_hosts: true\n", 403},
		{"strict-granted", "outbound_restrict_private_hosts: true\noutbound_allow_private_hosts: [127.0.0.1]\n", 200},
		{"relaxed", "outbound_restrict_private_hosts: false\n", 200},
		{"strict-revoked", "outbound_restrict_private_hosts: true\n", 403},
	} {
		t.Run(stage.name, func(t *testing.T) {
			save(stage.policy)
			code, body := status(t, http.MethodPost, base+"/api/v1/images/preview-archive-url", token,
				[]byte(fmt.Sprintf(`{"url":%q}`, origin.URL+"/image.tar")))
			if code != stage.want {
				t.Fatalf("preview=%d want=%d body=%s", code, stage.want, body)
			}
			if code == 200 && !strings.Contains(string(body), `"repo_tags":["example/default-private:v1"]`) {
				t.Fatalf("default/authorized preview did not parse real archive: %s", body)
			}
		})
	}
	save("outbound_restrict_private_hosts: false\noutbound_allow_private_hosts: [0.0.0.0/0, '::/0']\n")
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/", "http://100.100.100.200/latest/meta-data/",
		"http://[fe80::1]/image.tar", "http://[fd00:ec2::254]/image.tar",
	} {
		code, body := status(t, http.MethodPost, base+"/api/v1/images/preview-archive-url", token,
			[]byte(fmt.Sprintf(`{"url":%q}`, target)))
		if code != http.StatusForbidden || !strings.Contains(string(body), "IMAGE_ARCHIVE_URL_POLICY_BLOCKED") {
			t.Fatalf("relaxed policy allowed a permanently forbidden target: status=%d body=%s", code, body)
		}
	}
}

func TestInProcessPrivateDefaultsAndExplicitStrictMode(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer origin.Close()
	for _, strict := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.OutboundRestrictPrivateHosts = strict
		transport := outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, privateHostPermissions(cfg, nil))
		client := &http.Client{Transport: transport}
		response, err := client.Get(origin.URL)
		if strict {
			if !errors.Is(err, outbound.ErrTargetDenied) {
				t.Errorf("strict mode did not refuse an unapproved target: %v", err)
			}
		} else if err != nil || response == nil || response.StatusCode != http.StatusNoContent {
			t.Errorf("default mode did not allow a normal local source: response=%v err=%v", response, err)
		}
		if response != nil {
			response.Body.Close()
		}
		transport.CloseIdleConnections()
	}
}
