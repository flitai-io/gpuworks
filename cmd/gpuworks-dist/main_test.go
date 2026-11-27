package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

var testPublic, testPrivate, _ = ed25519.GenerateKey(rand.Reader)

// distDir writes fake release archives the way the two CI build jobs leave
// them merged in one dist/ directory.
func distDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("archive "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Each build job also leaves its own release.json; assemble must not read it.
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"release_id":"stale"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func realInput(t *testing.T, dist string) assembleInput {
	t.Helper()
	archive, digest, info, err := seed.Published()
	if err != nil {
		t.Fatal(err)
	}
	return assembleInput{
		Channel: "nightly", DistDir: dist, OutDir: filepath.Join(t.TempDir(), "tree"),
		Commit: "c0ffee", BuiltAt: "2026-10-11T00:00:00Z", PublishedAt: time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC),
		KeyID: "test", Key: testPrivate,
		ImageCatalog: imagecatalog.SeedJSON(), TemplateArchive: archive, TemplateDigest: digest, TemplateInfo: info,
	}
}

func readTree(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAssembleWritesASignedTree(t *testing.T) {
	dist := distDir(t,
		"gpuworks-abc123-linux-amd64.tar.gz", "gpuworks-abc123-linux-arm64.tar.gz",
		"gpuworks-abc123-darwin-arm64.tar.gz", "SHA256SUMS", "notes.txt")
	in := realInput(t, dist)
	index, err := assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	if index.Server.ReleaseID != "abc123" || len(index.Server.Files) != 3 || index.Server.Commit != "c0ffee" {
		t.Fatalf("server = %+v", index.Server)
	}
	asset, err := index.Server.Asset("linux", "arm64")
	if err != nil || asset.Path != "releases/abc123/gpuworks-abc123-linux-arm64.tar.gz" {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	if got := string(readTree(t, in.OutDir, asset.Path)); got != "archive gpuworks-abc123-linux-arm64.tar.gz" {
		t.Fatalf("archive copy = %q", got)
	}
	sums := string(readTree(t, in.OutDir, "releases/abc123/SHA256SUMS"))
	if !strings.Contains(sums, asset.SHA256+"  gpuworks-abc123-linux-arm64.tar.gz\n") || strings.Count(sums, "\n") != 3 {
		t.Fatalf("SHA256SUMS = %q", sums)
	}

	// Online readers select the signed descriptor for their supported format.
	legacy := readTree(t, in.OutDir, index.ImageCatalog.Path)
	m, err := manifest.Parse(legacy)
	if err != nil || m.SchemaVersion != 3 || m.CatalogVersion != index.ImageCatalog.Version || !bytes.Equal(legacy, readTree(t, in.OutDir, "nightly/manifest.json")) {
		t.Fatalf("legacy catalog %s: %v", m.CatalogVersion, err)
	}
	if !bytes.Equal(readTree(t, in.OutDir, index.CurrentImageCatalog().Path), imagecatalog.SeedJSON()) {
		t.Fatal("current catalog differs from seed")
	}
	if _, err := os.Stat(filepath.Join(in.OutDir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("assemble wrote a root manifest.json (stat: %v); each channel has its own", err)
	}
	if !bytes.Equal(readTree(t, in.OutDir, index.TemplateCatalog.Path), in.TemplateArchive) {
		t.Fatal("template catalog differs from the embedded package")
	}
	if tc := index.TemplateCatalog; tc.PackageDigest != in.TemplateDigest || tc.SchemaVersion != in.TemplateInfo.SchemaVersion ||
		!strings.HasSuffix(tc.Version, "."+in.TemplateDigest[:8]) {
		t.Fatalf("template catalog entry = %+v", tc)
	}

	envelope := readTree(t, in.OutDir, "nightly/index.json")
	payload, keyID, err := distribution.Open(envelope, distribution.Keys{"test": testPublic})
	if err != nil || keyID != "test" {
		t.Fatalf("Open = %v, %q", err, keyID)
	}
	parsed, err := distribution.ParseIndex(payload, "nightly")
	if err != nil || parsed.Server.ReleaseID != "abc123" {
		t.Fatalf("ParseIndex = %+v, %v", parsed, err)
	}
	if got := string(readTree(t, in.OutDir, "nightly/latest")); got != "abc123\n" {
		t.Fatalf("latest = %q", got)
	}
}

func TestAssembleRefuses(t *testing.T) {
	cases := map[string]struct {
		archives []string
		mutate   func(*assembleInput)
		want     string
	}{
		"mixed releases":  {archives: []string{"gpuworks-a1-linux-amd64.tar.gz", "gpuworks-b2-linux-arm64.tar.gz"}, want: "is release"},
		"no archives":     {archives: []string{"README"}, want: "holds no"},
		"pinned elsewise": {archives: []string{"gpuworks-a1-linux-amd64.tar.gz"}, mutate: func(in *assembleInput) { in.ReleaseID = "zz9" }, want: "not zz9"},
		"bad channel":     {archives: []string{"gpuworks-a1-linux-amd64.tar.gz"}, mutate: func(in *assembleInput) { in.Channel = "Beta" }, want: "not a channel name"},
		"no out dir":      {archives: []string{"gpuworks-a1-linux-amd64.tar.gz"}, mutate: func(in *assembleInput) { in.OutDir = "" }, want: "-out is required"},
		"existing out":    {archives: []string{"gpuworks-a1-linux-amd64.tar.gz"}, mutate: func(in *assembleInput) { in.OutDir = t.TempDir() }, want: "already exists"},
		"bad image catalog": {archives: []string{"gpuworks-a1-linux-amd64.tar.gz"},
			mutate: func(in *assembleInput) { in.ImageCatalog = []byte(`{"schema_version":99}`) }, want: "image catalog"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := realInput(t, distDir(t, tc.archives...))
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			if _, err := assemble(in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// serveTree publishes an assembled tree the way an object store's public
// endpoint does (GET, HEAD and ranges via http.FileServer).
func serveTree(t *testing.T, root string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)
	return server
}

func TestVerifyReadsTheTreeBackFromEachSource(t *testing.T) {
	in := realInput(t, distDir(t, "gpuworks-abc123-linux-amd64.tar.gz", "gpuworks-abc123-linux-arm64.tar.gz"))
	if _, err := assemble(in); err != nil {
		t.Fatal(err)
	}
	good := serveTree(t, in.OutDir)

	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.CopyFS(broken, os.DirFS(in.OutDir)); err != nil {
		t.Fatal(err)
	}
	// A mirror whose upload of one archive went wrong but kept the length.
	target := filepath.Join(broken, "releases", "abc123", "gpuworks-abc123-linux-amd64.tar.gz")
	data, _ := os.ReadFile(target)
	data[0] ^= 0xff
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	bad := serveTree(t, broken)
	keys := distribution.Keys{"test": testPublic}

	var out bytes.Buffer
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL}, ReleaseID: "abc123", Full: true, Keys: keys}, &out); err != nil {
		t.Fatalf("verify good: %v\n%s", err, out.String())
	}
	out.Reset()
	// A size check cannot see a flipped byte; the full check can.
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL, bad.URL}, Keys: keys}, &out); err != nil {
		t.Fatalf("size-only verify: %v\n%s", err, out.String())
	}
	out.Reset()
	err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL, bad.URL}, Full: true, Keys: keys}, &out)
	if err == nil || !strings.Contains(out.String(), "ok   "+good.URL) || !strings.Contains(out.String(), "FAIL "+bad.URL) {
		t.Fatalf("full verify = %v\n%s", err, out.String())
	}

	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL}, ReleaseID: "other", Keys: keys}, &out); err == nil || !strings.Contains(out.String(), "offers release abc123, want other") {
		t.Fatalf("release id check = %v\n%s", err, out.String())
	}
	out.Reset()
	if err := os.Remove(filepath.Join(in.OutDir, "nightly", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(in.OutDir, "releases", "abc123", "gpuworks-abc123-linux-arm64.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL}, Keys: keys}, &out); err == nil || !strings.Contains(out.String(), "404") {
		t.Fatalf("missing archive = %v\n%s", err, out.String())
	}
	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "release", Sources: []string{good.URL}, Keys: keys}, &out); err == nil || !strings.Contains(out.String(), "release/index.json") {
		t.Fatalf("missing channel = %v\n%s", err, out.String())
	}
}

func TestPrunable(t *testing.T) {
	at := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	listing := []lsjsonEntry{
		{Path: "r1/SHA256SUMS", ModTime: at(1)},
		{Path: "r1/gpuworks-r1-linux-amd64.tar.gz", ModTime: at(1)},
		{Path: "r2/SHA256SUMS", ModTime: at(2)},
		{Path: "r3/SHA256SUMS", ModTime: at(3)},
		{Path: "r4/SHA256SUMS", ModTime: at(4)},
		{Path: "stray-file", ModTime: at(9)},
	}
	if got := strings.Join(prunable(listing, 2, nil), ","); got != "r1,r2" {
		t.Fatalf("prunable keep 2 = %s", got)
	}
	// A release that has not moved for months is kept regardless of age.
	if got := strings.Join(prunable(listing, 2, []string{"r1"}), ","); got != "r2" {
		t.Fatalf("prunable with r1 protected = %s", got)
	}
	if got := prunable(listing, 10, nil); len(got) != 0 {
		t.Fatalf("nothing to prune when under the limit, got %v", got)
	}
}

// Version tags are kept forever and do not count against the nightly quota;
// only the nightly builds beyond it are deleted.
func TestPrunableKeepsEveryReleaseVersion(t *testing.T) {
	at := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	listing := []lsjsonEntry{
		{Path: "v26.9.0/SHA256SUMS", ModTime: at(1)},
		{Path: "a1b2c3d4e/SHA256SUMS", ModTime: at(2)},
		{Path: "v26.10.3/SHA256SUMS", ModTime: at(3)},
		{Path: "v26.11.0-rc.1/SHA256SUMS", ModTime: at(4)},
		{Path: "v26.11.1-3-g0bcb9eb/SHA256SUMS", ModTime: at(5)},
		{Path: "f00dfeed1/SHA256SUMS", ModTime: at(6)},
		{Path: "v26.11.2/SHA256SUMS", ModTime: at(7)},
		{Path: "0ddba11c3/SHA256SUMS", ModTime: at(8)},
	}
	if got := strings.Join(prunable(listing, 2, nil), ","); got != "a1b2c3d4e,v26.11.0-rc.1,v26.11.1-3-g0bcb9eb" {
		t.Fatalf("prunable keep 2 = %s", got)
	}
	if got := strings.Join(prunable(listing, 0, nil), ","); got != "0ddba11c3,a1b2c3d4e,f00dfeed1,v26.11.0-rc.1,v26.11.1-3-g0bcb9eb" {
		t.Fatalf("prunable keep 0 = %s", got)
	}
}

func TestPruneCommandKeepsThirtyNightliesByDefault(t *testing.T) {
	var entries []map[string]any
	for i := 0; i < 32; i++ {
		entries = append(entries, map[string]any{"Path": fmt.Sprintf("n%02d/SHA256SUMS", i), "ModTime": time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339)})
	}
	entries = append(entries, map[string]any{"Path": "v26.1.0/SHA256SUMS", "ModTime": "2025-01-01T00:00:00Z"})
	listing, _ := json.Marshal(entries)
	var out, errOut bytes.Buffer
	if code := run([]string{"prune"}, bytes.NewReader(listing), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if out.String() != "n00\nn01\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestPruneCommandReadsRcloneJSON(t *testing.T) {
	listing, _ := json.Marshal([]map[string]any{
		{"Path": "old/SHA256SUMS", "Name": "SHA256SUMS", "Size": 10, "ModTime": "2026-01-01T00:00:00Z", "IsDir": false},
		{"Path": "new/SHA256SUMS", "Name": "SHA256SUMS", "Size": 10, "ModTime": "2026-10-01T00:00:00.123456789+08:00", "IsDir": false},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"prune", "-keep", "1"}, bytes.NewReader(listing), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if out.String() != "old\n" {
		t.Fatalf("output = %q", out.String())
	}
	if code := run([]string{"prune"}, strings.NewReader("not json"), &out, &errOut); code != 1 {
		t.Fatalf("bad input exit = %d", code)
	}
}

func TestAssembleCommandNeedsTheSigningKey(t *testing.T) {
	t.Setenv("GPUWORKS_DIST_SIGNING_KEY", "")
	var out, errOut bytes.Buffer
	code := run([]string{"assemble", "-channel", "nightly", "-dist", distDir(t, "gpuworks-a1-linux-amd64.tar.gz"), "-out", filepath.Join(t.TempDir(), "o")}, nil, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "GPUWORKS_DIST_SIGNING_KEY") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	t.Setenv("GPUWORKS_DIST_SIGNING_KEY", base64.StdEncoding.EncodeToString(testPrivate.Seed()))
	t.Setenv("GPUWORKS_DIST_SIGNING_KEY_ID", "test")
	outDir := filepath.Join(t.TempDir(), "o")
	code = run([]string{"assemble", "-channel", "nightly", "-dist", distDir(t, "gpuworks-a1-linux-amd64.tar.gz"), "-out", outDir, "-commit", "c1"}, nil, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "nightly release a1 (1 archives)") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	if _, _, err := distribution.Open(readTree(t, outDir, "nightly/index.json"), distribution.Keys{"test": testPublic}); err != nil {
		t.Fatal(err)
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, nil, &out, &errOut); code != 2 {
		t.Fatalf("no args exit = %d", code)
	}
	if code := run([]string{"frobnicate"}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown subcommand") {
		t.Fatalf("unknown subcommand: %d %s", code, errOut.String())
	}
	if code := run([]string{"help"}, nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "gpuworks-dist assemble") {
		t.Fatalf("help: %d %s", code, out.String())
	}
}

func TestNewerComparesBuildTimes(t *testing.T) {
	in := realInput(t, distDir(t, "gpuworks-abc123-linux-amd64.tar.gz"))
	in.BuiltAt = "2026-10-11T08:00:00+08:00"
	if _, err := assemble(in); err != nil {
		t.Fatal(err)
	}
	served := serveTree(t, in.OutDir)
	client := &distribution.Client{Sources: []string{"http://127.0.0.1:1", served.URL}, Keys: distribution.Keys{"test": testPublic}, IndexTimeout: time.Second}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }

	got, err := publishedNewer(context.Background(), client, "nightly", at("2026-10-10T23:00:00Z"))
	if err != nil || !strings.Contains(got, "serves abc123 built 2026-10-11T08:00:00+08:00") {
		t.Fatalf("an older pipeline must see the newer build: %q, %v", got, err)
	}
	// 08:00+08:00 is 00:00Z: the same instant is not newer.
	if got, _ := publishedNewer(context.Background(), client, "nightly", at("2026-10-11T00:00:00Z")); got != "" {
		t.Fatalf("same build time reported as newer: %q", got)
	}
	if got, _ := publishedNewer(context.Background(), client, "release", at("2026-01-01T00:00:00Z")); got != "" {
		t.Fatalf("an unpublished channel is never newer: %q", got)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"newer", "-channel", "nightly", "-built-at", "yesterday"}, nil, &out, &errOut); code != 2 {
		t.Fatalf("bad -built-at exit = %d", code)
	}
	if code := run([]string{"newer", "-channel", "nightly", "-built-at", "2026-10-12T00:00:00Z", "-source", "http://127.0.0.1:1"}, nil, &out, &errOut); code != 1 {
		t.Fatalf("nothing newer exit = %d (%s)", code, errOut.String())
	}
}
