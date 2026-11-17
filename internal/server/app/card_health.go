package app

import (
	"context"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cardhealth"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// cardHealth is each card's checks in memory, their failure records and the
// agent reads behind them, as the API uses them (api.CardHealth).
type cardHealth struct {
	*cardhealth.Tracker
	details *cardhealth.Details
	log     *slog.Logger
}

const (
	cardHealthFlushEvery = 5 * time.Second
	cardHealthPruneEvery = 24 * time.Hour
)

// newCardHealth loads the open failure records and has the hub hand it every
// status report's cards.
func newCardHealth(log *slog.Logger, st *store.Store, h *hub.Hub) (*cardHealth, error) {
	tracker := cardhealth.New(log, episodeStore{st})
	if err := tracker.Load(); err != nil {
		return nil, err
	}
	h.SetCardCheckSinks(tracker.Observe, tracker.SetOffline)
	return &cardHealth{Tracker: tracker, details: cardhealth.NewDetails(h.CallNode), log: log}, nil
}

func (c *cardHealth) Detail(ctx context.Context, nodeUUID, ident, check string) (proto.CardCheckDetail, error) {
	return c.details.Detail(ctx, nodeUUID, ident, check)
}

// run writes the failure records as they fall due, prunes the old ones once a
// day, and writes everything still pending when ctx ends.
func (c *cardHealth) run(ctx context.Context) {
	flush := time.NewTicker(cardHealthFlushEvery)
	defer flush.Stop()
	prune := time.NewTicker(cardHealthPruneEvery)
	defer prune.Stop()
	c.prune()
	for {
		select {
		case <-ctx.Done():
			if err := c.Flush(true); err != nil {
				c.log.Warn("card health records not written at shutdown", "err", err)
			}
			return
		case <-flush.C:
			if err := c.Flush(false); err != nil {
				c.log.Warn("card health records not written; retried", "err", err)
			}
		case <-prune.C:
			c.prune()
		}
	}
}

func (c *cardHealth) prune() {
	if n, err := c.Prune(); err != nil {
		c.log.Warn("old card health records not deleted", "err", err)
	} else if n > 0 {
		c.log.Info("old card health records deleted", "records", n)
	}
}

// episodeStore keeps the tracker's episodes as store rows.
type episodeStore struct{ st *store.Store }

func (e episodeStore) OpenEpisodes() ([]cardhealth.Episode, error) {
	rows, err := e.st.OpenCardHealthEpisodes()
	out := make([]cardhealth.Episode, len(rows))
	for i, r := range rows {
		out[i] = fromEpisodeRow(r)
	}
	return out, err
}

func (e episodeStore) SaveEpisodes(eps []*cardhealth.Episode) error {
	rows := make([]*store.CardHealthEpisode, len(eps))
	for i, ep := range eps {
		row := toEpisodeRow(*ep)
		rows[i] = &row
	}
	if err := e.st.SaveCardHealthEpisodes(rows); err != nil {
		return err
	}
	for i, row := range rows {
		eps[i].ID = row.ID
	}
	return nil
}

func (e episodeStore) DeleteEpisodesEndedBefore(before time.Time, limit int) (int, error) {
	return e.st.DeleteCardHealthEpisodesEndedBefore(before, limit)
}

func toEpisodeRow(ep cardhealth.Episode) store.CardHealthEpisode {
	return store.CardHealthEpisode{
		ID: ep.ID, NodeID: ep.NodeID, CardIdent: ep.CardIdent, CheckID: ep.CheckID, Level: ep.Status, Reason: ep.Code,
		StartedAt: ep.StartedAt, EndedAt: ep.EndedAt, LastSeenAt: ep.LastSeenAt, Count: ep.Count, Flapping: ep.Flapping,
		FirstEvidence: ep.FirstEvidence, LatestEvidence: ep.LatestEvidence,
	}
}

func fromEpisodeRow(r store.CardHealthEpisode) cardhealth.Episode {
	return cardhealth.Episode{
		ID: r.ID, NodeID: r.NodeID, CardIdent: r.CardIdent, CheckID: r.CheckID, Status: r.Level, Code: r.Reason,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, LastSeenAt: r.LastSeenAt, Count: r.Count, Flapping: r.Flapping,
		FirstEvidence: r.FirstEvidence, LatestEvidence: r.LatestEvidence,
	}
}
