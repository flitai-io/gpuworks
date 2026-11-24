package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/version"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

var channelTestPublic, channelTestPrivate, _ = ed25519.GenerateKey(rand.Reader)

// testChannel is one distribution source serving a signed nightly index and
// the files it lists, the way an object store's public endpoint does; faults
// reproduce an unreachable, slow or altered source.
type testChannel struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	objects  map[string][]byte
	statuses map[string]int
	delays   map[string]time.Duration
	log      []string
	queries  []string
	index    distribution.Index

	imagePath    string
	templatePath string
	// indexTimeout replaces the client's index timeout when set.
	indexTimeout time.Duration
}

func newTestChannel(t *testing.T) *testChannel {
	t.Helper()
	c := &testChannel{t: t, objects: map[string][]byte{}, statuses: map[string]int{}, delays: map[string]time.Duration{}}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	c.index = distribution.Index{Schema: distribution.IndexSchema, Channel: "nightly", PublishedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)}
	c.publishServer("rel-0001")
	return c
}

func (c *testChannel) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	c.mu.Lock()
	c.log = append(c.log, key)
	c.queries = append(c.queries, r.URL.RawQuery)
	body, ok := c.objects[key]
	status, delay := c.statuses[key], c.delays[key]
	c.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	http.ServeContent(w, r, key, time.Unix(0, 0), strings.NewReader(string(body)))
}

// sources is what a server reading this channel is configured with. The
// timeouts are generous so a loaded test machine does not fail a transfer;
// a test about slowness sets indexTimeout.
func (c *testChannel) sources() OutsideSources {
	timeout := 5 * time.Second
	if c.indexTimeout > 0 {
		timeout = c.indexTimeout
	}
	return OutsideSources{
		Channel: &distribution.Client{
			Sources: []string{c.server.URL}, Keys: distribution.Keys{"test": channelTestPublic},
			IndexTimeout: timeout, StallTimeout: 5 * time.Second,
		},
	}
}

func (c *testChannel) put(key string, data []byte) distribution.File {
	sum := sha256.Sum256(data)
	c.mu.Lock()
	c.objects[key] = data
	c.mu.Unlock()
	return distribution.File{Path: key, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

// sign rewrites the index the channel serves.
func (c *testChannel) sign() {
	c.t.Helper()
	payload, err := json.Marshal(c.index)
	if err != nil {
		c.t.Fatal(err)
	}
	envelope, err := distribution.Seal(payload, "test", channelTestPrivate)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.objects[c.index.Channel+"/index.json"] = envelope
	c.mu.Unlock()
}

func (c *testChannel) publishServer(releaseID string) {
	file := c.put("releases/"+releaseID+"/gpuworks-"+releaseID+"-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz", []byte("archive "+releaseID))
	c.index.Server = distribution.ServerRelease{ReleaseID: releaseID, Commit: "c0ffee", BuiltAt: "2026-10-11T00:00:00Z",
		Files: []distribution.ReleaseFile{{OS: runtime.GOOS, Arch: runtime.GOARCH, File: file}}}
	c.sign()
}

// publishImageCatalog lists data as the channel's image catalog, named by its
// catalog_version when it has one; nil withdraws the catalog.
func (c *testChannel) publishImageCatalog(data []byte) {
	if data == nil {
		c.index.ImageCatalog = nil
		c.sign()
		return
	}
	var probe struct {
		CatalogVersion string `json:"catalog_version"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.CatalogVersion == "" {
		probe.CatalogVersion = "unnamed"
	}
	sum := sha256.Sum256(data)
	c.imagePath = "catalog/images/" + hex.EncodeToString(sum[:]) + ".json"
	c.index.ImageCatalog = &distribution.CatalogFile{Version: probe.CatalogVersion, File: c.put(c.imagePath, data)}
	c.sign()
}

func (c *testChannel) publishTemplateCatalog(archive []byte, digest string, schemaVersion int) {
	pkg, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(archive))
	if err != nil {
		c.t.Fatal(err)
	}
	c.publishTemplateCatalogAs(archive, digest, schemaVersion, inferencecatalog.CatalogVersion(pkg.Package.Manifest.PublishedAt, digest))
}

// publishTemplateCatalogAs publishes archive under an explicit version, for a
// package this server cannot read and so cannot name.
func (c *testChannel) publishTemplateCatalogAs(archive []byte, digest string, schemaVersion int, version string) {
	sum := sha256.Sum256(archive)
	c.templatePath = "catalog/templates/" + hex.EncodeToString(sum[:]) + ".tar.gz"
	c.index.TemplateCatalog = &distribution.TemplateCatalog{
		CatalogFile:   distribution.CatalogFile{Version: version, File: c.put(c.templatePath, archive)},
		PackageDigest: digest, SchemaVersion: schemaVersion,
	}
	c.sign()
}

// replace serves other bytes at key without touching the index, as an
// altered or half-synced mirror does.
func (c *testChannel) replace(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[key] = data
}

func (c *testChannel) fail(key string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses[key] = status
}

func (c *testChannel) delay(key string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays[key] = d
}

// queryLog is the query string of every request, in order.
func (c *testChannel) queryLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.queries...)
}

func (c *testChannel) requested() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func getUpdateStatus(t *testing.T, run *imageManifestRun) dto.UpdateStatus {
	t.Helper()
	rec := run.do(http.MethodGet, "/api/v1/updates", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates = %d %s", rec.Code, rec.Body.String())
	}
	var status dto.UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestUpdatesComparesTheChannelWithThisServer(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	channel.publishImageCatalog([]byte(upstreamManifest))
	run.useChannel(channel)
	if err := run.store.RecordImageCatalogImport("2026-10-08.11111111", nil); err != nil {
		t.Fatal(err)
	}

	status := getUpdateStatus(t, run)
	if !status.Available || status.Channel != "nightly" || status.Source != channel.server.URL {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Sources) != 1 || !status.Sources[0].OK || status.Sources[0].URL != channel.server.URL || status.Sources[0].Error != "" {
		t.Fatalf("sources = %+v", status.Sources)
	}
	if s := status.Server; s.Current != version.Version || s.Latest != "rel-0001" || !s.UpdateAvailable || !s.Installable || s.BuiltAt != "2026-10-11T00:00:00Z" {
		t.Fatalf("server = %+v", s)
	}
	if c := status.ImageCatalog; c.Current != "2026-10-08.11111111" || c.Latest != "2026-10-09.abcdef12" || !c.UpdateAvailable || !c.Compatible {
		t.Fatalf("image catalog = %+v", c)
	}
	// No template catalog published, none applied: nothing to offer.
	if c := status.TemplateCatalog; c.Latest != "" || c.UpdateAvailable {
		t.Fatalf("template catalog = %+v", c)
	}

	// After "check for updates" the image catalog is current.
	if rec := run.do(http.MethodPost, "/api/v1/images/sync", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	if c := getUpdateStatus(t, run).ImageCatalog; c.Current != "2026-10-09.abcdef12" || c.UpdateAvailable {
		t.Fatalf("image catalog after sync = %+v", c)
	}
}

// The catalogs a channel publishes come from its release's build. A server
// built after that release (a nightly switched to release, a build ahead of
// the channel) is not offered them as updates, though they are shown.
func TestCatalogsFromAnOlderReleaseAreNotUpdates(t *testing.T) {
	saved := version.BuildDate
	t.Cleanup(func() { version.BuildDate = saved })
	run := newImageManifestRun(t)
	channel := newTestChannel(t) // its release is built 2026-10-11T00:00:00Z
	channel.publishImageCatalog([]byte(upstreamManifest))
	run.useChannel(channel)

	version.BuildDate = "2026-10-11T08:00:00Z"
	status := getUpdateStatus(t, run)
	if c := status.ImageCatalog; c.Latest != "2026-10-09.abcdef12" || c.UpdateAvailable {
		t.Fatalf("image catalog from an older release = %+v", c)
	}
	if status.Server.UpdateAvailable {
		t.Fatalf("server = %+v", status.Server)
	}

	version.BuildDate = "2026-10-10T08:00:00Z"
	rec := run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	var newer dto.UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &newer); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	if c := newer.ImageCatalog; !c.UpdateAvailable {
		t.Fatalf("image catalog from a newer release = %+v", c)
	}
}

// A channel nobody serves is a status, not an error: the view shows each
// source's reason and what this server runs.
func TestUpdatesWhenNoSourceAnswers(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	channel.fail("nightly/index.json", http.StatusForbidden)
	run.useChannel(channel)
	status := getUpdateStatus(t, run)
	if status.Available || status.Source != "" || status.Server.Current != version.Version || status.Server.Latest != "" {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Sources) != 1 || status.Sources[0].OK || !strings.Contains(status.Sources[0].Error, "403") {
		t.Fatalf("sources = %+v", status.Sources)
	}
	if status.UnavailableReason != "unreachable" {
		t.Fatalf("a denied source is reported as %q, want unreachable", status.UnavailableReason)
	}
}

// A source that serves something that is not the signed index (a captive
// portal page) is not mistaken for a network problem or an empty channel.
func TestUpdatesWhenTheIndexDoesNotVerify(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	channel.replace("nightly/index.json", []byte("<html>sign in to the network</html>"))
	run.useChannel(channel)
	if status := getUpdateStatus(t, run); status.Available || status.UnavailableReason != "invalid" {
		t.Fatalf("status = %+v", status)
	}
}

// The wire shape the settings page reads, pinned literally.
func TestUpdateStatusWire(t *testing.T) {
	wire, err := json.Marshal(dto.UpdateStatus{
		Channel: "nightly", CheckedAt: time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC), Available: true, Source: "https://a",
		Sources:         []dto.UpdateSourceStatus{{URL: "https://a", OK: true, LatencyMS: 12}, {URL: "https://b", Error: "GET: 404"}},
		Server:          dto.ServerUpdate{CurrentBuild: dto.ServerBuildInfo{Version: "r1", Kind: "development", BuildChannel: "nightly"}, Current: "r1", Latest: "r2", BuiltAt: "t", UpdateAvailable: true, Installable: true, SelfUpgradeBlocker: "no_service_manager", InstallDir: "/opt/gpuworks/server"},
		ImageCatalog:    dto.CatalogUpdate{Current: "i1", Latest: "i2", UpdateAvailable: true, Compatible: true},
		TemplateCatalog: dto.CatalogUpdate{Current: "t1", Compatible: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"channel":"nightly","channel_editable":false,"auto_check":false,"checked_at":"2026-10-11T01:02:03Z","available":true,"source":"https://a",` +
		`"sources":[{"url":"https://a","ok":true,"latency_ms":12},{"url":"https://b","ok":false,"latency_ms":0,"error":"GET: 404"}],` +
		`"server":{"current_build":{"version":"r1","build_channel":"nightly","kind":"development"},"current":"r1","latest":"r2","built_at":"t","update_available":true,"installable":true,"self_upgrade":false,"self_upgrade_blocker":"no_service_manager","install_dir":"/opt/gpuworks/server"},` +
		`"image_catalog":{"current":"i1","latest":"i2","update_available":true,"compatible":true},` +
		`"template_catalog":{"current":"t1","update_available":false,"compatible":true}}`
	if string(wire) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", wire, want)
	}
}

// An unavailable channel names its reason on the wire; an available one
// leaves the key out.
func TestUpdateStatusUnavailableWire(t *testing.T) {
	wire, err := json.Marshal(dto.UpdateStatus{Channel: "release", CheckedAt: time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC),
		UnavailableReason: "unpublished", Sources: []dto.UpdateSourceStatus{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"available":false,"unavailable_reason":"unpublished","sources":[]`) {
		t.Fatalf("wire = %s", wire)
	}
	available, _ := json.Marshal(dto.UpdateStatus{Available: true, Sources: []dto.UpdateSourceStatus{}})
	if strings.Contains(string(available), "unavailable_reason") {
		t.Fatalf("an available status names a reason: %s", available)
	}
}

func TestUpdatesRequiresAnAdministrator(t *testing.T) {
	run := newImageManifestRun(t)
	_, user := roleTestToken(t, run.store, "user")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil)
	req.Header.Set("Authorization", "Bearer "+user)
	rec := httptest.NewRecorder()
	run.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user GET /updates = %d", rec.Code)
	}
	anonymous := httptest.NewRecorder()
	run.router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /updates = %d", anonymous.Code)
	}
}

// switchableServer keeps an update channel in memory, as server.yaml would.
type switchableServer struct {
	StaticServerSettings
	channel string
	saves   []string
}

func (s *switchableServer) UpdateChannel() (string, bool) { return s.channel, true }

func (s *switchableServer) SetUpdateChannel(channel string) error {
	if channel != "nightly" && channel != "release" {
		return &ServerSettingsError{Code: ErrorUpdateChannelInvalid, Fallback: "unknown channel", Params: map[string]any{"channel": channel}}
	}
	s.channel = channel
	s.saves = append(s.saves, channel)
	return nil
}

func countRequests(channel *testChannel, key string) int {
	n := 0
	for _, asked := range channel.requested() {
		if asked == key {
			n++
		}
	}
	return n
}

// Opening the console checks for updates; the sources are asked once an hour
// for a channel, and a manual check asks again.
func TestUpdatesReuseTheLastCheck(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	run.useChannel(channel)
	first := getUpdateStatus(t, run)
	second := getUpdateStatus(t, run)
	if got := countRequests(channel, "nightly/index.json"); got != 1 {
		t.Fatalf("two opens asked the sources %d times, want once", got)
	}
	if !second.CheckedAt.Equal(first.CheckedAt) {
		t.Fatalf("the reused check reports another time: %s vs %s", second.CheckedAt, first.CheckedAt)
	}
	rec := run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d", rec.Code)
	}
	if got := countRequests(channel, "nightly/index.json"); got != 2 {
		t.Fatalf("a manual check asked the sources %d times in all, want 2", got)
	}
}

// The sign-in page starts the check without being signed in. It learns
// nothing, never waits for the sources, and however many pages open, one
// probe runs; the administrator who then signs in is served that probe.
func TestPrefetchStartsOneCheckForTheSignInPage(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	channel.delay("nightly/index.json", 300*time.Millisecond)
	run.useChannel(channel)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			rec := httptest.NewRecorder()
			run.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/updates/prefetch", nil))
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Errorf("anonymous prefetch = %d %q", rec.Code, rec.Body.String())
			}
			if waited := time.Since(start); waited > 250*time.Millisecond {
				t.Errorf("prefetch waited %s for the sources", waited)
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for countRequests(channel, "nightly/index.json") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	status := getUpdateStatus(t, run)
	if status.Server.Latest != "rel-0001" {
		t.Fatalf("status after the prefetch = %+v", status)
	}
	if got := countRequests(channel, "nightly/index.json"); got != 1 {
		t.Fatalf("20 sign-in pages and a sign-in asked the sources %d times, want once", got)
	}
	// While the check is fresh another sign-in page asks nobody.
	rec := httptest.NewRecorder()
	run.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/updates/prefetch", nil))
	time.Sleep(50 * time.Millisecond)
	if got := countRequests(channel, "nightly/index.json"); rec.Code != http.StatusNoContent || got != 1 {
		t.Fatalf("a prefetch of a fresh check = %d and %d requests, want 204 and 1", rec.Code, got)
	}
}

func TestUpdateChannelSwitch(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	server := &switchableServer{channel: "nightly"}
	run.deps.Server = server
	run.useChannel(channel)
	if status := getUpdateStatus(t, run); status.Channel != "nightly" || !status.ChannelEditable || !status.Available {
		t.Fatalf("status = %+v", status)
	}
	rec := run.do(http.MethodPut, "/api/v1/updates/channel", "application/json", `{"channel":"release"}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"channel":"release"}` {
		t.Fatalf("switch = %d %s", rec.Code, rec.Body.String())
	}
	// Nothing is published on release yet: the view says so for that channel.
	status := getUpdateStatus(t, run)
	if status.Channel != "release" || status.Available || status.UnavailableReason != "unpublished" || countRequests(channel, "release/index.json") != 1 {
		t.Fatalf("status after the switch = %+v, requests %v", status, channel.requested())
	}
	for _, body := range []string{`{"channel":"preview"}`, `not json`} {
		rec := run.do(http.MethodPut, "/api/v1/updates/channel", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorUpdateChannelInvalid) {
			t.Fatalf("PUT %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	// Without server.yaml there is nowhere to keep the choice.
	run.deps.Server = StaticServerSettings{}
	run.router = Router(run.deps)
	rec = run.do(http.MethodPut, "/api/v1/updates/channel", "application/json", `{"channel":"release"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorUpdateChannelReadOnly) {
		t.Fatalf("read-only switch = %d %s", rec.Code, rec.Body.String())
	}
}
