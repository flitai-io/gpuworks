package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/monitorapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
)

// requestSource is monitorapi.RequestSource over the gateway's in-flight
// table and the request store (ux.db).
type requestSource struct {
	store *store.Store
	live  func(deploymentID uint, limit int) uxrecord.LiveSnapshot
	ux    *uxstore.Store
	now   func() time.Time

	mu       sync.Mutex
	names    map[uint]string
	namesAt  time.Time
	namesTTL time.Duration
}

// keyNamesTTL bounds how stale a renamed key's name may show in the request
// view, which a browser polls every few seconds.
const keyNamesTTL = 10 * time.Second

func newRequestSource(st *store.Store, live func(uint, int) uxrecord.LiveSnapshot, ux *uxstore.Store) *requestSource {
	return &requestSource{store: st, live: live, ux: ux, now: time.Now, namesTTL: keyNamesTTL}
}

func (s *requestSource) DeploymentExists(id uint) bool {
	_, err := s.store.GetDeployment(id)
	return err == nil
}

func (s *requestSource) Live(deploymentID uint, limit int) uxrecord.LiveSnapshot {
	return s.live(deploymentID, limit)
}

func (s *requestSource) History(ctx context.Context, q monitorapi.HistoryQuery) ([]uxrecord.Request, string, error) {
	records, next, err := s.ux.Requests(ctx, uxstore.RequestQuery{
		DeploymentID: q.DeploymentID, FromMS: q.FromMS, ToMS: q.ToMS,
		APIKeyID: q.APIKeyID, InstanceUUID: q.InstanceUUID, PriorityClass: q.PriorityClass, Results: q.Results,
		MinEndMS: q.MinEndMS, MaxEndMS: q.MaxEndMS, MinTTFTMS: q.MinTTFTMS, MaxTTFTMS: q.MaxTTFTMS, Limit: q.Limit, Cursor: q.Cursor,
	})
	if errors.Is(err, uxstore.ErrBadQuery) {
		return nil, "", fmt.Errorf("%w: %w", monitorapi.ErrBadHistoryQuery, err)
	}
	return records, next, err
}

func (s *requestSource) Grid(ctx context.Context, deploymentID uint, kind string, fromMS, toMS int64) ([]uxrecord.Cell, error) {
	return s.ux.Grid(ctx, uxstore.GridQuery{DeploymentID: deploymentID, Kind: kind, FromMS: fromMS, ToMS: toMS})
}

func (s *requestSource) Coverage(ctx context.Context, deploymentID uint, fromMS, toMS int64) (int64, int64, int64, error) {
	c, err := s.ux.Coverage(ctx, deploymentID, fromMS, toMS)
	return c.Total, c.WithUsage, c.WithCache, err
}

func (s *requestSource) Latency(ctx context.Context, deploymentID uint, fromMS, toMS int64) (uxrecord.LatencySums, error) {
	return s.ux.Latency(ctx, deploymentID, fromMS, toMS)
}

func (s *requestSource) Lookup(ctx context.Context, q monitorapi.LookupQuery) ([]uxrecord.Request, error) {
	records, err := s.ux.Lookup(ctx, uxstore.LookupQuery{TraceID: q.TraceID, RequestID: q.RequestID, FromMS: q.FromMS, ToMS: q.ToMS, Limit: q.Limit})
	if errors.Is(err, uxstore.ErrBadQuery) {
		return nil, fmt.Errorf("%w: %w", monitorapi.ErrBadHistoryQuery, err)
	}
	return records, err
}

// DeploymentNames names the deployments of a lookup's requests (a handful)
// by their served name, in one read: a deleted one by the name it left. A
// failed read names none, as for deployments deleted before names were
// kept.
func (s *requestSource) DeploymentNames(ids []uint) map[uint]monitorapi.DeploymentName {
	out := make(map[uint]monitorapi.DeploymentName, len(ids))
	names, err := s.store.DeploymentNames(ids)
	if err != nil {
		return out
	}
	for id, n := range names {
		name := n.ServedName
		if name == "" {
			name = n.Name
		}
		out[id] = monitorapi.DeploymentName{Name: name, Deleted: n.Deleted}
	}
	return out
}

// KeyNames names API keys by id, from a list refreshed at most every
// namesTTL. A failed refresh keeps the previous names.
func (s *requestSource) KeyNames() map[uint]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names != nil && s.now().Sub(s.namesAt) < s.namesTTL {
		return s.names
	}
	keys, err := s.store.ListAPIKeys()
	if err != nil {
		if s.names == nil {
			return map[uint]string{}
		}
		return s.names
	}
	names := make(map[uint]string, len(keys))
	for _, key := range keys {
		names[key.ID] = key.Name
	}
	s.names, s.namesAt = names, s.now()
	return names
}

// closeRequestSinks flushes and closes ux.db and the trace exporter once the
// gateway has stopped.
func (a *App) closeRequestSinks() {
	if a.telemetry != nil {
		a.telemetry.exporter.Close()
	}
	a.ops.close()
	if a.obs == nil || a.obs.UX() == nil {
		return
	}
	if err := a.obs.UX().Close(); err != nil {
		a.log.Warn("request store close failed", "err", err)
	}
}
