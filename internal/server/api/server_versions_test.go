package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/version"
)

func TestUpgradeSelectionNeverTreatsAPartialPinAsLatest(t *testing.T) {
	for _, input := range []string{"", "{}", " { } ", "null"} {
		if req, ok := readUpgradeSelection(strings.NewReader(input)); !ok || req.Version != "" {
			t.Fatal(input)
		}
	}
	for _, input := range []string{`{"version":null}`, `{"release":"v1"}`, `{"version":"v1"}`, `{"channel":"nightly"}`, `{}{}`, `{}` + strings.Repeat("x", 5000)} {
		if _, ok := readUpgradeSelection(strings.NewReader(input)); ok {
			t.Fatalf("partial pin accepted: %s", input)
		}
	}
}

func TestSelectedUpgradeUsesTheSignedChoiceAfterHeadMoves(t *testing.T) {
	c := newTestChannel(t)
	chosen := c.index.Server
	chosen.Commit = strings.Repeat("beef", 10)
	c.publishServer("rel-0002")
	c.index.PreviousServers = []distribution.ServerRelease{chosen}
	c.sign()
	server := &upgradingServer{}
	run := newUpgradeRun(t, server, c)
	choices := run.do(http.MethodGet, "/api/v1/updates/server/versions?channel=nightly", "", "")
	var list struct{ Versions []dto.ServerVersionChoice }
	if err := json.Unmarshal(choices.Body.Bytes(), &list); err != nil || len(list.Versions) != 2 || list.Versions[1].Version != chosen.ReleaseID {
		t.Fatalf("choices: %s", choices.Body)
	}
	if list.Versions[1].Commit != chosen.Commit {
		t.Fatal("the history must expose its signed commit so a tagged version can be found by commit")
	}
	target := dto.ServerUpgradeSelection{Channel: "nightly", Version: chosen.ReleaseID, SHA256: list.Versions[1].SHA256, Attempt: "browser-one"}
	post := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(target)
		return run.do(http.MethodPost, "/api/v1/updates/server", "application/json", string(body))
	}
	if got := post(); got.Code != http.StatusAccepted || len(server.archives) != 1 || server.archives[0] != "archive rel-0001" {
		t.Fatalf("selected archive %v, response %s", server.archives, got.Body)
	}
	if got := post(); got.Code != http.StatusConflict || len(server.archives) != 1 {
		t.Fatalf("replayed attempt: %s", got.Body)
	}
	progress := run.do(http.MethodGet, "/api/v1/updates/server/result?attempt=browser-one", "", "")
	if !strings.Contains(progress.Body.String(), `"phase":"restart"`) {
		t.Fatalf("progress: %s", progress.Body)
	}
	run = newUpgradeRun(t, server, c) // Independent requests after a process restart.
	target.Attempt = "changed"
	target.SHA256 = "stale"
	if got := post(); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), ErrorUpgradeSelectionChanged) {
		t.Fatal(got.Body)
	}
	target.Attempt = "withdrawn"
	c.index.PreviousServers = nil
	c.sign()
	if got := post(); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), ErrorUpgradeWithdrawn) {
		t.Fatal(got.Body)
	}
	target.Channel = "release"
	if got := post(); got.Code != http.StatusConflict || len(server.archives) != 1 {
		t.Fatalf("channel changed: %s", got.Body)
	}
}

func TestChoicesOmitPastBuildsOnlyOnTheRunningChannel(t *testing.T) {
	savedVersion, savedDate, savedChannel := version.Version, version.BuildDate, version.Channel
	t.Cleanup(func() { version.Version, version.BuildDate, version.Channel = savedVersion, savedDate, savedChannel })
	channel := newTestChannel(t)
	installed := channel.index.Server
	installed.ReleaseID, installed.BuiltAt = "bbbb2222", "2026-11-10T00:00:00Z"
	earlier := installed
	earlier.ReleaseID, earlier.BuiltAt = "aaaa1111", "2026-11-09T00:00:00Z"
	head := installed
	head.ReleaseID, head.BuiltAt = "cccc3333", "2026-11-11T00:00:00Z"
	channel.index.Server, channel.index.PreviousServers = head, []distribution.ServerRelease{installed, earlier}
	channel.sign()
	run := newUpgradeRun(t, &upgradingServer{StaticServerSettings: StaticServerSettings{Channel: "nightly"}}, channel)
	check := func(want ...string) {
		t.Helper()
		rec := run.do(http.MethodGet, "/api/v1/updates/server/versions?channel=nightly", "", "")
		var list dto.ServerVersionChoices
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("list: %s", rec.Body)
		}
		var got []string
		for _, entry := range list.Versions {
			got = append(got, entry.Version)
			if entry.Latest != (entry.Version == channel.index.Server.ReleaseID) {
				t.Fatalf("incorrect channel head: %+v", entry)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	version.Version, version.BuildDate, version.Channel = installed.ReleaseID, installed.BuiltAt, "nightly"
	check(head.ReleaseID, installed.ReleaseID)
	version.Version, version.Channel = "v26.11.4", "release"
	check(head.ReleaseID, installed.ReleaseID, earlier.ReleaseID)
}

func TestOlderBuildAcrossChannelsRequiresCompatibleSignedMigration(t *testing.T) {
	before := version.Channel
	version.Channel = distribution.ChannelNightly
	defer func() { version.Channel = before }()
	oldVersion, oldDate := version.Version, version.BuildDate
	version.Version, version.BuildDate = "abc123", "2026-11-01T00:00:00Z"
	defer func() { version.Version, version.BuildDate = oldVersion, oldDate }()
	r := distribution.ServerRelease{ReleaseID: "v26.10.0", BuiltAt: "2026-10-01T00:00:00Z"}
	if got := upgradeCompatibility(r, "release", 4); got != "backup" {
		t.Fatal(got)
	}
	r.DatabaseMigration = 4
	if got := upgradeCompatibility(r, "release", 4); got != "" {
		t.Fatal(got)
	}
	if got := upgradeCompatibility(r, "nightly", 4); got != "older" {
		t.Fatal(got)
	}
	if got := upgradeCompatibility(r, "release", 5); got != "backup" {
		t.Fatal(got)
	}
}
