package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cardhealth"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CardHealth is each card's checks as the server holds them in memory, their
// failure records, and the two reads that reach an agent (cardhealth).
type CardHealth interface {
	Node(nodeUUID string) (cardhealth.NodeSnapshot, bool)
	LiveEpisodes(nodeID uint) []cardhealth.Episode
	Detail(ctx context.Context, nodeUUID, ident, check string) (proto.CardCheckDetail, error)
	ForgetNode(nodeUUID string, nodeID uint)
}

// ErrCardHealthUnavailable answers the card health routes of a server built
// without the tracker (Deps.CardHealth nil).
var ErrCardHealthUnavailable = errors.New("card health is not available")

// cardFromQuery is the card a request names by pcie_bdf or uuid, or "" for
// every card of the node.
func cardFromQuery(r *http.Request) string {
	q := r.URL.Query()
	return accelerator.CardIdent(q.Get("uuid"), q.Get("pcie_bdf"))
}

// nodeHealthChecks answers GET /nodes/{uuid}/health-checks: each card's
// checks as the last report gave them, laid out as a table. Read from memory.
func (a *apiHandlers) nodeHealthChecks(w http.ResponseWriter, r *http.Request) {
	if a.d.CardHealth == nil {
		writeErr(w, http.StatusNotFound, ErrCardHealthUnavailable)
		return
	}
	node, err := a.d.Store.NodeWithGPUs(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	snap, ok := a.d.CardHealth.Node(node.UUID)
	if !ok {
		snap = cardhealth.NodeSnapshot{NodeID: node.ID}
	}
	if a.d.Online != nil {
		// The session says whether the node is there; the snapshot only
		// whether it has reported since.
		snap.Online = a.d.Online(node.UUID)
	}
	if card := cardFromQuery(r); card != "" {
		snap.Cards = slices.DeleteFunc(snap.Cards, func(c cardhealth.CardSnapshot) bool { return c.Ident != card })
	}
	view := cardhealth.BuildView(snap, time.Now().UTC())
	canRecheck := snap.Online && a.nodeHasCap(node.UUID, proto.CapCardChecks) && slices.Contains(view.Checks, accelerator.CheckRuntime)
	out := dto.CardHealthChecks{
		NodeUUID: node.UUID, Online: snap.Online, Reports: snap.Reports, Stale: view.Stale,
		Checks: []dto.CardHealthColumn{}, Cards: []dto.CardHealthCard{},
		Summary: map[string]int{},
		Notice:  cardHealthNotice(node.GPUs, cardFromQuery(r), snap, view, a.nodeHasCap(node.UUID, proto.CapCardChecks)),
	}
	if !snap.ReceivedAt.IsZero() {
		at := snap.ReceivedAt.UTC()
		out.ReceivedAt = &at
	}
	for _, id := range view.Checks {
		spec, _ := accelerator.CardCheckFor(id)
		out.Checks = append(out.Checks, dto.CardHealthColumn{ID: id, PeriodSeconds: int(spec.Period / time.Second)})
	}
	for _, card := range view.Cards {
		dc := dto.CardHealthCard{
			Ident: card.Ident, UUID: card.UUID, PCIeBDF: card.PCIeBDF, Name: card.Name, Index: card.Index,
			Worst: cardhealth.Worst(card.Cells), CanRecheck: canRecheck, Cells: make([]dto.CardHealthCell, 0, len(card.Cells)),
		}
		for _, cell := range card.Cells {
			dcell := dto.CardHealthCell{Check: cell.Check, Status: cell.Status, Code: cell.Code, DurationMS: cell.DurationMS, Stale: cell.Stale}
			if !cell.CheckedAt.IsZero() {
				at := cell.CheckedAt
				dcell.CheckedAt = &at
			}
			dc.Cells = append(dc.Cells, dcell)
			out.Summary[cell.Status]++
		}
		out.Cards = append(out.Cards, dc)
	}
	writeJSON(w, http.StatusOK, out)
}

// nodeHealthRecords answers GET /nodes/{uuid}/health-records: the stretches
// in which a check warned or failed, open ones first, then the newest; one
// card's with pcie_bdf or uuid, one check's with check.
func (a *apiHandlers) nodeHealthRecords(w http.ResponseWriter, r *http.Request) {
	if a.d.CardHealth == nil {
		writeErr(w, http.StatusNotFound, ErrCardHealthUnavailable)
		return
	}
	node, err := a.d.Store.NodeWithGPUs(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	q, err := parseListQuery(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	check := r.URL.Query().Get("check")
	if _, ok := accelerator.CardCheckFor(check); check != "" && !ok {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "check is not a card check", map[string]any{"field": "check", "detail": check})
		return
	}
	query := store.CardHealthEpisodeQuery{
		NodeID: node.ID, CardIdent: cardFromQuery(r), CheckID: check,
		Offset: q.offset(), Limit: q.pageSize,
		StartedAfter: time.Now().UTC().Add(-cardhealth.Retention),
	}
	rows, total, err := a.d.Store.CardHealthEpisodes(query)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// What memory holds is newer than what was last written.
	live := map[uint]cardhealth.Episode{}
	for _, ep := range a.d.CardHealth.LiveEpisodes(node.ID) {
		if ep.ID != 0 {
			live[ep.ID] = ep
		}
	}
	cards := map[string]store.GPU{}
	for _, g := range node.GPUs {
		cards[g.Ident] = g
	}
	now := time.Now().UTC()
	out := dto.CardHealthRecords{Records: make([]dto.CardHealthRecord, 0, len(rows)), Total: total, Page: q.page, PageSize: q.pageSize,
		RetentionDays: int(cardhealth.Retention / (24 * time.Hour))}
	for _, row := range rows {
		if l, ok := live[row.ID]; ok {
			row.Level, row.Reason, row.EndedAt, row.LastSeenAt = l.Status, l.Code, l.EndedAt, l.LastSeenAt
			row.Count, row.Flapping, row.LatestEvidence = l.Count, l.Flapping, l.LatestEvidence
		}
		out.Records = append(out.Records, toCardHealthRecord(row, cards, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func toCardHealthRecord(row store.CardHealthEpisode, cards map[string]store.GPU, now time.Time) dto.CardHealthRecord {
	rec := dto.CardHealthRecord{
		ID: row.ID, CardIdent: row.CardIdent, Check: row.CheckID, Status: row.Level, Code: row.Reason,
		StartedAt: row.StartedAt.UTC(), LastSeenAt: row.LastSeenAt.UTC(), Count: row.Count, Flapping: row.Flapping,
		Ongoing:        row.EndedAt == nil,
		FirstEvidence:  healthFields(row.FirstEvidence),
		LatestEvidence: healthFields(row.LatestEvidence),
	}
	end := now
	if row.EndedAt != nil {
		e := row.EndedAt.UTC()
		rec.EndedAt, end = &e, e
	}
	rec.DurationSeconds = int64(end.Sub(row.StartedAt) / time.Second)
	if g, ok := cards[row.CardIdent]; ok {
		index := g.Index
		rec.CardIndex, rec.CardName, rec.PCIeBDF = &index, g.Name, g.PCIeBDF
	} else {
		rec.Removed = true
	}
	return rec
}

func healthFields(text string) []dto.GPUHealthField {
	fields := accelerator.ParseHealthEvidence(text)
	out := make([]dto.GPUHealthField, 0, len(fields))
	for _, f := range fields {
		out = append(out, dto.GPUHealthField{Name: f.Name, Value: f.Value})
	}
	return out
}

// nodeHealthCheckDetail answers GET /nodes/{uuid}/health-checks/detail: the
// last run of one check on one card, asked of the agent, which answers from
// memory and runs nothing.
func (a *apiHandlers) nodeHealthCheckDetail(w http.ResponseWriter, r *http.Request) {
	if a.d.CardHealth == nil {
		writeErr(w, http.StatusNotFound, ErrCardHealthUnavailable)
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	card, check := cardFromQuery(r), r.URL.Query().Get("check")
	if card == "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "pcie_bdf or uuid names the card", map[string]any{"field": "pcie_bdf"})
		return
	}
	if _, ok := accelerator.CardCheckFor(check); !ok {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "check is not a card check", map[string]any{"field": "check", "detail": check})
		return
	}
	if !a.cardChecksReachable(w, node.UUID) {
		return
	}
	detail, err := a.d.CardHealth.Detail(r.Context(), node.UUID, card, check)
	proto.NormalizeCardCheckDetail(&detail)
	switch {
	case errors.Is(err, cardhealth.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, toCardCheckDetail(detail))
}

// recheckCard answers POST /nodes/{uuid}/gpus/recheck: the runtime check on
// one card, now. The agent records the answer like the periodic check's own,
// so the card's state follows at the next report.
func (a *apiHandlers) recheckCard(w http.ResponseWriter, r *http.Request) {
	var req dto.CardRecheckRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ident := accelerator.CardIdent(req.UUID, req.PCIeBDF)
	if ident == "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "pcie_bdf or uuid names the card", map[string]any{"field": "pcie_bdf"})
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	if !a.cardChecksReachable(w, node.UUID) {
		return
	}
	ctx, cancel := rpccontext.WithTimeout(r.Context(), nodeDoctorTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCNodeCardRecheck, proto.CardRecheckRequest{Ident: ident})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var detail proto.CardCheckDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	proto.NormalizeCardCheckDetail(&detail)
	writeJSON(w, http.StatusOK, toCardCheckDetail(detail))
}

// cardChecksReachable refuses, with the reason, a node whose agent cannot be
// asked about its checks: offline, or too old to have them.
func (a *apiHandlers) cardChecksReachable(w http.ResponseWriter, nodeUUID string) bool {
	if a.d.Online != nil && !a.d.Online(nodeUUID) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "the node is offline", nil)
		return false
	}
	if !a.nodeHasCap(nodeUUID, proto.CapCardChecks) {
		writeProductError(w, http.StatusConflict, ErrorCardChecksUnsupported, "the node's agent predates card checks; upgrade it", nil)
		return false
	}
	return true
}

func (a *apiHandlers) nodeHasCap(nodeUUID, capability string) bool {
	return a.d.NodeCaps != nil && slices.Contains(a.d.NodeCaps(nodeUUID), capability)
}

func toCardCheckDetail(d proto.CardCheckDetail) dto.CardCheckDetail {
	out := dto.CardCheckDetail{Ident: d.Ident, Check: d.Check, Last: toCardCheckRun(d.Last), LastRan: toCardCheckRun(d.LastRan), LastProblem: toCardCheckRun(d.LastProblem)}
	if at, err := time.Parse(time.RFC3339, d.AgentStartedAt); err == nil {
		out.AgentStartedAt = &at
	}
	return out
}

func toCardCheckRun(r *proto.CardCheckRun) *dto.CardCheckRun {
	if r == nil {
		return nil
	}
	out := &dto.CardCheckRun{Status: r.Status, Code: r.Code, DurationMS: r.DurationMS, Probe: r.Probe, Evidence: []dto.GPUHealthField{}}
	if at, err := time.Parse(time.RFC3339, r.At); err == nil {
		out.At = at.UTC()
	}
	for _, f := range r.Evidence {
		out.Evidence = append(out.Evidence, dto.GPUHealthField{Name: f.Name, Value: f.Value})
	}
	return out
}
