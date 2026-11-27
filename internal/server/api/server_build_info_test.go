package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/version"
)

const buildInfoCommit = "0123456789abcdef0123456789abcdef01234567"
const buildInfoDate = "2026-10-30T02:00:00Z"

func setBuildInfoVersion(t *testing.T, release, commit, built, channel string) {
	t.Helper()
	oldVersion, oldCommit, oldDate, oldChannel := version.Version, version.Commit, version.BuildDate, version.Channel
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuildDate, version.Channel = oldVersion, oldCommit, oldDate, oldChannel
	})
	version.Version, version.Commit, version.BuildDate, version.Channel = release, commit, built, channel
}

func verifiedBuildInfoIndex(release, channel string) *distribution.Fetched {
	return &distribution.Fetched{Channel: channel, Index: distribution.Index{
		Channel: channel, PublishedAt: time.Date(2026, 10, 30, 2, 1, 0, 0, time.UTC),
		Server: distribution.ServerRelease{ReleaseID: release, Commit: buildInfoCommit, BuiltAt: buildInfoDate},
	}}
}

func TestServerBuildInfoRequiresExactPublicationIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, release, buildChannel, publishedChannel, kind string
		mutate                                              func(*distribution.Fetched)
		published                                           bool
	}{
		{name: "stable", release: "v1.2.3", buildChannel: "release", publishedChannel: "release", kind: "stable", published: true},
		{name: "prerelease", release: "v1.2.3-rc.1", buildChannel: "release", publishedChannel: "release", kind: "prerelease", published: true},
		{name: "nightly", release: "a69b04107", buildChannel: "nightly", publishedChannel: "nightly", kind: "prerelease", published: true},
		{name: "future_candidate", release: "v1.2.3", buildChannel: "release", publishedChannel: "release", kind: "unknown", mutate: func(f *distribution.Fetched) { f.Index.Server.ReleaseID = "v1.2.4" }},
		{name: "same_commit_different_version", release: "v1.2.3", buildChannel: "nightly", publishedChannel: "release", kind: "development", mutate: func(f *distribution.Fetched) { f.Index.Server.ReleaseID = "v1.2.4" }},
		{name: "same_version_different_commit", release: "v1.2.3", buildChannel: "release", publishedChannel: "release", kind: "unknown", mutate: func(f *distribution.Fetched) { f.Index.Server.Commit = strings.Repeat("a", 40) }},
		{name: "same_commit_different_build_time", release: "v1.2.3", buildChannel: "nightly", publishedChannel: "release", kind: "development", mutate: func(f *distribution.Fetched) { f.Index.Server.BuiltAt = "2026-10-30T02:00:01Z" }},
		{name: "invalid_published_build_time", release: "v1.2.3", buildChannel: "release", publishedChannel: "release", kind: "unknown", mutate: func(f *distribution.Fetched) { f.Index.Server.BuiltAt = "unknown" }},
		{name: "same_build_different_timezone", release: "v1.2.3", buildChannel: "release", publishedChannel: "release", kind: "stable", published: true, mutate: func(f *distribution.Fetched) { f.Index.Server.BuiltAt = "2026-10-30T10:00:00+08:00" }},
		{name: "release_channel_non_semver", release: "release-candidate", buildChannel: "release", publishedChannel: "release", kind: "unknown", published: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setBuildInfoVersion(t, tt.release, buildInfoCommit, buildInfoDate, tt.buildChannel)
			fetched := verifiedBuildInfoIndex(tt.release, tt.publishedChannel)
			if tt.mutate != nil {
				tt.mutate(fetched)
			}
			got := serverBuildInfo(tt.release, fetched)
			if got.Kind != tt.kind || (got.PublishedVersion != "") != tt.published || got.Version != tt.release || got.Commit != buildInfoCommit || got.BuiltAt != buildInfoDate || got.BuildChannel != tt.buildChannel {
				t.Fatalf("build attribution = %+v", got)
			}
			if tt.published && (got.PublishedVersion != tt.release || got.PublishedAt != "2026-10-30T02:01:00Z") {
				t.Fatalf("publication metadata = %+v", got)
			}
		})
	}
}

func TestViewingAnotherChannelPreservesTheVerifiedRunningIdentity(t *testing.T) {
	setBuildInfoVersion(t, "v1.2.3", buildInfoCommit, buildInfoDate, "release")
	a := &apiHandlers{}
	stable := a.runningBuildInfo(version.Version, verifiedBuildInfoIndex(version.Version, "release"))
	if stable.Kind != "stable" {
		t.Fatal(stable)
	}
	other := a.runningBuildInfo(version.Version, verifiedBuildInfoIndex("b1234567", "nightly"))
	if other != stable {
		t.Fatalf("channel switch changed the running build: %+v", other)
	}
}

func TestServerBuildInfoDoesNotInferPublicationFromBuildLabels(t *testing.T) {
	for _, tt := range []struct{ version, commit, date, channel, kind string }{
		{"v1.2.3", buildInfoCommit, buildInfoDate, "release", "unknown"},
		{"v1.2.3-rc.1", buildInfoCommit, buildInfoDate, "release", "unknown"},
		{"a69b04107", buildInfoCommit, buildInfoDate, "nightly", "development"},
		{"74285901", buildInfoCommit, buildInfoDate, "release", "development"},
		{"anything", buildInfoCommit, buildInfoDate, "custom", "unknown"},
		{"v1.2.3", "01234567", buildInfoDate, "release", "unknown"},
		{"v1.2.3", "none", buildInfoDate, "release", "unknown"},
		{"v1.2.3", buildInfoCommit, "unknown", "release", "unknown"},
		{"v1.2.3", buildInfoCommit, "0001-01-01T00:00:00Z", "release", "unknown"},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s", tt.version, tt.commit, tt.date), func(t *testing.T) {
			setBuildInfoVersion(t, tt.version, tt.commit, tt.date, tt.channel)
			got := serverBuildInfo(tt.version, nil)
			if got.Kind != tt.kind || got.PublishedVersion != "" || got.PublishedAt != "" {
				t.Fatalf("unmatched build = %+v", got)
			}
			if tt.commit != buildInfoCommit || tt.date != buildInfoDate {
				if matched := serverBuildInfo(tt.version, verifiedBuildInfoIndex(tt.version, "release")); matched.Kind != tt.kind || matched.PublishedVersion != "" {
					t.Fatalf("partial or invalid identity matched publication: %+v", matched)
				}
			}
		})
	}
	setBuildInfoVersion(t, "v1.2.3", buildInfoCommit, buildInfoDate, "nightly")
	if got := serverBuildInfo("previous-target", verifiedBuildInfoIndex("previous-target", "release")); !reflect.DeepEqual(got, dto.ServerBuildInfo{Version: "previous-target", Kind: "unknown"}) {
		t.Fatalf("another target borrowed running-build facts: %+v", got)
	}
}

func TestServerBuildInfoOnlyAcceptsCompleteSemanticReleases(t *testing.T) {
	for _, tt := range []struct{ release, kind string }{
		{"v1.2.3", "stable"}, {"1.2.3", "stable"}, {"v0.0.0+build.7", "stable"},
		{"v1.2.3-alpha.1+build.7", "prerelease"}, {"v1.2.3-0", "prerelease"}, {"v1.2.3-01a", "prerelease"},
		{"v1", "unknown"}, {"v1.2", "unknown"}, {"v01.2.3", "unknown"}, {"v1.2.3-01", "unknown"},
		{"v1.2.3-alpha..1", "unknown"}, {"v1.2.3+", "unknown"}, {"v1.2.3-", "unknown"}, {"commit123", "unknown"},
	} {
		t.Run(tt.release, func(t *testing.T) {
			setBuildInfoVersion(t, tt.release, buildInfoCommit, buildInfoDate, "release")
			if got := serverBuildInfo(tt.release, verifiedBuildInfoIndex(tt.release, "release")); got.Kind != tt.kind {
				t.Fatalf("%q classified %q, want %q", tt.release, got.Kind, tt.kind)
			}
		})
	}
}

func publishBuildInfoChannel(t *testing.T, c *testChannel, channel, release string) {
	t.Helper()
	c.publishServer(release)
	c.index.Channel = channel
	c.index.Server.Commit, c.index.Server.BuiltAt = buildInfoCommit, buildInfoDate
	payload, err := json.Marshal(c.index)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := distribution.Seal(payload, "test", channelTestPrivate)
	if err != nil {
		t.Fatal(err)
	}
	c.replace(channel+"/index.json", envelope)
}

func readBuildInfoPreview(t *testing.T, r *imageManifestRun, path, body string) dto.AgentUpgradePreview {
	t.Helper()
	rec := r.do(http.MethodPost, path, "application/json", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status %d: %s", rec.Code, rec.Body)
	}
	var p dto.AgentUpgradePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServerBuildInfoPreviewUsesOnlyVerifiedCachedMetadata(t *testing.T) {
	setBuildInfoVersion(t, "target-build", buildInfoCommit, buildInfoDate, "nightly")
	r, svc := newAgentUpgradeAPIRig(t)
	c := newTestChannel(t)
	publishBuildInfoChannel(t, c, "nightly", version.Version)
	settings := &autoCheckServer{manual: true}
	r.deps.Server = settings
	r.useChannel(c)

	uncached := getUpdateStatus(t, r)
	if uncached.Available || uncached.Server.CurrentBuild.Kind != "development" || uncached.Server.CurrentBuild.Version != version.Version || len(c.requested()) != 0 {
		t.Fatalf("settings without a check lost local build facts or probed a source: %+v", uncached)
	}
	before := readBuildInfoPreview(t, r, "/api/v1/agent-upgrades/preview", `{"scope":"all"}`)
	if before.TargetBuild.Kind != "development" || len(c.requested()) != 0 {
		t.Fatalf("preview probed the network or inferred a publication: %+v, requests=%v", before.TargetBuild, c.requested())
	}
	rec := r.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("updates %d %s", rec.Code, rec.Body)
	}
	var status dto.UpdateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.Server.CurrentBuild.Kind != "prerelease" {
		t.Fatalf("verified matching nightly = %+v", status)
	}
	requests := len(c.requested())
	c.server.Close() // Confirmation remains usable with the publisher offline.
	after := readBuildInfoPreview(t, r, "/api/v1/agent-upgrades/preview", `{"scope":"all"}`)
	if !reflect.DeepEqual(after.TargetBuild, status.Server.CurrentBuild) || len(c.requested()) != requests {
		t.Fatalf("confirmation did not reuse settings metadata offline: %+v, requests=%v", after.TargetBuild, c.requested())
	}

	p, err := svc.Preview(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.Create(context.Background(), agentupgrade.CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "build-info-continue", RequestedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.PauseAgentUpgrade(run.Record.ID, "artifact_unavailable"); err != nil {
		t.Fatal(err)
	}
	continued := readBuildInfoPreview(t, r, fmt.Sprintf("/api/v1/agent-upgrades/%d/continue-preview", run.Record.ID), `{}`)
	if !reflect.DeepEqual(continued.TargetBuild, status.Server.CurrentBuild) || len(c.requested()) != requests {
		t.Fatalf("continue preview lost build information or probed offline source: %+v", continued)
	}
	settings.Channel = "release"
	changed := readBuildInfoPreview(t, r, "/api/v1/agent-upgrades/preview", `{"scope":"all"}`)
	if changed.TargetBuild.Kind != "development" || changed.TargetBuild.PublishedVersion != "" || changed.TargetBuild.BuildChannel != "nightly" || len(c.requested()) != requests {
		t.Fatalf("selected channel relabelled the running build or reused another channel's cache: %+v", changed.TargetBuild)
	}
}

func TestServerBuildInfoUpdatesWireKeepsLatestSeparate(t *testing.T) {
	setBuildInfoVersion(t, "v1.2.3", buildInfoCommit, buildInfoDate, "release")
	r := newImageManifestRun(t)
	c := newTestChannel(t)
	publishBuildInfoChannel(t, c, "release", "v1.2.4")
	r.useChannel(c)
	status := getUpdateStatus(t, r)
	if status.Server.Latest != "v1.2.4" || status.Server.CurrentBuild.Kind != "unknown" || status.Server.CurrentBuild.PublishedVersion != "" || status.Server.CurrentBuild.Version != "v1.2.3" {
		t.Fatalf("latest metadata attributed to current build: %+v", status.Server)
	}

	publishBuildInfoChannel(t, c, "release", version.Version)
	rec := r.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal(wire["server"], &server); err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1.2.3","commit":"0123456789abcdef0123456789abcdef01234567","built_at":"2026-10-30T02:00:00Z","build_channel":"release","edition":"community","kind":"stable","published_version":"v1.2.3","published_at":"2026-10-11T00:00:00Z"}`
	if string(server["current_build"]) != want {
		t.Fatalf("current_build wire = %s, want %s", server["current_build"], want)
	}
	var round dto.ServerBuildInfo
	if err := json.Unmarshal(server["current_build"], &round); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(round)
	if err != nil || string(b) != want {
		t.Fatalf("build wire round-trip %s: %v", b, err)
	}

	c.replace("release/index.json", []byte(`{"schema":1,"channel":"release","server":{"release_id":"v1.2.3"}}`))
	bad := r.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	var invalid dto.UpdateStatus
	if err := json.Unmarshal(bad.Body.Bytes(), &invalid); err != nil {
		t.Fatal(err)
	}
	if invalid.Available || invalid.Server.CurrentBuild.Kind != "unknown" || invalid.Server.CurrentBuild.PublishedVersion != "" {
		t.Fatalf("unsigned source established a publication: %+v", invalid)
	}
}

func TestServerBuildInfoPreviewDoesNotWaitForInflightSource(t *testing.T) {
	setBuildInfoVersion(t, "target-build", buildInfoCommit, buildInfoDate, "nightly")
	r, _ := newAgentUpgradeAPIRig(t)
	c := newTestChannel(t)
	publishBuildInfoChannel(t, c, "nightly", version.Version)
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			c.serve(w, req)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(source.Close)
	t.Cleanup(unblock)
	r.deps.Outside = c.sources()
	r.deps.Outside.Channel.Sources = []string{source.URL}
	r.deps.Outside.Channel.IndexTimeout = time.Minute
	r.router = Router(r.deps)

	checked := make(chan *httptest.ResponseRecorder, 1)
	go func() { checked <- r.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the controlled source did not receive an update check")
	}
	confirmed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		confirmed <- r.do(http.MethodPost, "/api/v1/agent-upgrades/preview", "application/json", `{"scope":"all"}`)
	}()
	select {
	case rec := <-confirmed:
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %d %s", rec.Code, rec.Body)
		}
		var preview dto.AgentUpgradePreview
		if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if preview.TargetVersion != version.Version || preview.TargetBuild.Kind != "development" || preview.TargetBuild.Commit != buildInfoCommit || preview.Counts["eligible"] != 2 {
			t.Fatalf("local target or eligibility was lost during a blocked update check: %+v", preview)
		}
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatal("preview waited for an unrelated source probe")
	}
	select {
	case rec := <-checked:
		t.Fatalf("source answered before it was released: %d", rec.Code)
	default:
	}
	unblock()
	select {
	case rec := <-checked:
		var status dto.UpdateStatus
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || status.Server.CurrentBuild.Kind != "prerelease" {
			t.Fatalf("completed verified check %d %s", rec.Code, rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released update check did not finish")
	}
	preview := readBuildInfoPreview(t, r, "/api/v1/agent-upgrades/preview", `{"scope":"all"}`)
	if preview.TargetBuild.Kind != "prerelease" || preview.TargetBuild.PublishedVersion != version.Version {
		t.Fatalf("completed publication was not reused: %+v", preview.TargetBuild)
	}
}
