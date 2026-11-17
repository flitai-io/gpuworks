package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Only the public catalog, README and current revision are readable through this
// endpoint. The control plane chooses the host and transport for every request.
var catalogRepo = `[a-zA-Z0-9_-][a-zA-Z0-9_.-]*`
var catalogREADME = regexp.MustCompile(`^/(` + catalogRepo + `/)?` + catalogRepo + `/resolve/` + catalogRepo + `/README\.md$`)
var catalogCommits = regexp.MustCompile(`^/api/v1/models/` + catalogRepo + `/` + catalogRepo + `/commits$`)

func catalogResource(source, path string) bool {
	switch source {
	case "modelscope":
		return path == "/openapi/v1/models" || catalogCommits.MatchString(path) || (strings.HasPrefix(path, "/models/") && catalogREADME.MatchString(strings.TrimPrefix(path, "/models")))
	case "huggingface":
		return path == "/api/models" || catalogREADME.MatchString(path)
	}
	return false
}

func (a *apiHandlers) modelCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source, path := q.Get("source"), q.Get("path")
	if !catalogResource(source, path) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "invalid catalog resource", map[string]any{"field": "path", "detail": "invalid catalog resource"})
		return
	}
	params := url.Values{}
	for _, key := range []string{"search", "filter", "sort", "filter.task", "filter.custom_tag", "page_number", "cursor", "pipeline_tag"} {
		if v := q.Get(key); v != "" {
			params.Set(key, v)
		}
	}
	switch {
	case path == "/api/models":
		params.Set("full", "true")
		params.Set("limit", "10")
	case path == "/openapi/v1/models":
		params.Set("page_size", "10")
	case catalogCommits.MatchString(path):
		params = url.Values{"Revision": {"master"}, "PageSize": {"1"}}
	default:
		params = url.Values{}
	}
	bases, err := planning.ServerFetchCandidates(a.d.Store, source, "")
	if err != nil {
		if containsProductCode(err, ErrorModelMetadataForbidden) {
			writeErr(w, http.StatusForbidden, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	client := &http.Client{Transport: a.outboundTransport()}
	defer client.CloseIdleConnections()
	status := http.StatusBadGateway
	for _, base := range bases {
		target := strings.TrimSuffix(base, "/") + path
		if len(params) > 0 {
			target += "?" + params.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 600 {
				status = resp.StatusCode
			}
			continue
		}
		const limit = 4 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil || len(body) > limit {
			continue
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if link := resp.Header.Get("Link"); link != "" {
			w.Header().Set("X-Catalog-Link", link)
		}
		_, _ = w.Write(body)
		return
	}
	w.WriteHeader(status)
}

func templatesWithWeight(models []store.InferenceModel, provider, repo, revision string) []store.InferenceModel {
	if repo == "" {
		return models
	}
	if revision == "" {
		return nil
	}
	var found []store.InferenceModel
	for _, model := range models {
		specs := model.Specs
		model.Specs = nil
		for _, spec := range specs {
			weights, _ := inferencecatalog.DecodeWeightSources(spec.WeightSources)
			for _, w := range weights {
				if w.Provider == provider && w.RepoID == repo && w.Revision == revision && w.Subfolder == "" {
					model.Specs = append(model.Specs, spec)
					break
				}
			}
		}
		if len(model.Specs) > 0 {
			found = append(found, model)
		}
	}
	return found
}
