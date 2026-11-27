package distribution

import (
	"encoding/json"
	"strings"
	"testing"
)

const goodSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validIndex() Index {
	index := testIndex("nightly",
		ReleaseFile{OS: "linux", Arch: "amd64", File: File{Path: "releases/abc1234/gpuworks-abc1234-linux-amd64.tar.gz", Size: 10, SHA256: goodSHA}},
		ReleaseFile{OS: "linux", Arch: "arm64", File: File{Path: "releases/abc1234/gpuworks-abc1234-linux-arm64.tar.gz", Size: 11, SHA256: goodSHA}},
	)
	index.ImageCatalog = &CatalogFile{Version: "2026-10-08.ab12cd34", File: File{Path: "catalog/images/" + goodSHA + ".json", Size: 5, SHA256: goodSHA}}
	index.TemplateCatalog = &TemplateCatalog{
		CatalogFile:   CatalogFile{Version: "2026-09-14", File: File{Path: "catalog/templates/" + goodSHA + ".tar.gz", Size: 7, SHA256: goodSHA}},
		PackageDigest: goodSHA, SchemaVersion: 1,
	}
	return index
}

// The wire form is what CI writes and every released client reads; a renamed
// field would strand installed servers, so the keys are pinned literally.
func TestIndexWireJSON(t *testing.T) {
	raw := `{"schema":1,"channel":"nightly","published_at":"2026-10-11T02:00:00Z",
	  "server":{"release_id":"abc1234","commit":"abc1234def","built_at":"2026-10-11T01:00:00Z",
	    "files":[{"os":"linux","arch":"amd64","path":"releases/abc1234/x.tar.gz","size":10,"sha256":"` + goodSHA + `"}]},
	  "image_catalog":{"version":"2026-10-08.ab12cd34","path":"catalog/images/a.json","size":5,"sha256":"` + goodSHA + `"},
	  "template_catalog":{"version":"2026-09-14","package_digest":"` + goodSHA + `","schema_version":1,"path":"catalog/templates/a.tar.gz","size":7,"sha256":"` + goodSHA + `"},
	  "added_later":{"anything":true}}`
	index, err := ParseIndex([]byte(raw), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	asset, err := index.Server.Asset("linux", "amd64")
	if err != nil || asset.Path != "releases/abc1234/x.tar.gz" || asset.Size != 10 || asset.SHA256 != goodSHA {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	if index.ImageCatalog.Version != "2026-10-08.ab12cd34" || index.ImageCatalog.Path != "catalog/images/a.json" {
		t.Fatalf("image catalog = %+v", index.ImageCatalog)
	}
	if tc := index.TemplateCatalog; tc.PackageDigest != goodSHA || tc.SchemaVersion != 1 || tc.Size != 7 || tc.Version != "2026-09-14" {
		t.Fatalf("template catalog = %+v", tc)
	}

	out, err := json.Marshal(validIndex())
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(out, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "channel", "published_at", "server", "image_catalog", "template_catalog"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("marshalled index lacks %q: %s", key, out)
		}
	}
	file := generic["server"].(map[string]any)["files"].([]any)[0].(map[string]any)
	for _, key := range []string{"os", "arch", "path", "size", "sha256"} {
		if _, ok := file[key]; !ok {
			t.Errorf("release file lacks %q: %v", key, file)
		}
	}
	template := generic["template_catalog"].(map[string]any)
	for _, key := range []string{"version", "package_digest", "schema_version", "path", "size", "sha256"} {
		if _, ok := template[key]; !ok {
			t.Errorf("template catalog lacks %q: %v", key, template)
		}
	}
}

func TestCatalogsAreOptional(t *testing.T) {
	index := validIndex()
	index.ImageCatalog, index.TemplateCatalog = nil, nil
	payload, _ := json.Marshal(index)
	if strings.Contains(string(payload), "image_catalog") || strings.Contains(string(payload), "template_catalog") {
		t.Fatalf("absent catalogs must be omitted: %s", payload)
	}
	if _, err := ParseIndex(payload, "nightly"); err != nil {
		t.Fatal(err)
	}
}

func TestParseIndexRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*Index)
		channel string
		want    string
	}{
		"other schema":          {mutate: func(x *Index) { x.Schema = 2 }, want: "schema 2"},
		"other channel":         {mutate: func(*Index) {}, channel: "release", want: `channel "nightly", not "release"`},
		"bad channel name":      {mutate: func(x *Index) { x.Channel = "Pre view" }, channel: "Pre view", want: "invalid"},
		"no release id":         {mutate: func(x *Index) { x.Server.ReleaseID = "" }, want: "release_id"},
		"release id with slash": {mutate: func(x *Index) { x.Server.ReleaseID = "a/b" }, want: "release_id"},
		"absolute path":         {mutate: func(x *Index) { x.Server.Files[0].Path = "/etc/passwd" }, want: "plain relative path"},
		"parent path":           {mutate: func(x *Index) { x.Server.Files[0].Path = "../x" }, want: "plain relative path"},
		"inner parent":          {mutate: func(x *Index) { x.Server.Files[0].Path = "releases/../x" }, want: "plain relative path"},
		"dot segment":           {mutate: func(x *Index) { x.Server.Files[0].Path = "./x" }, want: "plain relative path"},
		"backslash":             {mutate: func(x *Index) { x.Server.Files[0].Path = `releases\x` }, want: "plain relative path"},
		"url":                   {mutate: func(x *Index) { x.Server.Files[0].Path = "https://evil/x" }, want: "plain relative path"},
		"zero size":             {mutate: func(x *Index) { x.Server.Files[0].Size = 0 }, want: "no size"},
		"short sha":             {mutate: func(x *Index) { x.Server.Files[0].SHA256 = "abc" }, want: "sha256"},
		"upper-case sha":        {mutate: func(x *Index) { x.Server.Files[0].SHA256 = strings.ToUpper(goodSHA) }, want: "sha256"},
		"platform twice":        {mutate: func(x *Index) { x.Server.Files[1].Arch = "amd64" }, want: "twice"},
		"file without os":       {mutate: func(x *Index) { x.Server.Files[0].OS = "" }, want: "os/arch"},
		"image catalog version": {mutate: func(x *Index) { x.ImageCatalog.Version = " " }, want: "image catalog has no version"},
		"image catalog path":    {mutate: func(x *Index) { x.ImageCatalog.Path = "../m.json" }, want: "image catalog"},
		"template digest":       {mutate: func(x *Index) { x.TemplateCatalog.PackageDigest = "nope" }, want: "package_digest"},
		"template schema":       {mutate: func(x *Index) { x.TemplateCatalog.SchemaVersion = 0 }, want: "schema_version"},
		"template catalog sha":  {mutate: func(x *Index) { x.TemplateCatalog.SHA256 = "" }, want: "template catalog"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := validIndex()
			tc.mutate(&index)
			payload, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			channel := tc.channel
			if channel == "" {
				channel = "nightly"
			}
			_, err = ParseIndex(payload, channel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseIndex error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if _, err := ParseIndex([]byte("{"), "nightly"); err == nil {
		t.Fatal("a truncated payload must not parse")
	}
}

func TestAssetPicksExactPlatform(t *testing.T) {
	index := validIndex()
	got, err := index.Server.Asset("linux", "arm64")
	if err != nil || got.Size != 11 {
		t.Fatalf("Asset(linux, arm64) = %+v, %v", got, err)
	}
	if _, err := index.Server.Asset("darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "no darwin/arm64 build") {
		t.Fatalf("missing platform error = %v", err)
	}
}

func TestValidChannel(t *testing.T) {
	for _, ok := range []string{"nightly", "release", "beta-2"} {
		if !ValidChannel(ok) {
			t.Errorf("%q should be a valid channel", ok)
		}
	}
	for _, bad := range []string{"", "Stable", "2stable", "a/b", "..", strings.Repeat("a", 33)} {
		if ValidChannel(bad) {
			t.Errorf("%q should not be a valid channel", bad)
		}
	}
}

func TestNormalizeSources(t *testing.T) {
	got, err := NormalizeSources([]string{" https://a.example/dist/ ", "https://a.example/dist", "http://b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "https://a.example/dist,http://b.example" {
		t.Fatalf("NormalizeSources = %v", got)
	}
	for _, bad := range []string{"ftp://a", "https://", "a.example", "https://a.example/?x=1", "https://a.example/#f"} {
		if _, err := NormalizeSource(bad); err == nil {
			t.Errorf("NormalizeSource(%q) should fail", bad)
		}
	}
}

func TestDefaultSourcesAndKeys(t *testing.T) {
	sources, err := (&Client{}).sources()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sources, ",") != strings.Join(DefaultSources, ",") || len(sources) != 3 {
		t.Fatalf("default sources = %v", sources)
	}
	for _, source := range DefaultSources {
		if !strings.HasPrefix(source, "https://") {
			t.Errorf("default source %s must be https", source)
		}
	}
	if key := TrustedKeys()["gpuworks-dist-1"]; len(key) != 32 {
		t.Fatalf("built-in key gpuworks-dist-1 = %v", key)
	}
}

func TestKnownChannels(t *testing.T) {
	if !KnownChannel("nightly") || !KnownChannel("release") || KnownChannel("preview") || KnownChannel("") {
		t.Fatal("the published channels are nightly and release")
	}
}

func TestServerReleaseNewerThan(t *testing.T) {
	release := ServerRelease{ReleaseID: "v1.3.0", BuiltAt: "2026-10-11T10:00:00+08:00"}
	for _, tc := range []struct {
		id, built string
		want      bool
	}{
		{"v1.3.0", "2026-01-01T00:00:00Z", false},  // the same release
		{"abc1234", "2026-10-11T01:00:00Z", true},  // an older nightly (09:00 CST)
		{"def5678", "2026-10-11T03:00:00Z", false}, // a nightly built after the release
		{"dev", "unknown", true},                   // a local build
		{"abc1234", "2026-10-11T02:00:00Z", false}, // the same instant is not newer
	} {
		if got := release.NewerThan(tc.id, tc.built); got != tc.want {
			t.Errorf("NewerThan(%s, %s) = %v, want %v", tc.id, tc.built, got, tc.want)
		}
	}
	if !(ServerRelease{ReleaseID: "x", BuiltAt: "garbage"}).NewerThan("y", "2026-10-11T00:00:00Z") {
		t.Fatal("an unreadable release time must not hide an update")
	}
}

func TestServerReleaseOlderThan(t *testing.T) {
	release := ServerRelease{ReleaseID: "v1.3.0", BuiltAt: "2026-10-11T10:00:00+08:00"}
	for _, tc := range []struct {
		built string
		want  bool
	}{
		{"2026-10-11T03:00:00Z", true},  // a server built after the release
		{"2026-10-11T02:00:00Z", false}, // the same instant
		{"2026-10-11T01:00:00Z", false}, // a server built before it
		{"unknown", false},              // a local build: nothing is known
	} {
		if got := release.OlderThan("abc1234", tc.built); got != tc.want {
			t.Errorf("OlderThan(abc1234, %s) = %v, want %v", tc.built, got, tc.want)
		}
	}
	if (ServerRelease{BuiltAt: "garbage"}).OlderThan("abc1234", "2026-10-11T00:00:00Z") {
		t.Fatal("an unreadable release time must not hide catalog updates")
	}
}

// A build the channel never published is told apart from one that is ahead
// on the channel's own line only by the channel's list, never by build time.
func TestIndexUnpublished(t *testing.T) {
	nightly := Index{Channel: ChannelNightly,
		Server:          ServerRelease{ReleaseID: "cccc3333", BuiltAt: "2026-11-25T12:52:50Z"},
		PreviousServers: []ServerRelease{{ReleaseID: "bbbb2222", BuiltAt: "2026-11-24T10:00:00Z"}}}
	release := Index{Channel: ChannelRelease, Server: ServerRelease{ReleaseID: "v26.11.4", BuiltAt: "2026-11-20T00:00:00Z"}}
	for _, tc := range []struct {
		name               string
		index              Index
		channel, id, built string
		want               bool
	}{
		{"a branch build made after the head", nightly, "nightly", "b5137981c", "2026-11-25T13:04:08Z", true},
		{"the head itself", nightly, "nightly", "cccc3333", "2026-11-25T12:52:50Z", false},
		{"the head rebuilt later keeps its place", nightly, "nightly", "cccc3333", "2026-11-25T14:00:00Z", false},
		{"an earlier listed build", nightly, "nightly", "bbbb2222", "2026-11-24T10:00:00Z", false},
		{"an unlisted build made before the head is behind", nightly, "nightly", "aaaa1111", "2026-11-01T00:00:00Z", false},
		{"a build made at the head's instant", nightly, "nightly", "dddd4444", "2026-11-25T12:52:50Z", false},
		{"a local build with no build time", nightly, "nightly", "dev", "unknown", false},
		{"a release server following nightly", nightly, "release", "v26.11.4", "2026-11-26T00:00:00Z", false},
		{"a nightly server following release", release, "nightly", "b5137981c", "2026-11-25T13:04:08Z", false},
		{"a later version tag the index does not list yet", release, "release", "v26.11.5", "2026-11-26T00:00:00Z", false},
		{"a branch build made for the release channel", release, "release", "v26.11.4-3-gabc1234", "2026-11-26T00:00:00Z", true},
	} {
		if got := tc.index.Unpublished(tc.channel, tc.id, tc.built); got != tc.want {
			t.Errorf("%s: Unpublished = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Two version tags order by version, not build time: a patch to an older
// release line is built after the newer line's release but is still older.
func TestServerReleaseOrdersVersionTagsByVersion(t *testing.T) {
	const early, late = "2026-11-01T00:00:00Z", "2026-11-20T00:00:00Z"
	for _, tc := range []struct {
		name                     string
		theirs, theirBuilt       string
		ours, ourBuilt           string
		wantNewer, wantOlderThan bool
	}{
		{"a patch of the running line", "v26.11.1", late, "v26.11.0", early, true, false},
		{"the next line", "v26.12.0", late, "v26.11.3", early, true, false},
		{"a later patch of an older line", "v26.11.2", late, "v26.12.0", early, false, true},
		{"a newer line built before an old-line patch", "v26.12.0", early, "v26.11.2", late, true, false},
		{"months order numerically, not as text", "v26.10.0", early, "v26.9.4", late, true, false},
		{"years order before months", "v27.1.0", early, "v26.12.9", late, true, false},
		{"a pre-release tag falls back to build time", "v26.12.0", early, "v26.12.0-rc.1", late, false, true},
		{"a git describe build falls back to build time", "v26.11.0", late, "v26.11.0-3-gabc1234", early, true, false},
		{"a nightly falls back to build time", "v26.11.0", early, "abc1234", late, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := ServerRelease{ReleaseID: tc.theirs, BuiltAt: tc.theirBuilt}
			if got := release.NewerThan(tc.ours, tc.ourBuilt); got != tc.wantNewer {
				t.Errorf("%s NewerThan(%s) = %v, want %v", tc.theirs, tc.ours, got, tc.wantNewer)
			}
			if got := release.OlderThan(tc.ours, tc.ourBuilt); got != tc.wantOlderThan {
				t.Errorf("%s OlderThan(%s) = %v, want %v", tc.theirs, tc.ours, got, tc.wantOlderThan)
			}
		})
	}
	// Versions order even when a build time is unreadable.
	if (ServerRelease{ReleaseID: "v26.11.0", BuiltAt: "garbage"}).NewerThan("v26.11.1", "unknown") {
		t.Fatal("v26.11.0 is not an update for v26.11.1")
	}
}
