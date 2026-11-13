package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/version"
)

// upgradingServer stands in for the app's server settings: it records the
// archive a console upgrade hands it.
type upgradingServer struct {
	StaticServerSettings
	blocker    string
	installDir string
	err        error
	archives   []string
	by         []string
}

func (s *upgradingServer) SelfUpgradeBlocker() string { return s.blocker }
func (s *upgradingServer) InstallDir() string         { return s.installDir }

func (s *upgradingServer) Upgrade(_ context.Context, archive io.Reader, by string) (dto.ServerUpgradeStarted, error) {
	data, err := io.ReadAll(archive)
	if err != nil {
		return dto.ServerUpgradeStarted{}, err
	}
	s.archives = append(s.archives, string(data))
	s.by = append(s.by, by)
	if s.err != nil {
		return dto.ServerUpgradeStarted{}, s.err
	}
	return dto.ServerUpgradeStarted{From: version.Version, To: "rel-0001", Backup: "/opt/gpuworks/server/backups/x.tar.gz"}, nil
}

func newUpgradeRun(t *testing.T, server *upgradingServer, channel *testChannel) *imageManifestRun {
	t.Helper()
	run := newImageManifestRun(t)
	run.deps.Server = server
	if channel != nil {
		run.deps.Outside = channel.sources()
	}
	run.router = Router(run.deps)
	return run
}

func TestUpgradeServerFromTheChannel(t *testing.T) {
	server := &upgradingServer{}
	channel := newTestChannel(t)
	run := newUpgradeRun(t, server, channel)

	rec := run.do(http.MethodPost, "/api/v1/updates/server", "", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"from":"`+version.Version+`","to":"rel-0001","backup":"/opt/gpuworks/server/backups/x.tar.gz"}`+"\n" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// The archive handed over is the channel's file for this platform,
	// checked against the index on the way.
	if len(server.archives) != 1 || server.archives[0] != "archive rel-0001" {
		t.Fatalf("archives = %q", server.archives)
	}
	if len(server.by) != 1 || server.by[0] == "" {
		t.Fatalf("by = %q, want the administrator's name", server.by)
	}
	asked := strings.Join(channel.requested(), ",")
	if !strings.Contains(asked, "releases/rel-0001/gpuworks-rel-0001-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz") {
		t.Fatalf("requested = %s", asked)
	}
}

func TestUpgradeServerFromTheChannelRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		server    *upgradingServer
		prepare   func(*testChannel)
		buildDate string
		status    int
		code      string
	}{
		{name: "no service manager", server: &upgradingServer{blocker: "no_service_manager"}, status: http.StatusConflict, code: ErrorServerUpgradeUnavailable},
		{name: "already current", server: &upgradingServer{}, status: http.StatusConflict, code: ErrorServerAlreadyAtRelease,
			prepare: func(c *testChannel) { c.publishServer(version.Version) }},
		{name: "the channel offers an older release", server: &upgradingServer{}, status: http.StatusConflict, code: ErrorServerNewerThanRelease,
			buildDate: "2026-12-01T00:00:00Z"},
		{name: "no build for this platform", server: &upgradingServer{}, status: http.StatusNotFound, code: ErrorUpdateNotPublished,
			prepare: func(c *testChannel) {
				c.index.Server.Files[0].OS = "plan9"
				c.sign()
			}},
		{name: "archive altered on the source", server: &upgradingServer{}, status: http.StatusBadGateway, code: ErrorUpdateDownloadFailed,
			prepare: func(c *testChannel) {
				c.replace(c.index.Server.Files[0].Path, []byte("archive rel-0001, but tampered"))
			}},
		{name: "no source answers", server: &upgradingServer{}, status: http.StatusBadGateway, code: ErrorUpdateSourcesUnavailable,
			prepare: func(c *testChannel) { c.fail("nightly/index.json", http.StatusServiceUnavailable) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := newTestChannel(t)
			if tc.prepare != nil {
				tc.prepare(channel)
			}
			if tc.buildDate != "" {
				saved := version.BuildDate
				version.BuildDate = tc.buildDate
				t.Cleanup(func() { version.BuildDate = saved })
			}
			run := newUpgradeRun(t, tc.server, channel)
			rec := run.do(http.MethodPost, "/api/v1/updates/server", "", "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if len(tc.server.archives) != 0 {
				t.Fatalf("a refused upgrade reached the server: %q", tc.server.archives)
			}
		})
	}
}

func TestUpgradeServerFromAnUpload(t *testing.T) {
	server := &upgradingServer{}
	run := newUpgradeRun(t, server, nil)
	rec := run.do(http.MethodPost, "/api/v1/updates/server/upload", "application/gzip", "uploaded archive bytes")
	if rec.Code != http.StatusAccepted || len(server.archives) != 1 || server.archives[0] != "uploaded archive bytes" {
		t.Fatalf("upload = %d %s, archives %q", rec.Code, rec.Body.String(), server.archives)
	}

	server.err = &ServerSettingsError{Code: ErrorServerUpgradeArchiveInvalid, Fallback: "not an archive", Params: map[string]any{"detail": "gzip: invalid header"}}
	rec = run.do(http.MethodPost, "/api/v1/updates/server/upload", "application/gzip", "junk")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorServerUpgradeArchiveInvalid) || !strings.Contains(rec.Body.String(), "gzip: invalid header") {
		t.Fatalf("invalid archive = %d %s", rec.Code, rec.Body.String())
	}
	server.err = errors.New("disk full")
	if rec = run.do(http.MethodPost, "/api/v1/updates/server/upload", "application/gzip", "x"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failure = %d %s", rec.Code, rec.Body.String())
	}

	blocked := &upgradingServer{blocker: "not_writable"}
	run = newUpgradeRun(t, blocked, nil)
	rec = run.do(http.MethodPost, "/api/v1/updates/server/upload", "application/gzip", "x")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"reason":"not_writable"`) || len(blocked.archives) != 0 {
		t.Fatalf("blocked upload = %d %s", rec.Code, rec.Body.String())
	}
}

func TestUpdatesSaysWhetherTheServerCanUpgradeItself(t *testing.T) {
	channel := newTestChannel(t)
	run := newUpgradeRun(t, &upgradingServer{}, channel)
	if s := getUpdateStatus(t, run).Server; !s.SelfUpgrade || s.SelfUpgradeBlocker != "" {
		t.Fatalf("server = %+v", s)
	}
	run = newUpgradeRun(t, &upgradingServer{blocker: "no_service_manager", installDir: "/opt/gpuworks/server"}, channel)
	var raw struct {
		Server map[string]any `json:"server"`
	}
	rec := run.do(http.MethodGet, "/api/v1/updates", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	// The installation directory comes along, so the page can offer the
	// command that puts the server under a supervisor.
	if raw.Server["self_upgrade"] != false || raw.Server["self_upgrade_blocker"] != "no_service_manager" ||
		raw.Server["install_dir"] != "/opt/gpuworks/server" {
		t.Fatalf("server = %v", raw.Server)
	}
}

func TestServerUpgradeRoutesRequireAnAdministrator(t *testing.T) {
	run := newUpgradeRun(t, &upgradingServer{}, newTestChannel(t))
	_, user := roleTestToken(t, run.store, "user")
	for _, path := range []string{"/api/v1/updates/server", "/api/v1/updates/server/upload"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+user)
		rec := httptest.NewRecorder()
		run.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("user POST %s = %d", path, rec.Code)
		}
	}
}
