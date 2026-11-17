package selfinstall

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// channelMirror is one distribution source: it serves a signed channel index
// and the archives it lists, the way an object store's public endpoint does.
type channelMirror struct {
	*httptest.Server
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
}

func newChannelMirror(t *testing.T) *channelMirror {
	t.Helper()
	m := &channelMirror{objects: map[string][]byte{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		m.mu.Lock()
		m.requests = append(m.requests, key)
		body, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		http.ServeContent(w, r, key, time.Unix(0, 0), strings.NewReader(string(body)))
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *channelMirror) put(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = body
}

func (m *channelMirror) asked(prefix string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range m.requests {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

var channelKeyPublic, channelKeyPrivate, _ = ed25519.GenerateKey(rand.Reader)

// channelRelease is what a test publishes: one archive for the running
// platform, signed with key under keyID.
type channelRelease struct {
	releaseID string
	binary    []byte
	keyID     string
	key       ed25519.PrivateKey
	// archive overrides the bytes served for the archive (the index still
	// describes the real ones).
	archive []byte
	// platform overrides the os/arch the index lists.
	platform string
	// channel is where it is published; empty is nightly.
	channel string
	// builtAt overrides the release's build time.
	builtAt  string
	previous []distribution.ServerRelease
}

// publishChannel writes the release onto every mirror and returns the archive
// path inside the tree.
func publishChannel(t *testing.T, release channelRelease, mirrors ...*channelMirror) string {
	t.Helper()
	goos, goarch := currentPlatform()
	if release.platform != "" {
		goos, goarch, _ = strings.Cut(release.platform, "/")
	}
	archive := tarGz(t, map[string]string{binaryName: string(release.binary)})
	key := fmt.Sprintf("releases/%s/gpuworks-%s-%s-%s.tar.gz", release.releaseID, release.releaseID, goos, goarch)
	channel := release.channel
	if channel == "" {
		channel = "nightly"
	}
	builtAt := release.builtAt
	if builtAt == "" {
		builtAt = "2026-10-11T00:00:00Z"
	}
	index := distribution.Index{
		PreviousServers: release.previous, Schema: distribution.IndexSchema, Channel: channel, PublishedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
		Server: distribution.ServerRelease{
			ReleaseID: release.releaseID, Commit: "c0ffee12", BuiltAt: builtAt,
			Files: []distribution.ReleaseFile{{OS: goos, Arch: goarch, File: distribution.File{Path: key, Size: int64(len(archive)), SHA256: sha256Hex(archive)}}},
		},
	}
	payload, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	keyID, signer := release.keyID, release.key
	if keyID == "" {
		keyID, signer = "test", channelKeyPrivate
	}
	envelope, err := distribution.Seal(payload, keyID, signer)
	if err != nil {
		t.Fatal(err)
	}
	served := archive
	if release.archive != nil {
		served = release.archive
	}
	for _, m := range mirrors {
		m.put(channel+"/index.json", envelope)
		m.put(key, served)
	}
	return key
}

// useChannel points the installer at mirrors, trusting the test key.
func (h *harness) useChannel(mirrors ...*channelMirror) {
	urls := make([]string, 0, len(mirrors))
	for _, m := range mirrors {
		urls = append(urls, m.URL)
	}
	h.deps.Channel = func(sources []string) (*distribution.Client, error) {
		if len(sources) > 0 {
			urls = sources
		}
		return &distribution.Client{
			Sources: urls, Keys: distribution.Keys{"test": channelKeyPublic},
			IndexTimeout: 2 * time.Second, StallTimeout: time.Second,
		}, nil
	}
	h.installer = New(h.layout, h.deps, h.io)
}

func channelReq() UpgradeRequest {
	return UpgradeRequest{Channel: "nightly", Keep: DefaultBackupKeep}
}

func TestUpgradeFromTheChannel(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	first, second := newChannelMirror(t), newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("new binary")}, first, second)
	h.useChannel(first, second)

	report, err := h.installer.Upgrade(context.Background(), channelReq())
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if report.From != "v1.0.0" || report.To != "v2.0.0" || report.Commit != "c0ffee12" || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	if report.Source != first.URL && report.Source != second.URL {
		t.Fatalf("report.Source = %q", report.Source)
	}
	if out := h.out.String(); !strings.Contains(out, "downloading gpuworks-v2.0.0-") || !strings.Contains(out, "from "+report.Source) || !strings.Contains(out, "100% of") {
		t.Fatalf("progress narration = %q", out)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("service was not cycled: %v", h.run.executed())
	}
}

// The source that answers the index first may still lack the archive (a
// mirror caught halfway through a sync); the download moves on.
func TestUpgradeFromTheChannelFallsBackToAnotherSource(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	partial, complete := newChannelMirror(t), newChannelMirror(t)
	key := publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("new binary")}, partial, complete)
	partial.mu.Lock()
	delete(partial.objects, key)
	partial.mu.Unlock()
	h.useChannel(partial, complete)

	report, err := h.installer.Upgrade(context.Background(), channelReq())
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if report.Source == complete.URL {
		// The complete mirror won the index race; nothing was left to fall back from.
		return
	}
	if !strings.Contains(h.out.String(), "switched to "+complete.URL) {
		t.Fatalf("the switch was not narrated: %q", h.out.String())
	}
}

func TestUpgradeFromTheChannelRefusesAnUntrustedIndex(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("evil"), keyID: "test", key: stranger}, mirror)
	h.useChannel(mirror)

	_, err := h.installer.Upgrade(context.Background(), channelReq())
	if err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("an untrusted index must not stop the service: %v", h.run.executed())
	}
}

func TestUpgradeFromTheChannelRefusesACorruptArchive(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("new binary"), archive: []byte("not the archive the index describes")}, mirror)
	h.useChannel(mirror)

	_, err := h.installer.Upgrade(context.Background(), channelReq())
	if err == nil || !strings.Contains(err.Error(), "the index lists") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("a failed download must not stop the service: %v", h.run.executed())
	}
}

func TestPinnedCLIUpgradeInstallsHistoricalBinary(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	m := newChannelMirror(t)
	old := channelRelease{releaseID: "v2.0.0", binary: []byte("selected binary")}
	publishChannel(t, old, m)
	data, _, err := distribution.Open(m.objects["nightly/index.json"], distribution.Keys{"test": channelKeyPublic})
	if err != nil {
		t.Fatal(err)
	}
	previous, err := distribution.ParseIndex(data, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	publishChannel(t, channelRelease{releaseID: "v3.0.0", binary: []byte("latest binary"), previous: previous.Servers()}, m)
	h.useChannel(m)
	h.stagedReports("gpuworks v2.0.0 (cafe1234, 2026-10-08T00:00:00Z)\n")
	req := channelReq()
	req.Version = "v2.0.0"
	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil || report.To != req.Version || readFile(t, h.layout.Binary()) != "selected binary" {
		t.Fatalf("pin: %+v %v", report, err)
	}
}

func TestUpgradeFromTheChannelChecksWithoutDownloading(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("new binary")}, mirror)
	h.useChannel(mirror)
	req := channelReq()
	req.Check = true

	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil || !report.Checked || report.To != "v2.0.0" || report.Source != mirror.URL {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if mirror.asked("releases/") {
		t.Fatal("--check must not download the archive")
	}
}

func TestUpgradeFromTheChannelRefuses(t *testing.T) {
	cases := map[string]struct {
		release channelRelease
		req     func(UpgradeRequest) UpgradeRequest
		want    string
	}{
		"pin it does not offer": {
			release: channelRelease{releaseID: "v2.0.0", binary: []byte("b")},
			req:     func(r UpgradeRequest) UpgradeRequest { r.Version = "v3.0.0"; return r },
			want:    "is not what the nightly channel offers: it is at v2.0.0",
		},
		"no build for this platform": {
			release: channelRelease{releaseID: "v2.0.0", binary: []byte("b"), platform: "plan9/mips"},
			req:     func(r UpgradeRequest) UpgradeRequest { return r },
			want:    "has no ",
		},
		"bad channel name": {
			release: channelRelease{releaseID: "v2.0.0", binary: []byte("b")},
			req:     func(r UpgradeRequest) UpgradeRequest { r.Channel = "Not A Channel"; return r },
			want:    "is not one of nightly, release",
		},
		"channel nobody publishes": {
			release: channelRelease{releaseID: "v2.0.0", binary: []byte("b")},
			req:     func(r UpgradeRequest) UpgradeRequest { r.Channel = "release"; return r },
			want:    "404",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			mirror := newChannelMirror(t)
			publishChannel(t, tc.release, mirror)
			h.useChannel(mirror)
			_, err := h.installer.Upgrade(context.Background(), tc.req(channelReq()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if h.run.ran("sudo systemctl stop gpuworks") {
				t.Fatalf("a refused upgrade must not stop the service: %v", h.run.executed())
			}
		})
	}
}

func TestChannelClientDefaults(t *testing.T) {
	client, err := Deps{}.channelClient(nil)
	if err != nil || len(client.Sources) != 0 || client.Keys != nil {
		t.Fatalf("production client = %+v, %v (want built-in sources and keys)", client, err)
	}
	client, err = Deps{}.channelClient([]string{"https://mirror.example/dist/", "https://mirror.example/dist"})
	if err != nil || strings.Join(client.Sources, ",") != "https://mirror.example/dist" {
		t.Fatalf("client = %+v, %v", client, err)
	}
	if _, err := (Deps{}).channelClient([]string{"mirror.example"}); err == nil {
		t.Fatal("a source without a scheme must be refused")
	}
}

func TestProgressReport(t *testing.T) {
	var out strings.Builder
	io := &IO{Out: &out, Err: &out}
	report := newProgressReport(io, "https://a")
	for _, done := range []int64{10, 30, 50, 60, 100} {
		report.update(distribution.Progress{Source: "https://a", Done: done, Total: 100})
	}
	report.update(distribution.Progress{Source: "https://b", Done: 100, Total: 100})
	want := "  25% of 100 B\n  50% of 100 B\n  100% of 100 B\n  switched to https://b\n"
	if out.String() != want {
		t.Fatalf("narration = %q, want %q", out.String(), want)
	}
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KiB", 40559400: "38.7 MiB", 5 << 30: "5.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// Without --channel the installation's update_channel decides, so the console
// setting and the command agree.
func TestUpgradeFollowsTheConfiguredChannel(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	cfg := h.config()
	cfg.UpdateChannel = "release"
	h.writeConfig(cfg)
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "nightly-build", binary: []byte("nightly")}, mirror)
	publishChannel(t, channelRelease{releaseID: "v1.1.0", binary: []byte("release binary"), channel: "release"}, mirror)
	h.useChannel(mirror)

	report, err := h.installer.Upgrade(context.Background(), UpgradeRequest{Keep: DefaultBackupKeep})
	if err != nil || report.To != "v1.1.0" {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "release binary" {
		t.Fatalf("binary = %q", got)
	}
}

// Switching a nightly build to the release channel must not install an older
// release unless asked to.
func TestUpgradeDoesNotGoBackToAnOlderRelease(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("abc1234")
	h.run.mu.Lock()
	h.run.output[h.layout.Binary()+" version"] = "gpuworks abc1234 (abc1234, 2026-12-01T00:00:00Z)\n"
	h.run.mu.Unlock()
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v1.1.0", binary: []byte("older release"), channel: "release", builtAt: "2026-11-01T00:00:00Z"}, mirror)
	h.useChannel(mirror)

	report, err := h.installer.Upgrade(context.Background(), UpgradeRequest{Channel: "release", Keep: DefaultBackupKeep})
	if err != nil || !report.UpToDate || !report.AheadOfChannel {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("an older release was installed: %q", got)
	}
	report, err = h.installer.Upgrade(context.Background(), UpgradeRequest{Channel: "release", Keep: DefaultBackupKeep, Force: true})
	if err != nil || report.To != "v1.1.0" || report.AheadOfChannel {
		t.Fatalf("forced report = %+v, %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "older release" {
		t.Fatalf("--force did not install the release: %q", got)
	}
}
