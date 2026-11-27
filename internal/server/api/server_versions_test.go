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

func TestChoicesOmitPastBuildsOnEveryChannel(t *testing.T) {
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
	check(head.ReleaseID)
}

// A release whose database schema is older than this server's cannot open its
// database; the choice says so, and installing it is still allowed because the
// index's migration number is not an install requirement.
func TestAChoiceOlderThanTheDatabaseSaysItCannotOpenIt(t *testing.T) {
	savedVersion, savedDate, savedChannel := version.Version, version.BuildDate, version.Channel
	t.Cleanup(func() { version.Version, version.BuildDate, version.Channel = savedVersion, savedDate, savedChannel })
	version.Version, version.BuildDate, version.Channel = "b5137981c", "2026-10-11T00:11:18Z", "nightly"
	channel := newTestChannel(t)
	schema, err := newImageManifestRun(t).store.CurrentMigration()
	if err != nil || schema < 2 {
		t.Fatalf("the test database's schema = %d, %v", schema, err)
	}
	older := channel.index.Server
	older.ReleaseID, older.BuiltAt, older.DatabaseMigration = "rel-0000", "2026-10-10T00:00:00Z", schema-1
	same := channel.index.Server
	same.DatabaseMigration = schema
	channel.index.Server = same
	channel.index.PreviousServers = []distribution.ServerRelease{older}
	channel.sign()
	run := newUpgradeRun(t, &upgradingServer{StaticServerSettings: StaticServerSettings{Channel: "nightly"}}, channel)

	rec := run.do(http.MethodGet, "/api/v1/updates/server/versions?channel=nightly", "", "")
	var choices dto.ServerVersionChoices
	if err := json.Unmarshal(rec.Body.Bytes(), &choices); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("choices = %d %s", rec.Code, rec.Body.String())
	}
	if len(choices.Versions) != 2 || choices.Versions[0].Version != "rel-0001" || choices.Versions[0].DowngradesSchema ||
		choices.Versions[1].Version != "rel-0000" || !choices.Versions[1].DowngradesSchema {
		t.Fatalf("choices = %+v", choices.Versions)
	}
}

// TestOnlineUpgradeUsesOneOrderingRuleRegardlessOfChannel pins one ordering rule
// across the check, the list and the install; the unpublished-build exception is
// covered by TestABuildTheChannelNeverPublishedIsNotUpToDate.
func TestOnlineUpgradeUsesOneOrderingRuleRegardlessOfChannel(t *testing.T) {
	savedVersion, savedDate, savedChannel := version.Version, version.BuildDate, version.Channel
	t.Cleanup(func() { version.Version, version.BuildDate, version.Channel = savedVersion, savedDate, savedChannel })
	for _, tc := range []struct {
		name, current, target, built string
		migration                    int
		newer                        bool
		// unpublished: on its own channel the running build is later than
		// the head and not listed, so the target can be chosen by name.
		unpublished bool
	}{
		{"higher_tag_with_earlier_build", "v26.11.4", "v26.11.10", "2026-11-01T00:00:00Z", 0, true, false},
		{"lower_tag_with_later_build", "v26.11.4", "v26.11.3", "2026-11-20T00:00:00Z", 999, false, false},
		{"newer_build_without_migration", "v26.11.4", "abc12345", "2026-11-20T00:00:00Z", 0, true, false},
		{"newer_build_with_lower_migration", "abc12345", "v26.11.4", "2026-11-20T00:00:00Z", 1, true, false},
		{"older_build_with_migration", "abc12345", "v26.11.4", "2026-11-01T00:00:00Z", 999, false, true},
		{"equal_build_time", "abc12345", "def67890", "2026-11-18T00:00:00Z", 999, false, false},
		{"legacy_unknown_build_time", "dev", "abc12345", "unknown", 0, true, false},
	} {
		for _, running := range []string{"release", "nightly"} {
			for _, selected := range []string{"release", "nightly"} {
				for _, pinned := range []bool{false, true} {
					name := tc.name + "/" + running + "_to_" + selected
					if pinned {
						name += "/selected"
					} else {
						name += "/latest"
					}
					t.Run(name, func(t *testing.T) {
						version.Version, version.BuildDate, version.Channel = tc.current, "2026-11-18T00:00:00Z", running
						channel := newTestChannel(t)
						channel.index.Channel = selected
						channel.publishServer(tc.target)
						channel.index.Server.BuiltAt, channel.index.Server.DatabaseMigration = tc.built, tc.migration
						channel.sign()
						server := &upgradingServer{StaticServerSettings: StaticServerSettings{Channel: selected}}
						run := newUpgradeRun(t, server, channel)
						choosable := tc.newer || (tc.unpublished && running == selected)
						if status := getUpdateStatus(t, run); status.Server.UpdateAvailable != tc.newer || status.Server.UnpublishedBuild != (choosable && !tc.newer) {
							t.Fatalf("update status = %+v, want newer=%v", status.Server, tc.newer)
						}
						rec := run.do(http.MethodGet, "/api/v1/updates/server/versions?channel="+selected, "", "")
						var choices dto.ServerVersionChoices
						if err := json.Unmarshal(rec.Body.Bytes(), &choices); err != nil || rec.Code != http.StatusOK {
							t.Fatalf("choices: %d %s", rec.Code, rec.Body)
						}
						if choosable {
							if len(choices.Versions) != 1 || choices.Versions[0].Reason != "" || choices.Versions[0].Version != tc.target {
								t.Fatalf("version must be selectable: %+v", choices)
							}
						} else if len(choices.Versions) != 0 {
							t.Fatalf("a channel switch must not expose older versions: %+v", choices)
						}
						body := ""
						if pinned {
							encoded, err := json.Marshal(dto.ServerUpgradeSelection{Channel: selected, Version: tc.target})
							if err != nil {
								t.Fatal(err)
							}
							body = string(encoded)
						}
						response := run.do(http.MethodPost, "/api/v1/updates/server", "application/json", body)
						if tc.newer || (choosable && pinned) {
							if response.Code != http.StatusAccepted || len(server.archives) != 1 || server.archives[0] != "archive "+tc.target {
								t.Fatalf("install newer target: %d %s, archives=%v", response.Code, response.Body, server.archives)
							}
						} else if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), ErrorServerNewerThanRelease) || len(server.archives) != 0 {
							t.Fatalf("older target must be refused before installation: %d %s, archives=%v", response.Code, response.Body, server.archives)
						}
					})
				}
			}
		}
	}
}
