package api

import (
	"cmp"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// modelListSources are the model sources the list can be filtered by.
var modelListSources = []string{"huggingface", "modelscope", "url", "local"}

// modelListSorters are the orders GET /models offers, by the model list's
// column keys.
var modelListSorters = listSorters[dto.ModelListItem]{
	"name":      func(a, b dto.ModelListItem) int { return compareText(modelListName(a.Model), modelListName(b.Model)) },
	"source":    func(a, b dto.ModelListItem) int { return cmp.Compare(a.Source, b.Source) },
	"source_id": func(a, b dto.ModelListItem) int { return compareText(a.SourceID, b.SourceID) },
	"published_at": func(a, b dto.ModelListItem) int {
		return compareTime(modelPublishedAt(a.Model), modelPublishedAt(b.Model))
	},
	"created_at": func(a, b dto.ModelListItem) int { return a.CreatedAt.Compare(b.CreatedAt) },
	"weight":     func(a, b dto.ModelListItem) int { return cmp.Compare(a.WeightGiB, b.WeightGiB) },
	"presence":   func(a, b dto.ModelListItem) int { return cmp.Compare(a.Presence.ReadyNodes, b.Presence.ReadyNodes) },
}

func modelListName(m dto.Model) string {
	if m.Name != "" {
		return m.Name
	}
	return m.SourceID
}

// modelPublishedAt is when the model was published at its source: the
// repository's creation, else its last update.
func modelPublishedAt(m dto.Model) *time.Time {
	if m.OriginCreatedAt != nil {
		return m.OriginCreatedAt
	}
	return m.OriginUpdatedAt
}

// compareTime orders an absent time before every present one.
func compareTime(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(*b)
}

// modelPresenceSummary sums a model's file locations the way the list cell
// shows them: ready node copies (caches apart), ready shared copies,
// transfers in flight with their mean progress, and locations in trouble.
func modelPresenceSummary(m dto.Model) dto.ModelPresenceSummary {
	var s dto.ModelPresenceSummary
	var progress float64
	count := func(state string, p float64) {
		switch state {
		case proto.MFDownloading, proto.MFSyncing, proto.MFPending:
			s.InFlight++
			progress += p
		case proto.MFFailed, proto.MFDownloadRequired, proto.MFMissing, proto.MFInsufficientDisk:
			s.Failed++
		}
	}
	for _, n := range m.Nodes {
		if proto.ModelFileStateReady(n.State) {
			if n.Cache {
				s.CachedNodes++
			} else {
				s.ReadyNodes++
			}
		}
		count(n.State, n.Progress)
	}
	for _, st := range m.Storages {
		if proto.ModelFileStateReady(st.State) {
			s.ReadyStorages++
		}
		count(st.State, st.Progress)
	}
	if s.InFlight > 0 {
		s.InFlightPercent = int(progress/float64(s.InFlight)*100 + 0.5)
	}
	return s
}

func modelHasOperation(m dto.Model, types map[string]bool) bool {
	if len(types) == 0 {
		return true
	}
	for _, op := range m.Operations {
		if types[string(op.Type)] {
			return true
		}
	}
	return false
}

// modelListFilter is GET /models's column filters; each lists alternatives
// and an empty one matches every model. sourceIDs are lowercased: a source id
// matches exactly, ignoring case, as the deploy page looks up the models that
// serve a template's weight source.
type modelListFilter struct {
	sources, operations, sourceIDs map[string]bool
}

func (f modelListFilter) matches(m dto.Model) bool {
	return (len(f.sources) == 0 || f.sources[m.Source]) &&
		modelHasOperation(m, f.operations) &&
		(len(f.sourceIDs) == 0 || f.sourceIDs[strings.ToLower(m.SourceID)])
}

// modelPage answers a paged GET /models: q (a substring of the name, source
// id or revision), source, operation and source_id (comma separated
// alternatives) and
// sort cut every model before the page is taken. A row carries the presence
// summary instead of the per-location list and no README; GET /models/{id}
// has both. The summary always covers every model.
func modelPage(models []dto.Model, query listQuery, filter modelListFilter, order listSort) dto.ModelPage {
	matched := make([]dto.ModelListItem, 0, len(models))
	summary := dto.ModelListSummary{Registered: len(models)}
	seenSource := map[string]bool{}
	for _, m := range models {
		presence := modelPresenceSummary(m)
		summary.ReadyFiles += presence.ReadyNodes + presence.ReadyStorages
		summary.WeightGiB += m.WeightGiB
		if m.Source != "" && !seenSource[m.Source] {
			seenSource[m.Source] = true
			summary.Sources++
		}
		if !query.matchesSearch(m.Name, m.SourceID, m.Revision) || !filter.matches(m) {
			continue
		}
		row := m
		row.README, row.Nodes, row.Storages = "", nil, nil
		matched = append(matched, dto.ModelListItem{Model: row, Presence: presence})
	}
	modelListSorters.apply(matched, order)
	page := pageOf(matched, query, func(rows []dto.ModelListItem) []dto.ModelListItem { return append([]dto.ModelListItem(nil), rows...) })
	return dto.ModelPage{ListPage: page, Summary: summary}
}

// modelsMatching is every model the query's filters match, in the requested
// order, in full: GET /models without page parameters.
func modelsMatching(models []dto.Model, query listQuery, filter modelListFilter, order listSort) []dto.Model {
	rows := make([]dto.ModelListItem, 0, len(models))
	full := make(map[uint]dto.Model, len(models))
	for _, m := range models {
		if !query.matchesSearch(m.Name, m.SourceID, m.Revision) || !filter.matches(m) {
			continue
		}
		full[m.ID] = m
		rows = append(rows, dto.ModelListItem{Model: m, Presence: modelPresenceSummary(m)})
	}
	modelListSorters.apply(rows, order)
	out := make([]dto.Model, 0, len(rows))
	for _, row := range rows {
		out = append(out, full[row.ID])
	}
	return out
}

func parseModelListQuery(r *http.Request) (listQuery, modelListFilter, listSort, error) {
	var filter modelListFilter
	query, err := parseListQuery(r, nil)
	if err != nil {
		return query, filter, listSort{}, err
	}
	if filter.sources, err = listValues(r, "source", modelListSources); err != nil {
		return query, filter, listSort{}, err
	}
	if filter.operations, err = listValues(r, "operation", nil); err != nil {
		return query, filter, listSort{}, err
	}
	operations := make(map[string]bool, len(filter.operations))
	for op := range filter.operations {
		definition, known := inferenceapi.Lookup(op)
		if !known {
			return query, filter, listSort{}, listFilterError("operation", op)
		}
		operations[string(definition.Type)] = true
	}
	filter.operations = operations
	ids, err := listValues(r, "source_id", nil)
	if err != nil {
		return query, filter, listSort{}, err
	}
	if len(ids) > 0 {
		filter.sourceIDs = map[string]bool{}
		for id := range ids {
			filter.sourceIDs[strings.ToLower(id)] = true
		}
	}
	order, err := modelListSorters.parse(r)
	return query, filter, order, err
}
