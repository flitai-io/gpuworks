package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
	"gpuworks.ai/gpuworks/internal/version"
)

// updatesTimeout bounds one update check or one catalog download: every
// source is asked at once, and each answers within the client's own bounds.
const updatesTimeout = 2 * time.Minute

// prefetchUpdates lets the sign-in page start the update check, so the
// administrator who signs in finds the result ready. It is open to callers
// who are not signed in and so answers nothing about the result; the probe
// runs in the background, one at a time, and the cache bounds how often the
// sources are asked however often this is called (distribution.CheckTTL).
func (a *apiHandlers) prefetchUpdates(w http.ResponseWriter, _ *http.Request) {
	if auto, _ := a.serverSettings().AutoCheckUpdates(); auto {
		a.updates.Prefetch(a.d.Outside.channel(), a.channelName(), updatesTimeout, a.fleetReporter())
		a.refreshNotices()
	}
	w.WriteHeader(http.StatusNoContent)
}

// fleetReporter describes the installation to an automatic update check:
// its ID and versions always, its size and cards too when it takes part in
// the user experience improvement program (usage_report).
func (a *apiHandlers) fleetReporter() func() url.Values { return a.fleetReport }

// fleetReport is what an automatic update check tells the channel about this
// installation (usagereport.FleetReport). A fleet that cannot be read is not
// reported rather than reported wrong.
func (a *apiHandlers) fleetReport() url.Values {
	summary, err := a.d.Store.FleetSummary()
	if err != nil {
		return nil
	}
	if a.d.Online != nil {
		nodes, err := a.d.Store.FleetNodes()
		if err != nil {
			return nil
		}
		summary.OnlineNodes = 0
		for i := range nodes {
			if nodes[i].ConnectionStateForSession(a.d.Online(nodes[i].UUID)) == store.NodeStateOnline {
				summary.OnlineNodes++
			}
		}
	}
	report := &usagereport.FleetReport{
		InstallID: a.serverSettings().InstallID(), ServerVersion: version.Version,
		Fleet: a.d.UsageReport != nil && a.d.UsageReport.Status().Enabled,
		Nodes: summary.Nodes, OnlineNodes: summary.OnlineNodes,
	}
	for _, c := range summary.AgentVersions {
		report.AgentVersions = append(report.AgentVersions, usagereport.Count{Name: c.Name, N: c.Count})
	}
	for _, c := range summary.GPUModels {
		report.GPUModels = append(report.GPUModels, usagereport.Count{Name: c.Name, N: c.Count})
	}
	return report.Query()
}

// refreshNotices fetches the publisher's notices with the update check: the
// same channel, asked on the same occasions.
func (a *apiHandlers) refreshNotices() {
	if a.d.Notices != nil {
		a.d.Notices.Refresh()
	}
}

func (a *apiHandlers) channelName() string {
	channel, _ := a.serverSettings().UpdateChannel()
	return channel
}

// getUpdates reports what the distribution channel offers next to what this
// server runs (ADR 0040). Every source is asked and waited for (or the probe
// is reused, see distribution.CheckTTL); a channel nobody serves is still a 200 with
// available=false and the reasons.
func (a *apiHandlers) getUpdates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), updatesTimeout)
	defer cancel()
	channel, editable := a.serverSettings().UpdateChannel()
	auto, _ := a.serverSettings().AutoCheckUpdates()
	status := dto.UpdateStatus{
		Channel:         channel,
		ChannelEditable: editable,
		AutoCheck:       auto,
		Sources:         []dto.UpdateSourceStatus{},
		Server:          dto.ServerUpdate{Current: version.Version, CurrentBuild: serverBuildInfo(version.Version, nil)},
	}
	blocker := a.serverSettings().SelfUpgradeBlocker()
	status.Server.SelfUpgrade, status.Server.SelfUpgradeBlocker = blocker == "", blocker
	status.Server.InstallDir = a.serverSettings().InstallDir()
	state, err := a.d.Store.OfficialCatalogState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status.ImageCatalog = dto.CatalogUpdate{Current: state.ImageCatalogVersion, Uploaded: state.ImageCatalogUploaded, Compatible: true}
	currentDigest, err := a.d.Store.UpstreamInferencePackageDigest()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status.TemplateCatalog = dto.CatalogUpdate{Current: a.inferenceCatalogVersion(currentDigest), Compatible: true}

	refresh, _ := strconv.ParseBool(r.URL.Query().Get("refresh"))
	// With automatic checks off the sources are asked only when an
	// administrator asks, and they are told nothing then either; a page load
	// shows what an earlier manual check found.
	var fetched *distribution.Fetched
	var checkedAt time.Time
	if auto || refresh {
		a.refreshNotices()
	}
	switch {
	case auto:
		fetched, err, checkedAt = a.updates.Probe(ctx, a.d.Outside.channel(), channel, refresh, a.fleetReporter())
	case refresh:
		fetched, err, checkedAt = a.updates.Probe(ctx, a.d.Outside.channel(), channel, true, nil)
	default:
		fetched, err, checkedAt, _ = a.updates.Cached(channel)
	}
	status.CheckedAt = checkedAt
	if err == nil && fetched == nil {
		writeJSON(w, http.StatusOK, status)
		return
	}
	var attempts []distribution.Attempt
	var sourcesErr *distribution.SourcesError
	switch {
	case err == nil:
		attempts = fetched.Attempts
	case errors.As(err, &sourcesErr):
		attempts = sourcesErr.Attempts
		status.UnavailableReason = sourcesErr.Reason()
	default:
		writeProductError(w, http.StatusBadGateway, ErrorUpdateSourcesUnavailable, err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	for _, attempt := range attempts {
		if errors.Is(attempt.Err, distribution.ErrNotAwaited) {
			// The check answers with the first index that verifies; a slower
			// source was not waited for and says nothing about itself.
			continue
		}
		source := dto.UpdateSourceStatus{URL: attempt.Source, OK: attempt.Err == nil, LatencyMS: attempt.Latency.Milliseconds()}
		if attempt.Err != nil {
			source.Error = attempt.Err.Error()
		}
		status.Sources = append(status.Sources, source)
	}
	if fetched == nil {
		writeJSON(w, http.StatusOK, status)
		return
	}
	status.Server.CurrentBuild = a.runningBuildInfo(version.Version, fetched)
	index := fetched.Index
	status.Available, status.Source = true, fetched.Source
	status.Server.Latest, status.Server.BuiltAt = index.Server.ReleaseID, index.Server.BuiltAt
	status.Server.UpdateAvailable = index.Server.NewerThan(version.Version, version.BuildDate)
	_, assetErr := index.Server.Asset(runtime.GOOS, runtime.GOARCH)
	status.Server.Installable = assetErr == nil
	// Catalog versions are never ordered (ADR 0040); the build they come from
	// is. A channel whose release predates this server (a nightly server
	// switched to release, a build ahead of the channel) offers catalogs older
	// than the embedded ones, which are not updates. They can still be applied
	// by uploading them.
	catalogsBehind := index.Server.OlderThan(version.Version, version.BuildDate)
	if catalog := index.ImageCatalog; catalog != nil {
		status.ImageCatalog.Latest = catalog.Version
		// An uploaded catalog differs from the channel's because someone chose
		// it: the channel's is not offered as its update, and applying it is a
		// replacement the sync refuses unless asked for (syncImagesManifest).
		status.ImageCatalog.UpdateAvailable = catalog.Version != state.ImageCatalogVersion && !catalogsBehind && !state.ImageCatalogUploaded
	}
	if catalog := index.TemplateCatalog; catalog != nil {
		status.TemplateCatalog.Latest = catalog.Version
		status.TemplateCatalog.UpdateAvailable = catalog.PackageDigest != currentDigest && !catalogsBehind
		status.TemplateCatalog.Compatible = catalog.SchemaVersion <= inferencecatalog.PackageSchemaVersion
		if status.TemplateCatalog.Compatible {
			compatible, err := a.templateUpdate.check(ctx, a.d.Outside.channel(), fetched, auto || refresh)
			if err != nil {
				status.TemplateCatalog.CheckError = &dto.ProductMessage{
					Code: ErrorUpdateDownloadFailed, Message: "the published inference template catalog could not be checked: " + err.Error(),
					Params: map[string]any{"file": catalog.Path, "detail": err.Error()},
				}
			}
			status.TemplateCatalog.Compatible = compatible
		}
	}
	writeJSON(w, http.StatusOK, status)
}

// putUpdateAutoCheck switches the server's automatic update checks, and with
// them the report that goes along; it is kept in server.yaml.
func (a *apiHandlers) putUpdateAutoCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Enabled == nil {
		writeProductError(w, http.StatusBadRequest, ErrorUpdateAutoCheckInvalid, "enabled must be true or false", nil)
		return
	}
	if err := a.serverSettings().SetAutoCheckUpdates(*req.Enabled); err != nil {
		writeServerSettingsError(w, err)
		return
	}
	enabled, _ := a.serverSettings().AutoCheckUpdates()
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

// inferenceCatalogVersion names the package the upstream specs come from, the
// same way the distribution index names a published one.
func (a *apiHandlers) inferenceCatalogVersion(digest string) string {
	if digest == "" {
		return ""
	}
	publishedAt := ""
	if a.d.CatalogPackages != nil {
		if pkg, err := a.d.CatalogPackages.Load(digest); err == nil {
			publishedAt = pkg.Manifest.PublishedAt
		}
	}
	return inferencecatalog.CatalogVersion(publishedAt, digest)
}

// channelIndex reads the channel's index for an update action, answering the
// request itself when no source serves one.
func (a *apiHandlers) channelIndex(ctx context.Context, w http.ResponseWriter) (*distribution.Fetched, bool) {
	fetched, err := a.d.Outside.channel().FetchIndex(ctx, a.channelName())
	if err != nil {
		writeProductError(w, http.StatusBadGateway, ErrorUpdateSourcesUnavailable, err.Error(), map[string]any{"detail": err.Error()})
		return nil, false
	}
	return fetched, true
}

// channelFile downloads one file the index lists, checked against it.
func (a *apiHandlers) channelFile(ctx context.Context, w http.ResponseWriter, fetched *distribution.Fetched, file distribution.File) ([]byte, bool) {
	body, _, err := a.d.Outside.channel().ReadFile(ctx, fetched, file)
	if err != nil {
		writeDownloadFailed(w, file, err)
		return nil, false
	}
	return body, true
}

func writeNotPublished(w http.ResponseWriter, kind string) {
	writeProductError(w, http.StatusNotFound, ErrorUpdateNotPublished, "the channel does not publish "+kind, map[string]any{"kind": kind})
}

// maxServerArchiveBytes bounds an uploaded server archive; a release archive
// is about 40 MiB.
const maxServerArchiveBytes = 512 << 20

// serverDownloadTimeout bounds downloading the server archive from the
// channel. The work is detached from the request: a reverse proxy that gives
// up on a slow answer must not leave a half-staged upgrade behind.
const serverDownloadTimeout = 10 * time.Minute

// upgradeServerFromChannel downloads this platform's archive of the channel's
// release (checked against the index) and upgrades the server with it. The
// answer comes before the restart; the console then waits for the new version.
func (a *apiHandlers) upgradeServerFromChannel(w http.ResponseWriter, r *http.Request) {
	if !a.lockServerUpgrade(w) {
		return
	}
	defer a.serverUpgradeMu.Unlock()
	target, valid := readUpgradeSelection(http.MaxBytesReader(w, r.Body, 4096))
	if !valid {
		writeProductError(w, http.StatusBadRequest, ErrorUpgradeSelectionInvalid, "select a channel and version", nil)
		return
	}
	if target.Channel != "" && !a.matchChannel(w, target.Channel) {
		return
	}
	if target.Attempt != "" {
		prior := a.currentUpgradeProgress()
		if prior.Attempt == target.Attempt {
			writeUpgradeBusy(w)
			return
		}
		previous, err := a.serverSettings().RollbackResult(target.Attempt, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if previous.Phase == "rollback" {
			writeProductError(w, http.StatusConflict, ErrorUpgradeAttemptFinished, "this upgrade already rolled back", nil)
			return
		}
		a.setUpgradeProgress(dto.ServerUpgradeProgress{Attempt: target.Attempt, Phase: "download", From: version.Version, To: target.Version})
		defer func() {
			p := a.currentUpgradeProgress()
			if p.Phase == "download" {
				p.Phase = "failed"
				a.setUpgradeProgress(p)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), serverDownloadTimeout)
	defer cancel()
	if blocker := a.serverSettings().SelfUpgradeBlocker(); blocker != "" {
		writeServerUpgradeUnavailable(w, blocker)
		return
	}
	fetched, ok := a.channelIndex(ctx, w)
	if !ok {
		return
	}
	release := fetched.Index.Server
	if target.Version != "" {
		var found bool
		release, found = fetched.Index.FindServer(target.Version)
		if !found {
			writeProductError(w, http.StatusConflict, ErrorUpgradeWithdrawn, "the selected version is no longer published", map[string]any{"version": target.Version})
			return
		}
	}
	if release.ReleaseID == version.Version {
		writeProductError(w, http.StatusConflict, ErrorServerAlreadyAtRelease, "this server already runs "+release.ReleaseID, map[string]any{"release": release.ReleaseID})
		return
	}
	if !release.NewerThan(version.Version, version.BuildDate) {
		writeProductError(w, http.StatusConflict, ErrorServerNewerThanRelease, "this server runs "+version.Version+", which is newer than "+release.ReleaseID,
			map[string]any{"current": version.Version, "release": release.ReleaseID})
		return
	}
	asset, err := release.Asset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		writeNotPublished(w, "server_"+runtime.GOOS+"_"+runtime.GOARCH)
		return
	}
	if target.SHA256 != "" && target.SHA256 != asset.SHA256 {
		writeProductError(w, http.StatusConflict, ErrorUpgradeSelectionChanged, "the selected package changed; check again", nil)
		return
	}
	dir, err := os.MkdirTemp("", "gpuworks-upgrade-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	archivePath := filepath.Join(dir, "release.tar.gz")
	if _, err := a.d.Outside.channel().Download(ctx, fetched, asset.File, archivePath, func(distribution.Progress) {}); err != nil {
		writeDownloadFailed(w, asset.File, err)
		return
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer archive.Close()
	target.Version = release.ReleaseID
	a.upgradeServer(w, r.WithContext(ctx), archive, target)
}

// writeDownloadFailed answers a file the index lists that no source served
// intact. The index itself was fine, so this is not UPDATE_SOURCES_UNAVAILABLE.
func writeDownloadFailed(w http.ResponseWriter, file distribution.File, err error) {
	writeProductError(w, http.StatusBadGateway, ErrorUpdateDownloadFailed, err.Error(), map[string]any{"file": file.Path, "detail": err.Error()})
}

// upgradeServerFromUpload upgrades the server with an archive an
// administrator uploads, for a server that cannot reach the channel.
func (a *apiHandlers) upgradeServerFromUpload(w http.ResponseWriter, r *http.Request) {
	if blocker := a.serverSettings().SelfUpgradeBlocker(); blocker != "" {
		writeServerUpgradeUnavailable(w, blocker)
		return
	}
	if !a.lockServerUpgrade(w) {
		return
	}
	defer a.serverUpgradeMu.Unlock()
	a.upgradeServer(w, r, http.MaxBytesReader(w, r.Body, maxServerArchiveBytes), dto.ServerUpgradeSelection{})
}

func (a *apiHandlers) upgradeServer(w http.ResponseWriter, r *http.Request, archive io.Reader, target dto.ServerUpgradeSelection) {
	if a.d.AgentUpgrades.Service != nil {
		finish, err := a.d.AgentUpgrades.Service.BeginServerUpgrade()
		if err != nil {
			writeAgentUpgradeError(w, err)
			return
		}
		accepted := false
		defer func() { finish(accepted) }()
		a.upgradeServerWithAgentGuard(w, r, archive, target, &accepted)
		return
	}
	a.upgradeServerWithAgentGuard(w, r, archive, target, nil)
}

func (a *apiHandlers) upgradeServerWithAgentGuard(w http.ResponseWriter, r *http.Request, archive io.Reader, target dto.ServerUpgradeSelection, accepted *bool) {
	by := ""
	if claims, ok := a.claimsFromRequest(r); ok {
		by = claims.Username
	}
	started, err := a.serverSettings().Upgrade(r.Context(), archive, by, target)
	if err != nil {
		writeServerSettingsError(w, err)
		return
	}
	if accepted != nil {
		*accepted = true
	}
	if target.Attempt != "" {
		a.setUpgradeProgress(dto.ServerUpgradeProgress{Attempt: target.Attempt, Phase: "restart", From: started.From, To: started.To})
	}
	writeJSON(w, http.StatusAccepted, started)
}

func writeServerUpgradeUnavailable(w http.ResponseWriter, reason string) {
	writeProductError(w, http.StatusConflict, ErrorServerUpgradeUnavailable, "this server cannot upgrade itself ("+reason+"); run gpuworks upgrade on its host", map[string]any{"reason": reason})
}

// putUpdateChannel switches the channel this server follows; it is kept in
// server.yaml, so `gpuworks upgrade` on the host follows it too.
func (a *apiHandlers) putUpdateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorUpdateChannelInvalid, "invalid update channel request", map[string]any{"channel": ""})
		return
	}
	if !a.lockServerUpgrade(w) {
		return
	}
	defer a.serverUpgradeMu.Unlock()
	if err := a.serverSettings().SetUpdateChannel(strings.TrimSpace(req.Channel)); err != nil {
		writeServerSettingsError(w, err)
		return
	}
	channel, _ := a.serverSettings().UpdateChannel()
	writeJSON(w, http.StatusOK, map[string]string{"channel": channel})
}
