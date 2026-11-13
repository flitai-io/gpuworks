package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// configuredCommunityRepos reads the administrator's community repositories,
// which overlay the built-in ones.
func (a *apiHandlers) configuredCommunityRepos() ([]community.Repo, error) {
	raw := strings.TrimSpace(a.d.Store.GetSetting(store.SettingImageCommunityCatalog))
	if raw == "" {
		return nil, nil
	}
	var catalog community.Catalog
	if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
		return nil, err
	}
	return catalog.Repos, nil
}

// listImageEngines is the engine dictionary of the image form: every deploy
// engine plus the catalog-only vllm-omni family, with display names.
func (a *apiHandlers) listImageEngines(w http.ResponseWriter, _ *http.Request) {
	options := imagecatalog.EngineOptions()
	out := make([]dto.ImageEngineOption, 0, len(options))
	for _, option := range options {
		out = append(out, dto.ImageEngineOption{Name: option.Name, Label: option.Label})
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// listCommunityRepos lists the repositories the community tab offers as
// cards, without their tags.
func (a *apiHandlers) listCommunityRepos(w http.ResponseWriter, _ *http.Request) {
	configured, err := a.configuredCommunityRepos()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	repos, err := community.Repos(configured)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.CommunityRepo, 0, len(repos))
	for _, row := range repos {
		out = append(out, dto.CommunityRepo{Repo: row.Repo, Registry: row.Registry, AccessHost: row.AccessHost(), Engine: row.Engine, Framework: row.Framework, Siblings: row.Siblings})
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// browseCommunityImages lists every upstream tag of one repository: a card,
// a sibling on a card, or any repository the user typed.
func (a *apiHandlers) browseCommunityImages(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	configured, err := a.configuredCommunityRepos()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	row, err := a.d.Outside.community().Browse(r.Context(), repo, configured)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, community.ErrRepositoryNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	out := dto.CommunityRepo{Repo: row.Repo, Registry: row.Registry, AccessHost: row.AccessHost(), Engine: row.Engine, Framework: row.Framework, Siblings: row.Siblings, Tags: make([]dto.CommunityTag, 0, len(row.Tags))}
	for _, tag := range row.Tags {
		out.Tags = append(out.Tags, dto.CommunityTag{
			Tag: tag.Tag, Engine: tag.Engine, EngineVersion: tag.EngineVersion,
			Framework: tag.Framework, FrameworkVersion: tag.FrameworkVersion, Arch: tag.Arch,
			RollingTag: tag.RollingTag, UpstreamGone: tag.UpstreamGone, LastUpdated: tag.LastUpdated,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
