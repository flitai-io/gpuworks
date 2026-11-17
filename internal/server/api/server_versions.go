package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/version"
)

// Older clients post no selection (an empty body or object) to install the
// channel head. Once any field is supplied, the channel and version are required.
func readUpgradeSelection(body io.Reader) (dto.ServerUpgradeSelection, bool) {
	var fields map[string]string
	reader := json.NewDecoder(body)
	err := reader.Decode(&fields)
	if err == io.EOF {
		return dto.ServerUpgradeSelection{}, true
	}
	if err != nil || reader.Decode(&struct{}{}) != io.EOF {
		return dto.ServerUpgradeSelection{}, false
	}
	if len(fields) == 0 {
		return dto.ServerUpgradeSelection{}, true
	}
	for name := range fields {
		switch name {
		case "channel", "version", "sha256", "attempt":
		default:
			return dto.ServerUpgradeSelection{}, false
		}
	}
	target := dto.ServerUpgradeSelection{Channel: fields["channel"], Version: fields["version"], SHA256: fields["sha256"], Attempt: fields["attempt"]}
	return target, distribution.KnownChannel(target.Channel) && target.Version != "" && len(target.Attempt) <= 64
}

func upgradeCompatibility(release distribution.ServerRelease, channel string, migration int) string {
	if release.DatabaseMigration > 0 && release.DatabaseMigration < migration {
		return "backup"
	}
	if release.NewerThan(version.Version, version.BuildDate) {
		return ""
	}
	if channel == version.Channel {
		return "older"
	}
	if release.DatabaseMigration == 0 {
		return "backup"
	}
	return ""
}

func (a *apiHandlers) matchChannel(w http.ResponseWriter, channel string) bool {
	if channel == a.channelName() {
		return true
	}
	writeProductError(w, http.StatusConflict, ErrorUpgradeSelectionChanged, "the update channel changed; check again", nil)
	return false
}

func (a *apiHandlers) serverVersionChoices(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if !a.matchChannel(w, channel) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), updatesTimeout)
	defer cancel()
	result := dto.ServerVersionChoices{Channel: channel, Platform: runtime.GOOS + "/" + runtime.GOARCH, Versions: []dto.ServerVersionChoice{}}
	fetched, err := a.d.Outside.channel().FetchIndex(ctx, channel)
	if err != nil {
		var sourceErr *distribution.SourcesError
		if errors.As(err, &sourceErr) && sourceErr.Reason() == distribution.ReasonUnpublished {
			writeJSON(w, http.StatusOK, result)
			return
		}
		writeProductError(w, http.StatusBadGateway, ErrorUpdateSourcesUnavailable, err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	if !a.matchChannel(w, channel) {
		return
	}
	migration, err := a.d.Store.CurrentMigration()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for index, release := range fetched.Index.Servers() {
		installed := release.ReleaseID == version.Version
		if channel == version.Channel && !installed && !release.NewerThan(version.Version, version.BuildDate) {
			continue
		}
		choice := dto.ServerVersionChoice{Version: release.ReleaseID, Commit: release.Commit, BuiltAt: release.BuiltAt, Latest: index == 0}
		asset, err := release.Asset(runtime.GOOS, runtime.GOARCH)
		switch {
		case installed:
			choice.Reason = "installed"
		case err != nil:
			choice.Reason = "arch"
		default:
			choice.SHA256 = asset.SHA256
			choice.Reason = upgradeCompatibility(release, channel, migration)
		}
		result.Versions = append(result.Versions, choice)
	}
	writeJSON(w, http.StatusOK, result)
}

func writeUpgradeBusy(w http.ResponseWriter) {
	writeProductError(w, http.StatusConflict, ErrorUpgradeInProgress, "an upgrade is in progress; check its result", nil)
}

func (a *apiHandlers) lockServerUpgrade(w http.ResponseWriter) bool {
	if a.serverUpgradeMu.TryLock() {
		if a.currentUpgradeProgress().Phase != "restart" {
			return true
		}
		a.serverUpgradeMu.Unlock()
	}
	writeUpgradeBusy(w)
	return false
}

func (a *apiHandlers) currentUpgradeProgress() dto.ServerUpgradeProgress {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()
	return a.upgradeProgress
}

func (a *apiHandlers) setUpgradeProgress(p dto.ServerUpgradeProgress) {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()
	a.upgradeProgress = p
}

func (a *apiHandlers) serverUpgradeResult(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("attempt")
	p := a.currentUpgradeProgress()
	if id != "" && p.Attempt == id {
		writeJSON(w, http.StatusOK, p)
		return
	}
	result, err := a.serverSettings().RollbackResult(id, r.URL.Query().Get("backup"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
