package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ---- instances ----

func (a *apiHandlers) listInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := a.d.Store.ListInstances()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	deps, err := a.d.Store.ListDeployments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := deployment.NewListRows(a.d.Store, a.d.Preparation, deps, insts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.instanceDTOs(rows, insts)))
}

func (a *apiHandlers) getInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := a.d.Store.InstanceByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.instanceDTO(inst))
}

// eventPage reads the timeline cursor and page size. `before` is the exclusive
// event id to read backwards from; omitting it starts at the newest entry.
//
// The timeline is newest-first because the answer to "what happened to this
// deployment" is at the end of it. The endpoint used to return the oldest 500
// rows, which on a busy instance meant 500 lines of one download and no outcome
// at all.
func eventPage(r *http.Request) (before uint, limit int, code, detail string) {
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || value == 0 {
			return 0, 0, ErrorEventCursorInvalid, "invalid event cursor"
		}
		before = uint(value)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return 0, 0, ErrorEventLimitInvalid, "invalid event limit"
		}
		limit = value
	}
	return before, limit, "", ""
}

func (a *apiHandlers) instanceEvents(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	before, limit, code, detail := eventPage(r)
	if code != "" {
		writeProductError(w, http.StatusBadRequest, code, detail, nil)
		return
	}
	// The timeline is addressed by UUID, not by loading the instance first. The
	// history outlives the instance row, so requiring the row to still exist
	// would hide exactly the events a deleted deployment is diagnosed from.
	evs, err := a.d.Store.InstanceTimeline(uuid, before, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(evs) == 0 {
		// No events yet is not the same as no such instance. Only an unknown
		// UUID is a 404. A cursor that has run off the end of a real timeline
		// is an empty page, not a missing instance.
		if _, err := a.d.Store.InstanceByUUID(uuid); err != nil && before == 0 {
			writeErr(w, http.StatusNotFound, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, dto.NewCursorPage(instanceEventDTOs(evs)))
}

// deploymentEvents returns a deployment's whole timeline, including entries
// recorded before any instance existed. This is where a deployment that could
// never be scheduled explains itself.
//
// The deployment row is deliberately NOT loaded first: the history outlives the
// deployment, and requiring the row to exist would make the timeline of a
// deleted deployment unreachable - the one case it is most needed.
func (a *apiHandlers) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if id == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorDeploymentIDInvalid, "invalid deployment id", nil)
		return
	}
	before, limit, code, detail := eventPage(r)
	if code != "" {
		writeProductError(w, http.StatusBadRequest, code, detail, nil)
		return
	}
	evs, err := a.d.Store.DeploymentTimeline(id, before, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewCursorPage(instanceEventDTOs(evs)))
}

func instanceEventDTOs(evs []store.InstanceEvent) []dto.InstanceEvent {
	out := make([]dto.InstanceEvent, 0, len(evs))
	for _, e := range evs {
		var params map[string]any
		raw := e.Data
		if e.Code != "" && e.Data != "" {
			_ = json.Unmarshal([]byte(e.Data), &params)
			// A coded row's Data is exactly these params. Sending both doubled
			// the payload for nothing. Rows written before events carried codes
			// have no params, and their Data is the only thing that describes
			// them, so it is still sent.
			raw = ""
		}
		out = append(out, dto.InstanceEvent{
			ID: e.ID, Kind: e.Kind, Code: e.Code, Params: params, Message: e.Message, Data: raw, CreatedAt: utcTime(e.CreatedAt),
		})
	}
	return out
}

// ---- helpers ----

func parseUint(s string) uint {
	v, _ := strconv.ParseUint(s, 10, 64)
	return uint(v)
}

func jsonStr(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// relPath is the node-relative directory for a model's weights. The layout is
// owned by the store so every producer of a ModelFile.Path agrees on it.
func relPath(m *store.Model) string { return store.ModelArtifactPath(m) }
