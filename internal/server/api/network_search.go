package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// searchNetworkTargets uses the same effective nodes and attachment fields as
// the drawer. The latest test (potentially megabytes) is irrelevant to search.
func (a *apiHandlers) searchNetworkTargets(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	kind := params.Get("kind")
	if kind != "nodes" && kind != "attachments" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "kind must be nodes or attachments", map[string]any{"field": "kind", "detail": "expected nodes or attachments"})
		return
	}
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rawID := chi.URLParam(r, "id")
	var detail dto.NetworkDetail
	if isManagementNetworkID(rawID) {
		detail = st.managementDetail(nil)
	} else {
		id := parseUint(rawID)
		for _, n := range st.Networks {
			if n.ID == id {
				detail = st.networkDetail(n, nil)
				break
			}
		}
		if detail.ID == 0 {
			writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, matchNetworkTargets(detail, kind, params.Get("q"), params["role"], params["source"]))
}

func matchNetworkTargets(detail dto.NetworkDetail, kind, query string, roles, sources []string) dto.NetworkTargetMatches {
	q := strings.ToLower(strings.TrimSpace(query))
	matches := func(fields ...string) bool {
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), q) {
				return true
			}
		}
		return false
	}
	out := dto.NetworkTargetMatches{IDs: []string{}}
	// An explicitly configured interface, IP or HCA can differ from what the
	// node reported. Both belong to the same searchable overview row.
	directMatches := make(map[uint]bool)
	for _, a := range detail.Attachments {
		labelMatch := a.Target == netplan.SourceLabel && matches(a.LabelKey+"="+a.LabelValue)
		if labelMatch || matches(a.NodeName, a.NodeUUID, a.Interface, a.IP) || matches(a.HCAs...) {
			if kind == "attachments" {
				out.IDs = append(out.IDs, strconv.FormatUint(uint64(a.ID), 10))
			} else if a.Target == netplan.SourceNode {
				directMatches[a.ID] = true
			}
		}
	}
	if kind == "attachments" {
		return out
	}
	for _, n := range detail.Nodes {
		if len(sources) > 0 && !slices.Contains(sources, n.Source) {
			continue
		}
		if len(roles) > 0 && !slices.ContainsFunc(n.Roles, func(role string) bool { return slices.Contains(roles, role) }) {
			continue
		}
		hit := directMatches[n.AttachmentID] || matches(n.NodeName, n.NodeUUID, n.IP, n.Interface) ||
			(n.LabelKey != "" && matches(n.LabelKey+"="+n.LabelValue)) ||
			slices.ContainsFunc(n.HCAs, func(rail dto.NetworkRail) bool {
				return matches(rail.Name, rail.IP, rail.NetDev)
			})
		if !hit {
			for key, value := range n.Labels {
				if matches(key + "=" + value) {
					hit = true
					break
				}
			}
		}
		if hit {
			out.IDs = append(out.IDs, n.NodeUUID)
		}
	}
	return out
}
